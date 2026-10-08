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

// 准备：P001、P002 在站点A 经张三的批次 B1 出站，两件均已签收（批次已完成）。
func setupReturnBase(t *testing.T, s *Store) {
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
	setupReturnBase(t, s)
	t1 := tClock(2026, 10, 5, 11, 0)

	res := mustReceiptReturn(t, s, "RR1", "RC1", "客户申请退货", t1)
	if res.Request != "RR1" || res.Receipt != "RC1" || res.Batch != "B1" ||
		res.Parcel != "P001" || res.Station != "站点A" ||
		res.Reason != "客户申请退货" || !res.Time.Equal(t1) {
		t.Fatalf("退件结果不符: %+v", res)
	}

	// 成功仅将该件恢复原出发站在站，追加退件轨迹。
	p, _ := s.Query("P001")
	if p.Status != statusInStation || p.Station != "站点A" {
		t.Fatalf("退件后应在原出发站在站: %+v", p)
	}
	if len(p.Trail) != 4 {
		t.Fatalf("退件后应有四条轨迹，得到 %d", len(p.Trail))
	}
	e := p.Trail[3]
	if e.Op != "退件" || e.Station != "站点A" || e.Batch != "B1" ||
		e.Request != "RR1" || e.RefRequest != "RC1" ||
		e.Reason != "客户申请退货" || !e.Time.Equal(t1) {
		t.Fatalf("退件轨迹不符: %+v", e)
	}
	// 原签收轨迹永久保留。
	if p.Trail[2].Op != "回执" || p.Trail[2].Request != "RC1" {
		t.Fatalf("原签收轨迹必须保留: %+v", p.Trail[2])
	}

	// 原签收事实仍计有效回执，批次状态与进度不因退件改变（仍已完成 2/2）。
	b, _ := s.BatchQuery("B1")
	if !b.Done() || b.Effective() != 2 {
		t.Fatalf("退件不应改变批次状态与进度: done=%v effective=%d", b.Done(), b.Effective())
	}
	if b.Receipts["P001"].Request != "RC1" || b.Receipts["P001"].ReturnedBy != "RR1" ||
		b.Receipts["P001"].RevokedBy != "" {
		t.Fatalf("批次内原签收据目应保留并标记退件: %+v", b.Receipts["P001"])
	}
	rc := s.ReceiptOf("RC1")
	if rc == nil || rc.RevokedBy != "" || rc.ReturnedBy != "RR1" {
		t.Fatalf("原签收结果应保留、未撤销并标记退件: %+v", rc)
	}
	// 其他成员不变。
	p2, _ := s.Query("P002")
	if p2.Status != statusSigned || len(p2.Trail) != 3 {
		t.Fatalf("其他成员状态与轨迹不得改变: %+v", p2)
	}
}

func TestReceiptReturnBatchClosedStatesAllowed(t *testing.T) {
	// 批次已完成不阻止退件（setupReturnBase 即已完成）。
	s, _ := openTempStore(t)
	setupReturnBase(t, s)
	mustReceiptReturn(t, s, "RR1", "RC1", "退货", tClock(2026, 10, 5, 11, 0))

	// 批次已中止：P001 已签收、P002 未回执，中止只收回 P002，P001 仍可退件。
	s2, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s2, "P001", "站点A", t1)
	mustRegister(t, s2, "P002", "站点A", t1)
	mustDispatch(t, s2, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s2, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustAbort(t, s2, "A1", "B1", "车辆故障", tClock(2026, 10, 5, 10, 30))
	mustReceiptReturn(t, s2, "RR1", "RC1", "退货", tClock(2026, 10, 5, 11, 0))
	if b, _ := s2.BatchQuery("B1"); b.Effective() != 1 {
		t.Fatal("中止批次中已签收件退件后仍应计 1 条有效回执")
	}

	// 批次已转交：P001 已签收，续接把余件 P002、P003 转走后原批次永久转交，P001 仍可退件。
	s3, _ := openTempStore(t)
	setupRelayBase(t, s3) // P001 已 RC1 签收，P002、P003 未回执
	if _, _, err := s3.Transfer("T1", "B1", "B2", "李四", "原配送员车辆故障", nil, tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatal(err)
	}
	mustReceiptReturn(t, s3, "RR1", "RC1", "退货", tClock(2026, 10, 5, 12, 0))
	p, _ := s3.Query("P001")
	if p.Station != "站点A" || p.Status != statusInStation || len(p.Trail) != 4 ||
		p.Trail[3].Op != "退件" {
		t.Fatalf("已转交批次的签收件仍应可退件: %+v", p)
	}
}

