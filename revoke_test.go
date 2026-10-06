package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func mustRevoke(t *testing.T, s *Store, request, receipt, reason string, now time.Time) *RevokeResult {
	t.Helper()
	res, replayed, err := s.Revoke(request, receipt, reason, now)
	if err != nil || replayed {
		t.Fatalf("Revoke(%q) 意外失败: %v replayed=%v", request, err, replayed)
	}
	return res
}

func TestRevokeSignedReceiptSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))

	t2 := tClock(2026, 10, 5, 11, 0)
	res := mustRevoke(t, s, "RV1", "RC1", "误录回执，实际仍在配送", t2)
	if res.Request != "RV1" || res.Receipt != "RC1" || res.Batch != "B1" ||
		res.Parcel != "P001" || res.Reason != "误录回执，实际仍在配送" || !res.Time.Equal(t2) {
		t.Fatalf("撤销结果不符: %+v", res)
	}

	// 该件恢复原批次配送中，站点不变，追加一条可辨认的撤销轨迹。
	p, _ := s.Query("P001")
	if p.Status != statusDelivering || p.Station != "站点A" {
		t.Fatalf("撤销后应恢复原批次配送中、站点不变: %+v", p)
	}
	if len(p.Trail) != 4 {
		t.Fatalf("撤销后应有四条轨迹，得到 %d", len(p.Trail))
	}
	e := p.Trail[3]
	if e.Op != "撤销" || e.Station != "站点A" || e.Batch != "B1" || e.Request != "RV1" ||
		e.RefRequest != "RC1" || e.Reason != "误录回执，实际仍在配送" || !e.Time.Equal(t2) {
		t.Fatalf("撤销轨迹不符: %+v", e)
	}
	// 原回执轨迹永久保留。
	if p.Trail[2].Op != "回执" || p.Trail[2].Request != "RC1" {
		t.Fatalf("原回执轨迹必须保留: %+v", p.Trail[2])
	}

	// 原回执结果保留并标记已撤销，请求号不释放；批次有效回执计数取消。
	rc := s.ReceiptOf("RC1")
	if rc == nil || rc.RevokedBy != "RV1" || rc.Result != resultSigned {
		t.Fatalf("原回执应保留并标记已撤销: %+v", rc)
	}
	b, _ := s.BatchQuery("B1")
	if len(b.Receipts) != 0 || b.Done() {
		t.Fatalf("撤销后批次应无有效回执、恢复配送中: %+v", b.Receipts)
	}
	if b.Courier != "张三" || len(b.Parcels) != 2 || b.Parcels[0] != "P001" {
		t.Fatalf("批次原成员顺序与配送员必须保留: %+v", b)
	}

	// 其他成员不受影响。
	p2, _ := s.Query("P002")
	if p2.Status != statusDelivering || len(p2.Trail) != 2 {
		t.Fatalf("其他成员不得被改变: %+v", p2)
	}
}

func TestRevokeFailedReceiptReopensBatch(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultFailed, "收件人不在", tClock(2026, 10, 5, 10, 0))
	if b, _ := s.BatchQuery("B1"); !b.Done() {
		t.Fatal("全部回执后批次应已完成")
	}
	p, _ := s.Query("P001")
	if p.Status != statusInStation {
		t.Fatalf("失败回执后应恢复在站: %+v", p)
	}

	mustRevoke(t, s, "RV1", "RC1", "误录失败", tClock(2026, 10, 5, 11, 0))

	// 原本已完成的批次恢复配送中，该件恢复配送中、站点仍记出发站。
	if b, _ := s.BatchQuery("B1"); b.Done() || len(b.Receipts) != 0 {
		t.Fatalf("撤销后批次应恢复配送中: %+v", b.Receipts)
	}
	p, _ = s.Query("P001")
	if p.Status != statusDelivering || p.Station != "站点A" {
		t.Fatalf("撤销后应恢复原批次配送中: %+v", p)
	}
}

