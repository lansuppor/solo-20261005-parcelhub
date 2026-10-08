package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func mustReceiptReturn(t *testing.T, s *Store, request, receipt, reason string, now time.Time) *ReceiptReturnResult {
	t.Helper()
	res, replayed, err := s.ReceiptReturn(request, receipt, reason, now)
	if err != nil || replayed {
		t.Fatalf("ReceiptReturn(%q) 意外失败: %v replayed=%v", request, err, replayed)
	}
	return res
}

// 准备一个在站点A 出发、张三配送的批次 B1（P001、P002），两件均已签收。
func setupReceiptReturnBase(t *testing.T, s *Store) {
	t.Helper()
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustReceiptOK(t, s, "RC2", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 10, 5))
}

func TestReceiptReturnSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	setupReceiptReturnBase(t, s)
	t1 := tClock(2026, 10, 5, 11, 0)

	res := mustReceiptReturn(t, s, "PR1", "RC1", "收件人拒收", t1)
	if res.Request != "PR1" || res.Receipt != "RC1" || res.Batch != "B1" || res.Parcel != "P001" ||
		res.Station != "站点A" || res.Reason != "收件人拒收" || !res.Time.Equal(t1) {
		t.Fatalf("退件结果不符: %+v", res)
	}

	// 成功仅将该件恢复原出发站在站，追加一条退件轨迹。
	p, _ := s.Query("P001")
	if p.Status != statusInStation || p.Station != "站点A" {
		t.Fatalf("退件后应恢复原出发站在站: %+v", p)
	}
	if len(p.Trail) != 4 {
		t.Fatalf("退件后应有四条轨迹，得到 %d", len(p.Trail))
	}
	e := p.Trail[3]
	if e.Op != "退件" || e.Station != "站点A" || e.Batch != "B1" ||
		e.Request != "PR1" || e.RefRequest != "RC1" || e.Reason != "收件人拒收" || !e.Time.Equal(t1) {
		t.Fatalf("退件轨迹不符: %+v", e)
	}
	// 原签收轨迹永久保留。
	if p.Trail[2].Op != "回执" || p.Trail[2].Request != "RC1" || p.Trail[2].Result != resultSigned {
		t.Fatalf("原签收轨迹必须保留: %+v", p.Trail[2])
	}

	// 原签收事实仍计有效回执：批次状态与进度不因退件改变（仍为已完成 2/2）。
	b, _ := s.BatchQuery("B1")
	if !b.Done() || b.Effective() != 2 {
		t.Fatalf("退件不改变批次状态与进度: %+v", b)
	}
	if b.Receipts["P001"].Request != "RC1" || b.Receipts["P001"].ReturnedBy != "PR1" {
		t.Fatalf("批次内回执应保留并标记退件: %+v", b.Receipts["P001"])
	}
	if b.Receipts["P002"].ReturnedBy != "" {
		t.Fatalf("其他成员的回执不得改变: %+v", b.Receipts["P002"])
	}
	// 不列为待配送或已撤销回执。
	for _, pid := range b.Parcels {
		if en := b.Receipts[pid]; en != nil && en.RevokedBy != "" {
			t.Fatalf("退件不得把任何回执标为已撤销: %+v", en)
		}
	}
	rc := s.ReceiptOf("RC1")
	if rc == nil || rc.RevokedBy != "" || rc.ReturnedBy != "PR1" {
		t.Fatalf("原签收仍为有效回执并标记退件: %+v", rc)
	}
	// 其他成员状态不变。
	p2, _ := s.Query("P002")
	if p2.Status != statusSigned || len(p2.Trail) != 3 {
		t.Fatalf("其他成员状态与轨迹不得改变: %+v", p2)
	}
}