func TestReceiptReturnOtherMembersLaterWork(t *testing.T) {
	// 其他成员的后续作业不阻止退件：P002 失败回执、回站再出站后，P001 原签收仍可退。
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustReceiptOK(t, s, "RC2", "B1", "P002", resultFailed, "收件人不在", tClock(2026, 10, 5, 10, 5))
	mustDispatch(t, s, "B2", "站点A", "李四", []string{"P002"}, tClock(2026, 10, 5, 10, 40))
	mustReceiptReturn(t, s, "RR1", "RC1", "退货", tClock(2026, 10, 5, 11, 0))
}

func TestReceiptReturnRejections(t *testing.T) {
	s, path := openTempStore(t)
	setupReturnBase(t, s)
	now := tClock(2026, 10, 5, 11, 0)

	// 原回执不存在。
	if _, _, err := s.ReceiptReturn("RR1", "NOPE", "原因", now); err == nil {
		t.Fatal("原回执不存在必须拒绝")
	}
	// 失败回执不能退件（另起一件失败回执）。
	mustRegister(t, s, "P003", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B3", "站点A", "王五", []string{"P003"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC3", "B3", "P003", resultFailed, "收件人不在", tClock(2026, 10, 5, 10, 10))
	if _, _, err := s.ReceiptReturn("RR1", "RC3", "原因", now); err == nil ||
		!strings.Contains(err.Error(), "只有已真实签收") {
		t.Fatalf("失败回执不能退件: %v", err)
	}
	// 失败不占号：同一退件号可用于合法退件。
	res := mustReceiptReturn(t, s, "RR1", "RC1", "退货", now)
	if res.Receipt != "RC1" {
		t.Fatalf("失败的首次退件不得占号: %+v", res)
	}
	// 每条签收只能成功退件一次：换请求号再退拒绝。
	if _, _, err := s.ReceiptReturn("RR2", "RC1", "再次退货", now); err == nil ||
		!strings.Contains(err.Error(), "每条签收只能退件一次") {
		t.Fatalf("已退件的签收换号再退必须拒绝: %v", err)
	}
	// 退件后不能再撤销该签收。
	if _, _, err := s.RevokeReceipt("RV1", "RC1", "误录签收", now); err == nil ||
		!strings.Contains(err.Error(), "退件后不能再撤销") {
		t.Fatalf("退件后撤销必须拒绝: %v", err)
	}

	// 已撤销的签收不能退件。
	s2, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s2, "P001", "站点A", t1)
	mustDispatch(t, s2, "B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s2, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s2, "RV1", "RC1", "误录签收", tClock(2026, 10, 5, 11, 0))
	if _, _, err := s2.ReceiptReturn("RR1", "RC1", "退货", tClock(2026, 10, 5, 12, 0)); err == nil ||
		!strings.Contains(err.Error(), "已撤销") {
		t.Fatalf("已撤销签收不能退件: %v", err)
	}

	// 上述失败均不得写入数据文件（成功的 RR1 除外）。
	after, _ := os.ReadFile(path)
	if strings.Contains(string(after), "RR2") || strings.Contains(string(after), "RV1") {
		t.Fatal("失败的退件与被拒撤销不得留下记录")
	}
}