func TestRevokeReplayAndConflict(t *testing.T) {
	s, path := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s, "P002", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustReceiptOK(t, s, "RC2", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 10, 5))
	t1 := tClock(2026, 10, 5, 11, 0)
	first := mustRevoke(t, s, "RV1", "RC1", "误录回执", t1)

	// 同号、同原回执、清理后同原因重放：返回首次信息和时间，不产生轨迹、不改写台账。
	//（空白清洗在 CLI 层完成，Store 收到的已是清理后的值。）
	before, _ := os.ReadFile(path)
	res, replayed, err := s.Revoke("RV1", "RC1", "误录回执", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed {
		t.Fatalf("同内容重放应成功: %v replayed=%v", err, replayed)
	}
	if res != first || !res.Time.Equal(t1) {
		t.Fatalf("重放应返回首次结果与时间: %+v", res)
	}
	p, _ := s.Query("P001")
	if len(p.Trail) != 4 {
		t.Fatalf("重放不得产生轨迹，得到 %d 条", len(p.Trail))
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("重放不得改写台账")
	}

	// 换内容冲突：换原回执或换原因均拒绝。
	if _, _, err := s.Revoke("RV1", "RC2", "误录回执", tClock(2026, 10, 5, 12, 5)); err == nil {
		t.Fatal("同撤销号换原回执必须冲突")
	}
	if _, _, err := s.Revoke("RV1", "RC1", "另一个原因", tClock(2026, 10, 5, 12, 10)); err == nil {
		t.Fatal("同撤销号换原因必须冲突")
	}

	// 后续重新回执后重放仍成立。
	mustReceiptOK(t, s, "RC3", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 13, 0))
	if _, replayed, err := s.Revoke("RV1", "RC1", "误录回执", tClock(2026, 10, 5, 14, 0)); err != nil || !replayed {
		t.Fatalf("重新回执后同内容重放仍应成立: %v replayed=%v", err, replayed)
	}
}

func TestRevokeReplaySurvivesAbort(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s, "RV1", "RC1", "误录回执", tClock(2026, 10, 5, 11, 0))
	// 撤销后随原批次中止。
	mustAbort(t, s, "A1", "B1", "车辆故障", tClock(2026, 10, 5, 12, 0))

	res, replayed, err := s.Revoke("RV1", "RC1", "误录回执", tClock(2026, 10, 5, 13, 0))
	if err != nil || !replayed || res.Batch != "B1" || res.Parcel != "P001" {
		t.Fatalf("中止后同内容重放仍应成立: %v replayed=%v res=%+v", err, replayed, res)
	}
}

func TestRevokeFailuresAtomic(t *testing.T) {
	s, path := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s, "P002", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s, "RV0", "RC1", "误录回执", tClock(2026, 10, 5, 10, 30))
	mustReceiptOK(t, s, "RC2", "B1", "P002", resultFailed, "收件人不在", tClock(2026, 10, 5, 10, 40))
	// 失败回执后包裹被冻结：原回执不再是最后一条轨迹。
	mustFreeze(t, s, "E1", "P002", "站点A", "外包装破损", tClock(2026, 10, 5, 10, 50))
	// 已中止的批次：P003 已回执（回执保留），P006 未回执被收回。
	mustRegister(t, s, "P003", "站点A", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s, "P006", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B2", "站点A", "李四", []string{"P003", "P006"}, tClock(2026, 10, 5, 9, 40))
	mustReceiptOK(t, s, "RC3", "B2", "P003", resultSigned, "", tClock(2026, 10, 5, 10, 10))
	mustAbort(t, s, "A1", "B2", "整批收回", tClock(2026, 10, 5, 11, 0))
	// 已转交的批次：P004 已回执（回执保留），P005 未回执被转交。
	mustRegister(t, s, "P004", "站点A", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s, "P005", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B3", "站点A", "王五", []string{"P004", "P005"}, tClock(2026, 10, 5, 9, 50))
	mustReceiptOK(t, s, "RC4", "B3", "P004", resultSigned, "", tClock(2026, 10, 5, 10, 20))
	mustTransfer(t, s, "T1", "B3", "B4", "赵六", "原配送员车辆故障", tClock(2026, 10, 5, 11, 30))

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		request string
		receipt string
		reason  string
	}{
		{"原回执不存在", "RV1", "NOPE", "原因"},
		{"已撤销的回执不能再次撤销", "RV2", "RC1", "原因"},
		{"批次已中止", "RV3", "RC3", "原因"},
		{"回执后有冻结", "RV4", "RC2", "原因"},
		{"批次已转交", "RV5", "RC4", "原因"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Revoke(c.request, c.receipt, c.reason, tClock(2026, 10, 5, 12, 0)); err == nil {
				t.Fatal("条件不满足时撤销必须失败")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("失败撤销不得写入数据文件")
			}
			if _, ok := s.data.Revokes[c.request]; ok {
				t.Fatalf("失败的首次撤销不得占用请求号 %q", c.request)
			}
		})
	}
}

