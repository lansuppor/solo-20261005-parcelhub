package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func mustRevoke(t *testing.T, s *Store, request, receipt, reason string, now time.Time) *RevokeResult {
	t.Helper()
	res, replayed, err := s.RevokeReceipt(request, receipt, reason, now)
	if err != nil || replayed {
		t.Fatalf("RevokeReceipt(%q) 意外失败: %v replayed=%v", request, err, replayed)
	}
	return res
}

// 准备一个在站点A 出发、张三配送的批次 B1（P001、P002）。
func setupRevokeBase(t *testing.T, s *Store) {
	t.Helper()
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
}

func TestRevokeReceiptSignedSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	setupRevokeBase(t, s)
	t1 := tClock(2026, 10, 5, 10, 0)
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", t1)
	mustReceiptOK(t, s, "RC2", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 10, 5))
	if b, _ := s.BatchQuery("B1"); !b.Done() {
		t.Fatal("全部回执后批次应已完成")
	}

	// 其他成员的后续操作不阻止撤销：P002 的回执在 P001 之后，仍可撤销 P001 的回执。
	t2 := tClock(2026, 10, 5, 11, 0)
	res := mustRevoke(t, s, "RV1", "RC1", "误录签收，实际仍配送中", t2)
	if res.Request != "RV1" || res.Receipt != "RC1" || res.Batch != "B1" || res.Parcel != "P001" ||
		res.Reason != "误录签收，实际仍配送中" || !res.Time.Equal(t2) {
		t.Fatalf("撤销结果不符: %+v", res)
	}

	// 该件恢复原批次配送中，站点、配送员不变，追加一条可辨认的撤销回执轨迹。
	p, _ := s.Query("P001")
	if p.Status != statusDelivering || p.Station != "站点A" {
		t.Fatalf("撤销后应恢复原批次配送中、站点不变: %+v", p)
	}
	if currentBatch(p) != "B1" {
		t.Fatalf("撤销后当前配送归属应仍为原批次: %+v", p)
	}
	if len(p.Trail) != 4 {
		t.Fatalf("撤销后应有四条轨迹，得到 %d", len(p.Trail))
	}
	e := p.Trail[3]
	if e.Op != "撤销回执" || e.Station != "站点A" || e.Batch != "B1" ||
		e.Request != "RV1" || e.RefRequest != "RC1" || e.Reason != "误录签收，实际仍配送中" || !e.Time.Equal(t2) {
		t.Fatalf("撤销回执轨迹不符: %+v", e)
	}
	// 原回执轨迹永久保留。
	if p.Trail[2].Op != "回执" || p.Trail[2].Request != "RC1" {
		t.Fatalf("原回执轨迹必须保留: %+v", p.Trail[2])
	}

	// 取消其有效回执计数：原本已完成的批次恢复配送中，其他成员及回执不变。
	b, _ := s.BatchQuery("B1")
	if b.Done() || b.Effective() != 1 {
		t.Fatalf("撤销后批次应恢复配送中（有效回执 1/2）: %+v", b)
	}
	if b.Receipts["P001"].RevokedBy != "RV1" {
		t.Fatalf("批次内回执条目应标记已撤销: %+v", b.Receipts["P001"])
	}
	if b.Receipts["P002"].RevokedBy != "" || b.Receipts["P002"].Request != "RC2" {
		t.Fatalf("其他成员的回执不得改变: %+v", b.Receipts["P002"])
	}
	if b.Courier != "张三" || len(b.Parcels) != 2 || b.Parcels[0] != "P001" {
		t.Fatalf("批次原成员顺序与配送员必须保留: %+v", b)
	}
	// 原回执结果永久保留并可辨认已撤销，回执请求号不删除也不释放。
	rc := s.ReceiptOf("RC1")
	if rc == nil || rc.RevokedBy != "RV1" || rc.Result != resultSigned || !rc.Time.Equal(t1) {
		t.Fatalf("原回执结果应保留并标记已撤销: %+v", rc)
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusSigned || len(p2.Trail) != 3 {
		t.Fatalf("其他成员状态与轨迹不得改变: %+v", p2)
	}
}

func TestRevokeReceiptFailedSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	setupRevokeBase(t, s)
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultFailed, "收件人不在", tClock(2026, 10, 5, 10, 0))
	if p, _ := s.Query("P001"); p.Status != statusInStation {
		t.Fatal("失败回执后应在站")
	}

	res := mustRevoke(t, s, "RV1", "RC1", "误录失败，实际仍配送中", tClock(2026, 10, 5, 11, 0))
	if res.Batch != "B1" || res.Parcel != "P001" {
		t.Fatalf("撤销结果不符: %+v", res)
	}
	p, _ := s.Query("P001")
	if p.Status != statusDelivering || p.Station != "站点A" || currentBatch(p) != "B1" {
		t.Fatalf("撤销失败回执后应恢复原批次配送中: %+v", p)
	}
}

func TestRevokeReceiptReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	setupRevokeBase(t, s)
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	t1 := tClock(2026, 10, 5, 11, 0)
	first := mustRevoke(t, s, "RV1", "RC1", "误录签收", t1)

	// 同号、同原回执、清理后同原因重放：返回首次信息和时间，不检查现状、不追加轨迹。
	res, replayed, err := s.RevokeReceipt("RV1", "RC1", "误录签收", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || res != first || !res.Time.Equal(t1) {
		t.Fatalf("同内容重放应返回首次结果: %v replayed=%v res=%+v", err, replayed, res)
	}
	if p, _ := s.Query("P001"); len(p.Trail) != 4 {
		t.Fatalf("重放不得追加轨迹，得到 %d 条", len(p.Trail))
	}

	// 撤销后用新回执请求号重新签收，批次再次完成；此后重放撤销仍成立。
	mustReceiptOK(t, s, "RC9", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 12, 30))
	mustReceiptOK(t, s, "RC2", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 12, 40))
	res, replayed, err = s.RevokeReceipt("RV1", "RC1", "误录签收", tClock(2026, 10, 5, 13, 0))
	if err != nil || !replayed || !res.Time.Equal(t1) {
		t.Fatalf("重新回执后重放撤销仍应成立: %v replayed=%v", err, replayed)
	}
	if p, _ := s.Query("P001"); len(p.Trail) != 5 || p.Status != statusSigned {
		t.Fatalf("重放不得改变现状: %+v", p)
	}

	// 换内容冲突，已有结果不变。
	if _, _, err := s.RevokeReceipt("RV1", "RC1", "别的原因", tClock(2026, 10, 5, 13, 30)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("换原因应报冲突: %v", err)
	}
	if _, _, err := s.RevokeReceipt("RV1", "RC2", "误录签收", tClock(2026, 10, 5, 13, 30)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("换原回执应报冲突: %v", err)
	}
}