func TestReceiptReturnReplayConflictAndLaterFlow(t *testing.T) {
	s, path := openTempStore(t)
	setupReturnBase(t, s)
	t1 := tClock(2026, 10, 5, 11, 0)
	first := mustReceiptReturn(t, s, "RR1", "RC1", "退货", t1)

	// 同号、同原签收、同原因重放（空白清洗由 CLI 层完成）：返回首次结果与时间，不追加轨迹。
	res, replayed, err := s.ReceiptReturn("RR1", "RC1", "退货", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || res != first || !res.Time.Equal(t1) {
		t.Fatalf("同内容重放应返回首次结果: %v replayed=%v res=%+v", err, replayed, res)
	}
	if p, _ := s.Query("P001"); len(p.Trail) != 4 {
		t.Fatalf("重放不得追加轨迹，得到 %d 条", len(p.Trail))
	}
	// 换原因、换原签收均冲突。
	if _, _, err := s.ReceiptReturn("RR1", "RC1", "别的原因", tClock(2026, 10, 5, 12, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("换原因应报冲突: %v", err)
	}
	if _, _, err := s.ReceiptReturn("RR1", "RC2", "退货", tClock(2026, 10, 5, 12, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("换原签收容应报冲突: %v", err)
	}

	// 退件后继续流转：交接、再次出站、再次签收、再次退件；此后旧退件重放仍成立，
	// 不检查现状、不改变状态。
	if _, _, err := s.Handoff("H1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 12, 30)); err != nil {
		t.Fatal(err)
	}
	mustDispatch(t, s, "B9", "站点B", "李四", []string{"P001"}, tClock(2026, 10, 5, 13, 0))
	mustReceiptOK(t, s, "RC9", "B9", "P001", resultSigned, "", tClock(2026, 10, 5, 13, 30))
	mustReceiptReturn(t, s, "RR9", "RC9", "再次退货", tClock(2026, 10, 5, 14, 0))

	res, replayed, err = s.ReceiptReturn("RR1", "RC1", "退货", tClock(2026, 10, 5, 15, 0))
	if err != nil || !replayed || !res.Time.Equal(t1) {
		t.Fatalf("后续流转及再次退件后旧重放仍应成立: %v replayed=%v", err, replayed)
	}
	p, _ := s.Query("P001")
	if p.Status != statusInStation || p.Station != "站点B" || len(p.Trail) != 8 {
		t.Fatalf("重放不得改变现状: station=%s status=%s trails=%d", p.Station, p.Status, len(p.Trail))
	}

	// 合法后续流转（交接、新批次、二次签收、二次退件）落盘后必须能正常重载，
	// 且重载后旧退件重放仍返回首次时间——损坏校验不误伤合法状态。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("退件后继续流转的台账应可正常重载: %v", err)
	}
	if rr := s2.ReceiptReturnOf("RR1"); rr == nil || !rr.Time.Equal(t1) {
		t.Fatalf("重载后旧退件结果应完整保留: %+v", rr)
	}
	if _, replayed, err := s2.ReceiptReturn("RR1", "RC1", "退货", tClock(2026, 10, 5, 16, 0)); err != nil || !replayed {
		t.Fatalf("重载后旧退件同内容重放应返回首次结果: %v replayed=%v", err, replayed)
	}
}

func TestReceiptReturnThenNewFlowAndReSign(t *testing.T) {
	s, _ := openTempStore(t)
	// 先在站点A 登记，交接至站点B，再出站签收：退件接收站应为该次配送出发站 B。
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	if _, _, err := s.Handoff("H1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 9, 30)); err != nil {
		t.Fatal(err)
	}
	mustDispatch(t, s, "B1", "站点B", "张三", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 11, 0))
	mustReceiptReturn(t, s, "RR1", "RC1", "退货", tClock(2026, 10, 5, 12, 0))

	p, _ := s.Query("P001")
	if p.Station != "站点B" || p.Status != statusInStation {
		t.Fatalf("退件应恢复该次配送出发站 B 在站: %+v", p)
	}
	// 退件算新流转，不恢复旧交接 H1 的退回资格。
	if _, _, err := s.Return("RT1", "H1", "错发站点", tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatal("退件后不得恢复旧交接的退回资格")
	}
	// 退件后可冻结、再解除、交接、发运或加入新配送批次。
	if _, _, err := s.Freeze("E1", "P001", "站点B", "破损", tClock(2026, 10, 5, 13, 0)); err != nil {
		t.Fatalf("退件回站后应可冻结: %v", err)
	}
	if _, _, err := s.Unfreeze("U1", "E1", "放行", tClock(2026, 10, 5, 13, 30)); err != nil {
		t.Fatal(err)
	}
	mustDispatch(t, s, "B2", "站点B", "王五", []string{"P001"}, tClock(2026, 10, 5, 14, 0))
	mustReceiptOK(t, s, "RC2", "B2", "P001", resultSigned, "", tClock(2026, 10, 5, 15, 0))
	// 再次签收可针对新回执办理退件；旧签收不能重复使用。
	mustReceiptReturn(t, s, "RR2", "RC2", "二次退货", tClock(2026, 10, 5, 16, 0))
	if _, _, err := s.ReceiptReturn("RR3", "RC1", "旧签收再退", tClock(2026, 10, 5, 16, 30)); err == nil ||
		!strings.Contains(err.Error(), "每条签收只能退件一次") {
		t.Fatalf("旧签收不能重复退件: %v", err)
	}
	// 原批次状态与进度不变：B1、B2 都仍已完成。
	if b, _ := s.BatchQuery("B1"); !b.Done() || b.Effective() != 1 {
		t.Fatal("B1 不应因退件改变状态")
	}
	if b, _ := s.BatchQuery("B2"); !b.Done() || b.Effective() != 1 {
		t.Fatal("B2 不应因退件改变状态")
	}
}

func TestReceiptReturnReceiptReplaysUnchanged(t *testing.T) {
	s, _ := openTempStore(t)
	setupReturnBase(t, s)
	tSign := tClock(2026, 10, 5, 10, 0)
	mustReceiptReturn(t, s, "RR1", "RC1", "退货", tClock(2026, 10, 5, 11, 0))

	// receipt 重放旧签收仍返回原结果和时间，不改变包裹。
	rc, replayed, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || !rc.Time.Equal(tSign) || rc.ReturnedBy != "RR1" || rc.RevokedBy != "" {
		t.Fatalf("旧签收重放应返回原结果并保留退件标记: %v replayed=%v rc=%+v", err, replayed, rc)
	}
	p, _ := s.Query("P001")
	if p.Status != statusInStation || len(p.Trail) != 4 {
		t.Fatalf("重放签收不得改变包裹: %+v", p)
	}

	// receipt-import 混合旧签收重放与新回执，整份原子处理。
	items, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultSigned},
	}, tClock(2026, 10, 5, 12, 30))
	if err != nil {
		t.Fatalf("旧签收重放导入应成功: %v", err)
	}
	if len(items) != 1 || !items[0].Replayed || items[0].Revoked {
		t.Fatalf("导入标记不符: %+v", items)
	}
	if b, _ := s.BatchQuery("B1"); b.Effective() != 2 {
		t.Fatalf("重放导入不得改变进度: %d", b.Effective())
	}

	// 退件回站后加入新批次，导入混合“已退件旧签收的重放”与“新批次新签收”：
	// 重放不改变包裹，新回执生效，整份一次原子保存。
	mustDispatch(t, s, "B8", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 13, 0))
	items, err = s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultSigned}, // 旧签收重放
		{Request: "RC8", Batch: "B8", Parcel: "P001", Result: resultSigned}, // 新签收
	}, tClock(2026, 10, 5, 13, 30))
	if err != nil {
		t.Fatalf("混合重放与新回执的导入应整体成功: %v", err)
	}
	if len(items) != 2 || !items[0].Replayed || items[1].Replayed {
		t.Fatalf("导入标记应分别为重放与新增: %+v", items)
	}
	p, _ = s.Query("P001")
	if p.Status != statusSigned || currentBatch(p) != "B8" || len(p.Trail) != 6 {
		t.Fatalf("混合导入后应以新批次签收为准: %+v trails=%d", p, len(p.Trail))
	}
	if rc := s.ReceiptOf("RC1"); rc.ReturnedBy != "RR1" {
		t.Fatalf("旧签收的退件事实不得被重放导入抹掉: %+v", rc)
	}
}