func TestRevokeRejectsSubsequentFlow(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultFailed, "收件人不在", tClock(2026, 10, 5, 10, 0))
	// 失败回执后又发生交接：原回执不再是最后一条轨迹。
	mustHandoff(t, s, "H1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 11, 0))

	if _, _, err := s.Revoke("RV1", "RC1", "误录", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("原回执之后发生新流转时必须拒绝撤销")
	}
}

func TestRevokeThenReReceiptAndOldReplay(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s, "RV1", "RC1", "误录回执", tClock(2026, 10, 5, 11, 0))

	// 撤销后可用新回执请求号再次提交签收。
	mustReceiptOK(t, s, "RC2", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 12, 0))
	p, _ := s.Query("P001")
	if p.Status != statusSigned {
		t.Fatalf("重新签收后应为已签收: %+v", p)
	}
	b, _ := s.BatchQuery("B1")
	if !b.Done() || b.Receipts["P001"].Request != "RC2" {
		t.Fatalf("批次应只计新的有效回执: %+v", b.Receipts)
	}

	// 原回执请求号不释放：同内容重放返回原结果并标明已撤销，不恢复回执。
	res, replayed, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 13, 0))
	if err != nil || !replayed {
		t.Fatalf("已撤销回执的同内容重放应成功: %v replayed=%v", err, replayed)
	}
	if res.RevokedBy != "RV1" || !res.Time.Equal(tClock(2026, 10, 5, 10, 0)) {
		t.Fatalf("重放应返回原结果、首次时间并标明已撤销: %+v", res)
	}
	if b2, _ := s.BatchQuery("B1"); b2.Receipts["P001"].Request != "RC2" {
		t.Fatal("已撤销回执的重放不得恢复回执或改变当前状态")
	}
	// 原回执请求号换内容仍冲突。
	if _, _, err := s.Receipt("RC1", "B1", "P001", resultFailed, "改失败", tClock(2026, 10, 5, 13, 5)); err == nil {
		t.Fatal("已撤销回执的请求号换内容必须冲突")
	}
}

func TestRevokeThenAbortIncludesParcel(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s, "P002", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s, "RV1", "RC1", "误录回执", tClock(2026, 10, 5, 11, 0))

	// 撤销后该件恢复未回执，可随原批次中止。
	res := mustAbort(t, s, "A1", "B1", "车辆故障", tClock(2026, 10, 5, 12, 0))
	if len(res.Parcels) != 2 || res.Parcels[0] != "P001" || res.Parcels[1] != "P002" {
		t.Fatalf("撤销后的包裹应随原批次收回: %v", res.Parcels)
	}
}

func TestRevokeThenRelayIncludesParcel(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s, "RV1", "RC1", "误录回执", tClock(2026, 10, 5, 11, 0))

	// 撤销后该件可随原批次续接。
	res := mustTransfer(t, s, "T1", "B1", "B2", "李四", "原配送员车辆故障", tClock(2026, 10, 5, 12, 0))
	if len(res.Parcels) != 1 || res.Parcels[0] != "P001" {
		t.Fatalf("撤销后的包裹应随原批次续接: %v", res.Parcels)
	}
}