func TestRevokeReceiptRejections(t *testing.T) {
	s, path := openTempStore(t)
	setupRevokeBase(t, s)
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustReceiptOK(t, s, "RC2", "B1", "P002", resultFailed, "收件人不在", tClock(2026, 10, 5, 10, 5))
	now := tClock(2026, 10, 5, 11, 0)

	// 原回执不存在。
	if _, _, err := s.RevokeReceipt("RV1", "NOPE", "原因", now); err == nil {
		t.Fatal("原回执不存在必须拒绝")
	}
	// 原回执之后发生过流转：P002 失败回执后重新出站。
	mustDispatch(t, s, "B2", "站点A", "李四", []string{"P002"}, tClock(2026, 10, 5, 10, 30))
	if _, _, err := s.RevokeReceipt("RV1", "RC2", "原因", now); err == nil ||
		!strings.Contains(err.Error(), "之后又发生流转") {
		t.Fatalf("回执后发生流转必须拒绝: %v", err)
	}
	// 失败不占号：同一撤销号可继续用于其他撤销。
	res := mustRevoke(t, s, "RV1", "RC1", "误录签收", now)
	if res.Receipt != "RC1" {
		t.Fatalf("失败的首次撤销不得占号: %+v", res)
	}
	// 每条原回执只能撤销一次：换撤销号再次撤销它拒绝。
	if _, _, err := s.RevokeReceipt("RV2", "RC1", "再次撤销", now); err == nil ||
		!strings.Contains(err.Error(), "只能撤销一次") {
		t.Fatalf("已撤销的回执换号再撤销必须拒绝: %v", err)
	}
	// 失败回执后被冻结：冻结或解除均阻止撤销。
	mustReceiptOK(t, s, "RC3", "B2", "P002", resultFailed, "再次不在", tClock(2026, 10, 5, 11, 30))
	if _, _, err := s.Freeze("E1", "P002", "站点A", "破损", tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RevokeReceipt("RV3", "RC3", "原因", now); err == nil {
		t.Fatal("回执后发生冻结必须拒绝")
	}
	if _, _, err := s.Unfreeze("U1", "E1", "放行", tClock(2026, 10, 5, 12, 30)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RevokeReceipt("RV3", "RC3", "原因", now); err == nil {
		t.Fatal("回执后发生冻结并解除也必须拒绝")
	}

	// 上述失败均不得写入数据文件（成功的 RV1 除外）。
	after, _ := os.ReadFile(path)
	if strings.Contains(string(after), "RV2") || strings.Contains(string(after), "RV3") {
		t.Fatal("失败的撤销不得留下记录")
	}
}

func TestRevokeReceiptBatchClosed(t *testing.T) {
	s, _ := openTempStore(t)
	setupRevokeBase(t, s)
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustAbort(t, s, "A1", "B1", "车辆故障", tClock(2026, 10, 5, 11, 0))
	if _, _, err := s.RevokeReceipt("RV1", "RC1", "原因", tClock(2026, 10, 5, 12, 0)); err == nil ||
		!strings.Contains(err.Error(), "已中止") {
		t.Fatalf("批次已中止必须拒绝撤销: %v", err)
	}

	s2, _ := openTempStore(t)
	setupRelayBase(t, s2) // P001 已以 RC1 签收
	if _, _, err := s2.Transfer("T1", "B1", "B2", "李四", "原配送员车辆故障", nil, tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.RevokeReceipt("RV1", "RC1", "原因", tClock(2026, 10, 5, 12, 0)); err == nil ||
		!strings.Contains(err.Error(), "已转交") {
		t.Fatalf("批次已转交必须拒绝撤销: %v", err)
	}
}

func TestRevokeThenReReceiptAndAbort(t *testing.T) {
	s, _ := openTempStore(t)
	setupRevokeBase(t, s)
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultFailed, "收件人不在", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s, "RV1", "RC1", "误录失败", tClock(2026, 10, 5, 11, 0))

	// 撤销后可用新回执请求号再次提交回执；原回执请求号不释放，同内容重放仍返回原结果。
	mustReceiptOK(t, s, "RC2", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 12, 0))
	res, replayed, err := s.Receipt("RC1", "B1", "P001", resultFailed, "收件人不在", tClock(2026, 10, 5, 13, 0))
	if err != nil || !replayed || res.RevokedBy != "RV1" || res.Result != resultFailed {
		t.Fatalf("已撤销回执的同内容重放应返回原结果并标明已撤销: %v replayed=%v res=%+v", err, replayed, res)
	}
	if p, _ := s.Query("P001"); p.Status != statusSigned || len(p.Trail) != 5 {
		t.Fatalf("重放已撤销回执不得恢复回执或改变当前状态: %+v", p)
	}
	// 换内容仍冲突。
	if _, _, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 13, 30)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("已撤销回执换内容应报冲突: %v", err)
	}
	// 批次内条目已换成新回执，旧条目内容仍由台账级结果表保留。
	b, _ := s.BatchQuery("B1")
	if b.Receipts["P001"].Request != "RC2" || b.Receipts["P001"].RevokedBy != "" {
		t.Fatalf("批次内应记录新的有效回执: %+v", b.Receipts["P001"])
	}

	// 撤销后该件也可随原批次中止。
	s2, _ := openTempStore(t)
	setupRevokeBase(t, s2)
	mustReceiptOK(t, s2, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s2, "RV1", "RC1", "误录签收", tClock(2026, 10, 5, 11, 0))
	ab := mustAbort(t, s2, "A1", "B1", "车辆故障", tClock(2026, 10, 5, 12, 0))
	if len(ab.Parcels) != 2 || ab.Parcels[0] != "P001" || ab.Parcels[1] != "P002" {
		t.Fatalf("撤销件应计入中止收回集合: %v", ab.Parcels)
	}
	if p, _ := s2.Query("P001"); p.Status != statusInStation {
		t.Fatalf("中止后撤销件应在站: %+v", p)
	}

	// 撤销后该件也可随原批次续接。
	s3, _ := openTempStore(t)
	setupRevokeBase(t, s3)
	mustReceiptOK(t, s3, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s3, "RV1", "RC1", "误录签收", tClock(2026, 10, 5, 11, 0))
	tr, _, err := s3.Transfer("T1", "B1", "B2", "李四", "原配送员车辆故障", nil, tClock(2026, 10, 5, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Parcels) != 2 || tr.Parcels[0] != "P001" {
		t.Fatalf("撤销件应计入续接转交集合: %v", tr.Parcels)
	}
}