func TestReceiptReturnIndependentNumbering(t *testing.T) {
	s, _ := openTempStore(t)
	setupReturnBase(t, s)
	// 退件请求号可与其他业务编号同名。
	mustReceiptReturn(t, s, "RC1", "RC1", "与回执同名的退件号", tClock(2026, 10, 5, 11, 0))
	// 同号同内容重放走退件去重范围。
	rr, replayed, err := s.ReceiptReturn("RC1", "RC1", "与回执同名的退件号", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || rr.Request != "RC1" {
		t.Fatalf("退件号与回执同名应独立去重: %v replayed=%v", err, replayed)
	}
	// 同名号用于撤销另一签收属于另一去重范围：可以成功，不受退件号同名影响。
	if _, _, err := s.RevokeReceipt("RC1", "RC2", "误录签收", tClock(2026, 10, 5, 13, 0)); err != nil {
		t.Fatalf("退件号同名不应影响其他业务的独立去重范围: %v", err)
	}
	if rc := s.ReceiptOf("RC2"); rc.RevokedBy != "RC1" {
		t.Fatalf("RC2 应被同名撤销请求撤销: %+v", rc)
	}
}

func TestReceiptReturnPersistence(t *testing.T) {
	s, path := openTempStore(t)
	setupReturnBase(t, s)
	t1 := tClock(2026, 10, 5, 11, 0)
	mustReceiptReturn(t, s, "RR1", "RC1", "退货", t1)

	// 重启后规则不变：状态、轨迹、关联与去重结果整体保留。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开台账失败: %v", err)
	}
	p, _ := s2.Query("P001")
	if p.Status != statusInStation || len(p.Trail) != 4 || p.Trail[3].Op != "退件" {
		t.Fatalf("重开后退件状态应保留: %+v", p)
	}
	res, replayed, err := s2.ReceiptReturn("RR1", "RC1", "退货", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || !res.Time.Equal(t1) {
		t.Fatalf("重开后重放退件应返回首次结果: %v replayed=%v", err, replayed)
	}
	if _, _, err := s2.ReceiptReturn("RR2", "RC1", "再退", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("重开后仍不得重复退件")
	}
	if _, _, err := s2.RevokeReceipt("RV1", "RC1", "误录", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("重开后退件与撤销仍互斥")
	}
	if b, _ := s2.BatchQuery("B1"); b.Effective() != 2 || !b.Done() {
		t.Fatalf("重开后批次进度应保留: effective=%d", b.Effective())
	}
}

func TestReceiptReturnCorruptReloadRejected(t *testing.T) {
	s, path := openTempStore(t)
	setupReturnBase(t, s)
	mustReceiptReturn(t, s, "RR1", "RC1", "退货", tClock(2026, 10, 5, 11, 0))

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

	// 新增退件关联空缺：回执标记了退件但退件结果表为空。
	bad := tamper(func(doc map[string]any) { doc["receiptReturns"] = map[string]any{} })
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("退件关联空缺应拒绝读写: %v", err)
	}
	// 同一签收重复退件：退件结果表出现第二条指向同一回执。
	bad = tamper(func(doc map[string]any) {
		first := doc["receiptReturns"].(map[string]any)["RR1"].(map[string]any)
		cp := map[string]any{}
		for k, v := range first {
			cp[k] = v
		}
		cp["request"] = "RR2"
		doc["receiptReturns"].(map[string]any)["RR2"] = cp
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("同一签收重复退件应拒绝读写: %v", err)
	}
	// 退件未紧随原签收：在回执与退件之间插入一条交接轨迹。
	bad = tamper(func(doc map[string]any) {
		p1 := doc["parcels"].(map[string]any)["P001"].(map[string]any)
		trail := p1["trail"].([]any)
		// 退件移到一条交接记录之后（交接站点与退件站相同，末条状态仍自洽）。
		ret := trail[len(trail)-1]
		mid := map[string]any{
			"op": "交接", "station": "站点A",
			"time": "2026-10-05T10:30:00Z",
		}
		p1["trail"] = append(append(trail[:len(trail)-1], mid), ret)
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("退件未紧随原签收入库应拒绝读写: %v", err)
	}
	// 结果与轨迹不符：退件原因与轨迹中的原因不一致。
	bad = tamper(func(doc map[string]any) {
		doc["receiptReturns"].(map[string]any)["RR1"].(map[string]any)["reason"] = "别的原因"
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("退件结果与轨迹不符应拒绝读写: %v", err)
	}
	// 退件结果引用不存在的原回执。
	bad = tamper(func(doc map[string]any) {
		doc["receiptReturns"].(map[string]any)["RR1"].(map[string]any)["receipt"] = "NOPE"
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("退件引用不存在的原回执应拒绝读写: %v", err)
	}
	// 无记录支持的当前状态：去掉退件轨迹却把包裹标回已签收，退件结果仍在。
	bad = tamper(func(doc map[string]any) {
		p1 := doc["parcels"].(map[string]any)["P001"].(map[string]any)
		trail := p1["trail"].([]any)
		p1["trail"] = trail[:len(trail)-1]
		p1["status"] = "已签收"
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("缺少退件轨迹的退件结果应拒绝读写: %v", err)
	}
	// 撤销与退件同时标记。
	bad = tamper(func(doc map[string]any) {
		doc["receipts"].(map[string]any)["RC1"].(map[string]any)["revokedBy"] = "RV1"
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("同一签收同时撤销与退件应拒绝读写: %v", err)
	}
}

func TestReceiptReturnLegacyLedgerCompatible(t *testing.T) {
	// 旧有效台账没有 receiptReturns 字段：直接使用，载入后可正常办理退件。
	legacy := []byte(`{
	  "version": 1,
	  "parcels": {
	    "P001": {
	      "id": "P001",
	      "station": "站点A",
	      "status": "在站",
	      "registered": "2026-10-05T09:00:00Z",
	      "trail": [
	        {"op": "收件", "station": "站点A", "time": "2026-10-05T09:00:00Z"}
	      ]
	    }
	  },
	  "handoffs": {}
	}`)
	path := t.TempDir() + "/legacy.json"
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("旧台账应可直接打开: %v", err)
	}
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustReceiptReturn(t, s, "RR1", "RC1", "退货", tClock(2026, 10, 5, 11, 0))
}