func TestRevokeOtherMembersDoNotBlock(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s, "P002", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	// 其他成员的后续操作（回执、冻结、解除）不阻止对 P001 回执的撤销。
	mustReceiptOK(t, s, "RC2", "B1", "P002", resultFailed, "收件人不在", tClock(2026, 10, 5, 10, 10))
	mustFreeze(t, s, "E1", "P002", "站点A", "外包装破损", tClock(2026, 10, 5, 10, 20))
	if _, _, err := s.Unfreeze("U1", "E1", "已核实放行", tClock(2026, 10, 5, 10, 30)); err != nil {
		t.Fatal(err)
	}

	mustRevoke(t, s, "RV1", "RC1", "误录回执", tClock(2026, 10, 5, 11, 0))
}

func TestRevokePersistsAcrossReopen(t *testing.T) {
	s, path := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	t1 := tClock(2026, 10, 5, 11, 0)
	mustRevoke(t, s, "RV1", "RC1", "误录回执", t1)

	// 重启后规则不变：状态、进度、撤销事实与去重结果完整恢复。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开台账失败: %v", err)
	}
	p, _ := s2.Query("P001")
	if p.Status != statusDelivering || len(p.Trail) != 4 || p.Trail[3].Op != "撤销" {
		t.Fatalf("重开后状态与轨迹应完整: %+v", p)
	}
	if rc := s2.ReceiptOf("RC1"); rc == nil || rc.RevokedBy != "RV1" {
		t.Fatalf("重开后撤销标记应保留: %+v", rc)
	}
	res, replayed, err := s2.Revoke("RV1", "RC1", "误录回执", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || !res.Time.Equal(t1) {
		t.Fatalf("重开后同内容重放应返回首次结果: %v replayed=%v res=%+v", err, replayed, res)
	}
	if _, _, err := s2.Revoke("RV2", "RC1", "再次撤销", tClock(2026, 10, 5, 12, 5)); err == nil {
		t.Fatal("重开后换撤销号再次撤销同一回执必须拒绝")
	}
}

func TestRevokeCorruptLedgerRejected(t *testing.T) {
	s, path := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s, "RV1", "RC1", "误录回执", tClock(2026, 10, 5, 11, 0))
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		mutate func(l *ledgerFile)
	}{
		{"撤销记录缺失", func(l *ledgerFile) { delete(l.Revokes, "RV1") }},
		{"回执撤销标记缺失", func(l *ledgerFile) { l.Receipts["RC1"].RevokedBy = "" }},
		{"撤销记录与原回执不一致", func(l *ledgerFile) { l.Revokes["RV1"].Receipt = "RC9" }},
		{"撤销记录与批次不一致", func(l *ledgerFile) { l.Revokes["RV1"].Batch = "B9" }},
		{"撤销轨迹缺失", func(l *ledgerFile) {
			p := l.Parcels["P001"]
			p.Trail = p.Trail[:3]
		}},
		{"已撤销回执仍计为有效", func(l *ledgerFile) {
			b := l.Batches["B1"]
			b.Receipts["P001"] = &ReceiptEntry{Request: "RC1", Result: resultSigned, Time: tClock(2026, 10, 5, 10, 0)}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 用被篡改的内存状态覆盖数据文件，再重开必须明确拒绝。
			bad, err := Open(path)
			if err != nil {
				t.Fatalf("基线台账应有效: %v", err)
			}
			c.mutate(&bad.data)
			if err := bad.save(); err != nil {
				t.Fatalf("写入篡改台账失败: %v", err)
			}
			if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("撤销关联不一致应拒绝读写，得到 %v", err)
			}
			if err := os.WriteFile(path, good, 0o644); err != nil {
				t.Fatal(err)
			}
			// 恢复后旧有效台账直接可用。
			if _, err := Open(path); err != nil {
				t.Fatalf("恢复后台账应可用: %v", err)
			}
		})
	}
}