func TestRevokeDoesNotRestoreReturnEligibility(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	if _, _, err := s.Handoff("H1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 9, 30)); err != nil {
		t.Fatal(err)
	}
	mustDispatch(t, s, "B1", "站点B", "张三", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultFailed, "收件人不在", tClock(2026, 10, 5, 11, 0))
	mustRevoke(t, s, "RV1", "RC1", "误录失败", tClock(2026, 10, 5, 12, 0))
	mustReceiptOK(t, s, "RC2", "B1", "P001", resultFailed, "再次不在", tClock(2026, 10, 5, 13, 0))
	// 撤销不恢复配送前旧交接的退回资格。
	if _, _, err := s.Return("RT1", "H1", "错发站点", tClock(2026, 10, 5, 14, 0)); err == nil {
		t.Fatal("撤销后仍不得退回配送前的旧交接")
	}
}

func TestRevokeReceiptImportReplay(t *testing.T) {
	s, _ := openTempStore(t)
	setupRevokeBase(t, s)
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s, "RV1", "RC1", "误录签收", tClock(2026, 10, 5, 11, 0))

	// 导入混合已撤销回执的同内容重放与新回执：重放标明已撤销，进度只计未撤销结果。
	items, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultSigned},
		{Request: "RC2", Batch: "B1", Parcel: "P002", Result: resultSigned},
	}, tClock(2026, 10, 5, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || !items[0].Replayed || !items[0].Revoked || items[1].Replayed || items[1].Revoked {
		t.Fatalf("导入结果标记不符: %+v", items)
	}
	if b, _ := s.BatchQuery("B1"); b.Effective() != 1 || b.Done() {
		t.Fatalf("进度只计未撤销结果: %+v", b)
	}
	// 已撤销回执换内容仍冲突，整份拒绝且新号不占。
	_, err = s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultFailed, Reason: "破损"},
		{Request: "RC3", Batch: "B1", Parcel: "P001", Result: resultSigned},
	}, tClock(2026, 10, 5, 13, 0))
	if err == nil || !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("已撤销回执换内容应整份拒绝: %v", err)
	}
	if _, ok := s.data.Receipts["RC3"]; ok {
		t.Fatal("整份拒绝时新请求号不得占用")
	}
}

func TestRevokeReceiptPersistence(t *testing.T) {
	s, path := openTempStore(t)
	setupRevokeBase(t, s)
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	t1 := tClock(2026, 10, 5, 11, 0)
	mustRevoke(t, s, "RV1", "RC1", "误录签收", t1)

	// 重启后规则不变：状态、进度、撤销事实及去重结果整体保留。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开台账失败: %v", err)
	}
	p, _ := s2.Query("P001")
	if p.Status != statusDelivering || len(p.Trail) != 4 || p.Trail[3].Op != "撤销回执" {
		t.Fatalf("重开后撤销状态应保留: %+v", p)
	}
	res, replayed, err := s2.RevokeReceipt("RV1", "RC1", "误录签收", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || !res.Time.Equal(t1) {
		t.Fatalf("重开后重放撤销应返回首次结果: %v replayed=%v", err, replayed)
	}
	if _, _, err := s2.RevokeReceipt("RV2", "RC1", "再次撤销", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("重开后仍不得再次撤销同一回执")
	}
	if b, _ := s2.BatchQuery("B1"); b.Effective() != 0 {
		t.Fatalf("重开后有效回执计数应保留: %+v", b)
	}
}

func TestRevokeCorruptLinksRejected(t *testing.T) {
	s, path := openTempStore(t)
	setupRevokeBase(t, s)
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s, "RV1", "RC1", "误录签收", tClock(2026, 10, 5, 11, 0))

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

	// 撤销关联缺失：回执标记了撤销但撤销表为空。
	bad := tamper(func(doc map[string]any) { doc["revokes"] = map[string]any{} })
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("撤销关联缺失应拒绝读写: %v", err)
	}
	// 撤销关联不一致：回执的撤销标记被清除。
	bad = tamper(func(doc map[string]any) {
		doc["receipts"].(map[string]any)["RC1"].(map[string]any)["revokedBy"] = ""
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("撤销标记不一致应拒绝读写: %v", err)
	}
	// 撤销记录指向的原回执不存在。
	bad = tamper(func(doc map[string]any) {
		doc["revokes"].(map[string]any)["RV1"].(map[string]any)["receipt"] = "NOPE"
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("撤销引用不存在的回执应拒绝读写: %v", err)
	}
	// 撤销轨迹缺失。
	bad = tamper(func(doc map[string]any) {
		trail := doc["parcels"].(map[string]any)["P001"].(map[string]any)["trail"].([]any)
		doc["parcels"].(map[string]any)["P001"].(map[string]any)["trail"] = trail[:len(trail)-1]
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("撤销轨迹缺失应拒绝读写: %v", err)
	}
}