func TestReceiptReturnCompletedAbortedTransferredBatch(t *testing.T) {
	// 原批次已完成：退件照常。
	s, _ := openTempStore(t)
	setupReceiptReturnBase(t, s) // 两件均签收，批次已完成
	mustReceiptReturn(t, s, "PR1", "RC1", "拒收", tClock(2026, 10, 5, 11, 0))

	// 原批次已中止：已回执成员的退件不受影响。
	s2, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s2, "P001", "站点A", t1)
	mustRegister(t, s2, "P002", "站点A", t1)
	mustDispatch(t, s2, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s2, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustAbort(t, s2, "A1", "B1", "车辆故障", tClock(2026, 10, 5, 10, 30))
	mustReceiptReturn(t, s2, "PR1", "RC1", "拒收", tClock(2026, 10, 5, 11, 0))
	b, _ := s2.BatchQuery("B1")
	if b.AbortedBy != "A1" || b.Effective() != 1 {
		t.Fatalf("退件不改变批次中止状态与进度: %+v", b)
	}
	if p, _ := s2.Query("P001"); p.Status != statusInStation || p.Station != "站点A" {
		t.Fatalf("已中止批次的签收退件后应在出发站在站: %+v", p)
	}

	// 原批次已转交：被转交批次中已先签收成员的退件不受影响。
	s3, _ := openTempStore(t)
	setupRelayBase(t, s3) // P001 已以 RC1 签收，P002/P003 待配送
	if _, _, err := s3.Transfer("T1", "B1", "B2", "李四", "原配送员车辆故障", nil, tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatal(err)
	}
	mustReceiptReturn(t, s3, "PR1", "RC1", "拒收", tClock(2026, 10, 5, 11, 30))
	b3, _ := s3.BatchQuery("B1")
	if b3.TransferredBy != "T1" || b3.Effective() != 1 {
		t.Fatalf("退件不改变批次转交状态与进度: %+v", b3)
	}
}

func TestReceiptReturnReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	setupReceiptReturnBase(t, s)
	t1 := tClock(2026, 10, 5, 11, 0)
	first := mustReceiptReturn(t, s, "PR1", "RC1", "拒收", t1)

	// 同号、同原签收、清理后同原因重放：返回首次结果与时间，不检查现状、不追加轨迹。
	res, replayed, err := s.ReceiptReturn("PR1", "RC1", "拒收", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || res != first || !res.Time.Equal(t1) {
		t.Fatalf("同内容重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	if p, _ := s.Query("P001"); len(p.Trail) != 4 {
		t.Fatalf("重放不得追加轨迹，得到 %d 条", len(p.Trail))
	}

	// 退件后发生新流转（交接），重放仍返回首次结果、不移动包裹。
	if _, _, err := s.Handoff("H1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 12, 30)); err != nil {
		t.Fatal(err)
	}
	res, replayed, err = s.ReceiptReturn("PR1", "RC1", "拒收", tClock(2026, 10, 5, 13, 0))
	if err != nil || !replayed || !res.Time.Equal(t1) {
		t.Fatalf("后续流转后重放退件仍应成立: %v replayed=%v", err, replayed)
	}
	if p, _ := s.Query("P001"); p.Station != "站点B" || len(p.Trail) != 5 {
		t.Fatalf("重放不得改变现状: %+v", p)
	}

	// 换内容冲突，已有结果不变。
	if _, _, err := s.ReceiptReturn("PR1", "RC1", "别的原因", tClock(2026, 10, 5, 13, 30)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("换原因应报冲突: %v", err)
	}
	if _, _, err := s.ReceiptReturn("PR1", "RC2", "拒收", tClock(2026, 10, 5, 13, 30)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("换原签收成报冲突: %v", err)
	}
}

func TestReceiptReturnRejections(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustReceiptOK(t, s, "RC2", "B1", "P002", resultFailed, "收件人不在", tClock(2026, 10, 5, 10, 5))
	now := tClock(2026, 10, 5, 11, 0)

	// 原回执不存在。
	if _, _, err := s.ReceiptReturn("PR1", "NOPE", "原因", now); err == nil ||
		!strings.Contains(err.Error(), "不存在") {
		t.Fatalf("原回执不存在必须拒绝: %v", err)
	}
	// 失败回执不能退件。
	if _, _, err := s.ReceiptReturn("PR1", "RC2", "原因", now); err == nil ||
		!strings.Contains(err.Error(), "只有真实签收") {
		t.Fatalf("失败回执不能退件: %v", err)
	}
	// 已撤销的签收不能退件。
	s2, _ := openTempStore(t)
	mustRegister(t, s2, "P001", "站点A", t1)
	mustDispatch(t, s2, "B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s2, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s2, "RV1", "RC1", "误录签收", tClock(2026, 10, 5, 10, 30))
	if _, _, err := s2.ReceiptReturn("PR1", "RC1", "原因", now); err == nil ||
		!strings.Contains(err.Error(), "已撤销") {
		t.Fatalf("已撤销签收不能退件: %v", err)
	}

	// 成功退件一次后：换请求号再退同一签收拒绝。
	mustReceiptReturn(t, s, "PR1", "RC1", "拒收", now)
	if _, _, err := s.ReceiptReturn("PR2", "RC1", "再退", tClock(2026, 10, 5, 11, 30)); err == nil ||
		!strings.Contains(err.Error(), "只能成功退件一次") {
		t.Fatalf("每条签收只能退件一次: %v", err)
	}
	// 退件后不能再撤销该签收。
	if _, _, err := s.RevokeReceipt("RV1", "RC1", "误录签收", tClock(2026, 10, 5, 11, 30)); err == nil ||
		!strings.Contains(err.Error(), "已办理实物退件") {
		t.Fatalf("退件后不能撤销该签收: %v", err)
	}

	// 上述失败均不得写入数据文件（成功的 PR1 除外）。
	after, _ := os.ReadFile(path)
	if strings.Contains(string(after), "PR2") || strings.Contains(string(after), "RV1") {
		t.Fatal("失败的退件/撤销不得留下记录")
	}
}

func TestReceiptReturnFailureDoesNotOccupyNumber(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustReceiptOK(t, s, "RC2", "B1", "P002", resultFailed, "收件人不在", tClock(2026, 10, 5, 10, 5))
	now := tClock(2026, 10, 5, 11, 0)

	// 首次用 PR1 提交一个必然失败的退件（失败回执）：不占号。
	if _, _, err := s.ReceiptReturn("PR1", "RC2", "拒收", now); err == nil {
		t.Fatal("失败回执退件应失败")
	}
	res := mustReceiptReturn(t, s, "PR1", "RC1", "拒收", tClock(2026, 10, 5, 11, 30))
	if res.Receipt != "RC1" {
		t.Fatalf("失败的首次退件不得占号: %+v", res)
	}
}

func TestReceiptReturnIndependentNumbering(t *testing.T) {
	s, _ := openTempStore(t)
	setupReceiptReturnBase(t, s)
	// 退件请求号可与回执请求号、批次号等同名。
	res := mustReceiptReturn(t, s, "RC1", "RC1", "拒收", tClock(2026, 10, 5, 11, 0))
	if res.Request != "RC1" {
		t.Fatalf("退件请求号应可与回执请求号同名: %+v", res)
	}
	if rc := s.ReceiptOf("RC1"); rc.ReturnedBy != "RC1" {
		t.Fatalf("同名编号下退件关联仍应正确: %+v", rc)
	}
}

func TestReceiptReturnIsNewFlow(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	// 配送前旧交接 A->B，再 B 出站、签收、退件回 B（出发站）。
	if _, _, err := s.Handoff("H1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 9, 10)); err != nil {
		t.Fatal(err)
	}
	mustDispatch(t, s, "B1", "站点B", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustReceiptReturn(t, s, "PR1", "RC1", "拒收", tClock(2026, 10, 5, 11, 0))

	// 退件算新流转：不能退回配送前的旧交接 H1。
	if _, _, err := s.Return("RT1", "H1", "错发站点", tClock(2026, 10, 5, 11, 30)); err == nil {
		t.Fatal("退件后不得退回配送前的旧交接")
	}
	// 退件后可交接、冻结、发运或加入新配送批次。
	if _, _, err := s.Handoff("H2", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatalf("退件后应可交接: %v", err)
	}
}

func TestReceiptReturnThenReSignAndReturnAgain(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustReceiptReturn(t, s, "PR1", "RC1", "首次拒收", tClock(2026, 10, 5, 11, 0))

	// 加入新配送批次并再次签收；针对新回执可再办退件。
	mustDispatch(t, s, "B2", "站点A", "李四", []string{"P001"}, tClock(2026, 10, 5, 12, 0))
	mustReceiptOK(t, s, "RC3", "B2", "P001", resultSigned, "", tClock(2026, 10, 5, 13, 0))
	res := mustReceiptReturn(t, s, "PR2", "RC3", "再次拒收", tClock(2026, 10, 5, 14, 0))
	if res.Batch != "B2" || res.Station != "站点A" {
		t.Fatalf("再次退件应针对新回执的新批次: %+v", res)
	}
	// 旧签收不能重复使用：PR1 重放返回首次结果，旧签收 RC1 不能再退。
	old, replayed, err := s.ReceiptReturn("PR1", "RC1", "首次拒收", tClock(2026, 10, 5, 14, 30))
	if err != nil || !replayed || old.Reason != "首次拒收" {
		t.Fatalf("旧退件应仍可同内容重放: %v replayed=%v", err, replayed)
	}
	if _, _, err := s.ReceiptReturn("PR3", "RC1", "再退旧签收", tClock(2026, 10, 5, 14, 30)); err == nil ||
		!strings.Contains(err.Error(), "只能成功退件一次") {
		t.Fatalf("旧签收不能重复退件: %v", err)
	}
	// 两次退件事实都保留，两次签收仍计有效回执。
	if rc := s.ReceiptOf("RC1"); rc.ReturnedBy != "PR1" {
		t.Fatalf("RC1 退件关联应保留: %+v", rc)
	}
	if rc := s.ReceiptOf("RC3"); rc.ReturnedBy != "PR2" {
		t.Fatalf("RC3 退件关联应保留: %+v", rc)
	}
	// 收件、出站、回执、退件、出站、回执、退件共 7 条轨迹。
	if p, _ := s.Query("P001"); p.Status != statusInStation || len(p.Trail) != 7 {
		t.Fatalf("两次配送退件后轨迹数应为 7，得到 %d: %+v", len(p.Trail), p)
	}
}

func TestReceiptReturnAtomicSaveRollback(t *testing.T) {
	// 落盘失败时回滚：让数据文件的父路径是一个普通文件，MkdirAll 必失败。
	s, _ := openTempStore(t)
	setupReceiptReturnBase(t, s)
	blocker := t.TempDir() + "/blocker"
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.path = blocker + "/ledger.json"
	if _, _, err := s.ReceiptReturn("PR1", "RC1", "拒收", tClock(2026, 10, 5, 11, 0)); err == nil {
		t.Fatal("保存失败应报错且不报告成功")
	}
	if _, ok := s.data.ReceiptReturns["PR1"]; ok {
		t.Fatal("保存失败后内存中不应留下退件结果")
	}
	if rc := s.data.Receipts["RC1"]; rc.ReturnedBy != "" {
		t.Fatal("保存失败后回执不应保留退件标记")
	}
	if p, _ := s.Query("P001"); p.Status != statusSigned || len(p.Trail) != 3 {
		t.Fatalf("保存失败后包裹状态与轨迹应回滚: %+v", p)
	}
}

func TestReceiptReturnPersistence(t *testing.T) {
	s, path := openTempStore(t)
	setupReceiptReturnBase(t, s)
	t1 := tClock(2026, 10, 5, 11, 0)
	mustReceiptReturn(t, s, "PR1", "RC1", "拒收", t1)

	// 重启后规则不变：状态、进度、退件事实及去重结果整体保留。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开台账失败: %v", err)
	}
	p, _ := s2.Query("P001")
	if p.Status != statusInStation || len(p.Trail) != 4 || p.Trail[3].Op != "退件" {
		t.Fatalf("重开后退件状态应保留: %+v", p)
	}
	res, replayed, err := s2.ReceiptReturn("PR1", "RC1", "拒收", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || !res.Time.Equal(t1) {
		t.Fatalf("重开后重放退件应返回首次结果: %v replayed=%v", err, replayed)
	}
	if _, _, err := s2.ReceiptReturn("PR2", "RC1", "再退", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("重开后仍不得对同一签收再次退件")
	}
	if b, _ := s2.BatchQuery("B1"); b.Effective() != 2 || !b.Done() {
		t.Fatalf("重开后批次有效回执计数应保留: %+v", b)
	}
}

func TestReceiptReturnLegacyLedgerCompat(t *testing.T) {
	// 旧有效台账（无 receiptReturns 字段）应可直接打开使用，退件功能照常。
	s, path := openTempStore(t)
	setupReceiptReturnBase(t, s)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "receiptReturns")
	buf, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("旧版台账（无 receiptReturns）应直接可用: %v", err)
	}
	mustReceiptReturn(t, s2, "PR1", "RC1", "拒收", tClock(2026, 10, 5, 11, 0))
}

func TestReceiptReturnCorruptLinksRejected(t *testing.T) {
	s, path := openTempStore(t)
	setupReceiptReturnBase(t, s)
	mustReceiptReturn(t, s, "PR1", "RC1", "拒收", tClock(2026, 10, 5, 11, 0))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tamper := func(mut func(map[string]any)) string {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		mut(doc)
		buf, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		bad := path + ".bad"
		if err := os.WriteFile(bad, buf, 0o644); err != nil {
			t.Fatal(err)
		}
		return bad
	}

	// 退件关联空缺：回执标记了退件但退件表为空。
	bad := tamper(func(doc map[string]any) { doc["receiptReturns"] = map[string]any{} })
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("退件关联空缺应拒绝读写: %v", err)
	}
	// 同一签收重复退件：退件表中凭空多一条指向 RC1。
	bad = tamper(func(doc map[string]any) {
		rrs := doc["receiptReturns"].(map[string]any)
		rrs["PR2"] = map[string]any{
			"request": "PR2", "receipt": "RC1", "batch": "B1", "parcel": "P001",
			"station": "站点A", "reason": "第二条退件", "time": "2026-10-05T11:30:00Z",
		}
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("同一签收重复退件应拒绝读写: %v", err)
	}
	// 退件未紧随原签收：把退件轨迹移到包裹轨迹开头（破坏紧随关系）。
	bad = tamper(func(doc map[string]any) {
		trail := doc["parcels"].(map[string]any)["P001"].(map[string]any)["trail"].([]any)
		ret := trail[len(trail)-1]
		rest := append([]any{}, trail[:len(trail)-1]...)
		doc["parcels"].(map[string]any)["P001"].(map[string]any)["trail"] = append([]any{ret}, rest...)
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("退件未紧随原签收应拒绝读写: %v", err)
	}
	// 结果与轨迹不符：退件结果的原因被改写。
	bad = tamper(func(doc map[string]any) {
		doc["receiptReturns"].(map[string]any)["PR1"].(map[string]any)["reason"] = "被改写"
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("退件结果与轨迹不符应拒绝读写: %v", err)
	}
	// 无记录支持的当前状态：把已退件件改回已签收但保留退件轨迹（末条轨迹为退件却状态已签收）。
	bad = tamper(func(doc map[string]any) {
		doc["parcels"].(map[string]any)["P001"].(map[string]any)["status"] = statusSigned
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("无轨迹支持的当前状态应拒绝读写: %v", err)
	}
	// 合法后续流转按后来轨迹核对：退件后再交接（末条为交接，状态在站 B）应通过校验。
	s2, path2 := openTempStore(t)
	setupReceiptReturnBase(t, s2)
	mustReceiptReturn(t, s2, "PR1", "RC1", "拒收", tClock(2026, 10, 5, 11, 0))
	if _, _, err := s2.Handoff("H1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path2); err != nil {
		t.Fatalf("退件后的合法后续流转应通过重载校验: %v", err)
	}
}