func TestRevokeImportReplayOfRevokedReceipt(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s, "P002", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRevoke(t, s, "RV1", "RC1", "误录回执", tClock(2026, 10, 5, 11, 0))

	// 导入混合已撤销回执的重放与新回执：重放标明已撤销，新回执正常受理。
	items, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultSigned},
		{Request: "RC2", Batch: "B1", Parcel: "P002", Result: resultFailed, Reason: "收件人不在"},
	}, tClock(2026, 10, 5, 12, 0))
	if err != nil {
		t.Fatalf("混合导入应成功: %v", err)
	}
	if !items[0].Replayed || !items[0].Revoked {
		t.Fatalf("已撤销回执的导入重放应标明已撤销: %+v", items[0])
	}
	if items[1].Replayed || items[1].Revoked {
		t.Fatalf("新回执应为新增: %+v", items[1])
	}
	// 进度只计未撤销结果。
	b, _ := s.BatchQuery("B1")
	if len(b.Receipts) != 1 || b.Receipts["P002"] == nil {
		t.Fatalf("批次进度应只计未撤销结果: %+v", b.Receipts)
	}
	// 已撤销回执的导入重放不恢复回执。
	if _, ok := b.Receipts["P001"]; ok {
		t.Fatal("已撤销回执不得因导入重放而恢复")
	}
}

func TestCLIReceiptRevokeEndToEnd(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"

	if _, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A"); code != 0 {
		t.Fatal("登记失败")
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A", "--courier", "张三", "--parcel", "P001"); code != 0 {
		t.Fatal("出站失败")
	}
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001", "--result", "签收"); code != 0 {
		t.Fatal("回执失败")
	}

	// 参数清洗：空白拒绝，退出码 1。
	if _, _, code := runCLI(t, dbPath, "receipt-revoke", "--request", " ", "--receipt", "RC1", "--reason", "误录"); code != exitBusiness {
		t.Fatalf("空白撤销请求号应以 1 退出，得到 %d", code)
	}

	out, _, code := runCLI(t, dbPath, "receipt-revoke", "--request", "RV1", "--receipt", "RC1", "--reason", "误录回执，实际仍在配送")
	if code != 0 || !strings.Contains(out, "撤销成功") ||
		!strings.Contains(out, "撤销请求号: RV1") || !strings.Contains(out, "原回执请求号: RC1") ||
		!strings.Contains(out, "批次号: B1") || !strings.Contains(out, "包裹编号: P001") ||
		!strings.Contains(out, "撤销原因: 误录回执，实际仍在配送") {
		t.Fatalf("撤销输出不符: code=%d out=%s", code, out)
	}

	// 同内容重放：标明重复提交，退出码 0。
	out, _, code = runCLI(t, dbPath, "receipt-revoke", "--request", "RV1", "--receipt", "RC1", "--reason", "误录回执，实际仍在配送")
	if code != 0 || !strings.Contains(out, "重复提交") {
		t.Fatalf("重放输出不符: code=%d out=%s", code, out)
	}

	// query：回执记录标明已撤销，撤销记录可辨认。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "已撤销（撤销请求号: RV1）") ||
		!strings.Contains(out, "操作: 撤销") || !strings.Contains(out, "原回执请求号: RC1") ||
		!strings.Contains(out, "当前状态: 配送中") {
		t.Fatalf("query 应区分有效回执与撤销历史: code=%d out=%s", code, out)
	}

	// batch：恢复配送中，撤销历史与有效回执分列。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 配送中") ||
		!strings.Contains(out, "0/1 已回执") || !strings.Contains(out, "撤销历史") ||
		!strings.Contains(out, "原回执请求号: RC1") || !strings.Contains(out, "撤销请求号: RV1") {
		t.Fatalf("batch 应展示撤销历史: code=%d out=%s", code, out)
	}

	// receipt 对已撤销回执的同内容重放：返回原结果并标明已撤销。
	out, _, code = runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001", "--result", "签收")
	if code != 0 || !strings.Contains(out, "重复提交") || !strings.Contains(out, "已撤销（撤销请求号: RV1）") {
		t.Fatalf("已撤销回执的重放应标明已撤销: code=%d out=%s", code, out)
	}

	// 撤销后可再次回执。
	out, _, code = runCLI(t, dbPath, "receipt", "--request", "RC2", "--batch", "B1", "--parcel", "P001", "--result", "签收")
	if code != 0 || !strings.Contains(out, "回执成功") {
		t.Fatalf("撤销后应可再次回执: code=%d out=%s", code, out)
	}
}
