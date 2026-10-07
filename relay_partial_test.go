package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func mustTransferParcels(t *testing.T, s *Store, request, fromBatch, toBatch, courier, reason string, parcels []string, now time.Time) *TransferResult {
	t.Helper()
	res, replayed, err := s.Transfer(request, fromBatch, toBatch, courier, reason, parcels, now)
	if err != nil || replayed {
		t.Fatalf("Transfer(%q) 意外失败: %v replayed=%v", request, err, replayed)
	}
	return res
}

// 准备一个在站点A 出发、张三配送的批次 B1（P001、P002、P003、P004），均未回执。
func setupRelayPartialBase(t *testing.T, s *Store) {
	t.Helper()
	t1 := tClock(2026, 10, 5, 9, 0)
	for _, id := range []string{"P001", "P002", "P003", "P004"} {
		mustRegister(t, s, id, "站点A", t1)
	}
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002", "P003", "P004"}, tClock(2026, 10, 5, 9, 30))
}

func TestTransferPartialSequence(t *testing.T) {
	s, _ := openTempStore(t)
	setupRelayPartialBase(t, s)

	// 第一次分批续接：显式选 P002，集合按原成员顺序保存，原批次仍配送中。
	t1 := tClock(2026, 10, 5, 10, 0)
	res := mustTransferParcels(t, s, "T1", "B1", "B2", "李四", "分批换配送员", []string{"P002"}, t1)
	if !res.Explicit || len(res.Parcels) != 1 || res.Parcels[0] != "P002" {
		t.Fatalf("显式分批续接结果不符: %+v", res)
	}
	old, _ := s.BatchQuery("B1")
	if old.TransferredBy != "" || len(old.Relays) != 1 || old.Relays[0] != "T1" {
		t.Fatalf("分批续接后原批次应仍开放且记录一次续接: %+v", old)
	}
	if got := s.pendingParcels(old); len(got) != 3 || got[0] != "P001" || got[1] != "P003" || got[2] != "P004" {
		t.Fatalf("余件应为 P001、P003、P004: %v", got)
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusDelivering || p2.Station != "站点A" || currentBatch(p2) != "B2" {
		t.Fatalf("转交件应保持配送中、站点不变并切换到新批次: %+v", p2)
	}
	nb, _ := s.BatchQuery("B2")
	if nb.RelayedFrom != "B1" || nb.RelayRequest != "T1" || nb.Courier != "李四" ||
		len(nb.Parcels) != 1 || nb.Parcels[0] != "P002" || !nb.Time.Equal(t1) {
		t.Fatalf("新批次不符: %+v", nb)
	}

	// 第二次分批续接：显式集合换序给出，结果仍按原成员顺序。
	res2 := mustTransferParcels(t, s, "T2", "B1", "B3", "王五", "再次分批", []string{"P004", "P001"}, tClock(2026, 10, 5, 11, 0))
	if len(res2.Parcels) != 2 || res2.Parcels[0] != "P001" || res2.Parcels[1] != "P004" {
		t.Fatalf("显式集合应按原成员顺序保存: %v", res2.Parcels)
	}
	old, _ = s.BatchQuery("B1")
	if old.TransferredBy != "" || len(old.Relays) != 2 || old.Relays[1] != "T2" {
		t.Fatalf("两次分批续接后原批次应仍开放: %+v", old)
	}
	if got := s.pendingParcels(old); len(got) != 1 || got[0] != "P003" {
		t.Fatalf("余件应只剩 P003: %v", got)
	}

	// 第三次不选成员：取当时全部余件，转走最后待配送件，原批次永久关闭为已转交。
	res3 := mustTransferParcels(t, s, "T3", "B1", "B4", "赵六", "转走余件", nil, tClock(2026, 10, 5, 12, 0))
	if res3.Explicit || len(res3.Parcels) != 1 || res3.Parcels[0] != "P003" {
		t.Fatalf("不选成员应取当时全部余件: %+v", res3)
	}
	old, _ = s.BatchQuery("B1")
	if old.TransferredBy != "T3" || len(old.Relays) != 3 || old.Relays[2] != "T3" {
		t.Fatalf("转走最后待配送件后原批次应永久关闭: %+v", old)
	}
	if s.BatchTransferSource("B1") != res3 {
		t.Fatal("BatchTransferSource 应返回关闭批次的续接结果")
	}
	trs := s.BatchTransfers("B1")
	if len(trs) != 3 || trs[0].Request != "T1" || trs[1].Request != "T2" || trs[2].Request != "T3" {
		t.Fatalf("BatchTransfers 应按提交顺序返回各次续接: %+v", trs)
	}
	if s.ParcelTransfer("B1", "P001") != trs[1] || s.ParcelTransfer("B1", "P002") != trs[0] ||
		s.ParcelTransfer("B1", "P003") != trs[2] || s.ParcelTransfer("B1", "P004") != trs[1] {
		t.Fatal("ParcelTransfer 应返回各件实际转交去向")
	}

	// 已永久转交的批次不能再次续接或中止。
	if _, _, err := s.Transfer("T9", "B1", "B9", "孙七", "原因", nil, tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("已永久转交批次不能再次续接")
	}
	if _, _, err := s.Abort("A9", "B1", "原因", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("已永久转交批次不能中止")
	}
}

func TestTransferPartialRemainingWork(t *testing.T) {
	s, _ := openTempStore(t)
	setupRelayPartialBase(t, s)
	mustTransferParcels(t, s, "T1", "B1", "B2", "李四", "分批", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))

	// 保留件仍可按原条件回执；余件经回执办结时批次已完成。
	mustReceiptOK(t, s, "RC3", "B1", "P003", resultSigned, "", tClock(2026, 10, 5, 11, 0))
	mustReceiptOK(t, s, "RC4", "B1", "P004", resultFailed, "收件人不在", tClock(2026, 10, 5, 11, 30))
	old, _ := s.BatchQuery("B1")
	if got := s.pendingParcels(old); len(got) != 0 {
		t.Fatalf("余件全部回执后不应再有待配送件: %v", got)
	}
	if old.TransferredBy != "" {
		t.Fatal("余件经回执办结时应为已完成而非已转交")
	}

	// 失败回执可撤销恢复配送，撤销件可再次回执或续接。
	if _, _, err := s.RevokeReceipt("RV1", "RC4", "误录失败，实际仍配送中", tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatalf("已完成批次可按原条件撤销回执: %v", err)
	}
	p4, _ := s.Query("P004")
	if p4.Status != statusDelivering || currentBatch(p4) != "B1" {
		t.Fatalf("撤销后该件应恢复原批次配送中: %+v", p4)
	}
	// 撤销件随原批次再次分批续接。
	res := mustTransferParcels(t, s, "T2", "B1", "B3", "王五", "撤销后转交", []string{"P004"}, tClock(2026, 10, 5, 12, 30))
	if len(res.Parcels) != 1 || res.Parcels[0] != "P004" {
		t.Fatalf("撤销件应可再次续接: %+v", res)
	}
	old, _ = s.BatchQuery("B1")
	if old.TransferredBy != "T2" {
		t.Fatal("转走最后待配送件后原批次应永久关闭为已转交")
	}
	// 已回执、已转交件的后续作业不妨碍：新批次可回执。
	mustReceiptOK(t, s, "RC5", "B2", "P001", resultSigned, "", tClock(2026, 10, 5, 13, 0))
	mustReceiptOK(t, s, "RC6", "B3", "P004", resultSigned, "", tClock(2026, 10, 5, 13, 30))
}

func TestTransferPartialAbort(t *testing.T) {
	s, _ := openTempStore(t)
	setupRelayPartialBase(t, s)
	mustTransferParcels(t, s, "T1", "B1", "B2", "李四", "分批", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	mustReceiptOK(t, s, "RC2", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 10, 30))

	// 中止只收回无有效回执且未转交的配送中件。
	ab := mustAbort(t, s, "A1", "B1", "车辆故障", tClock(2026, 10, 5, 11, 0))
	if len(ab.Parcels) != 2 || ab.Parcels[0] != "P003" || ab.Parcels[1] != "P004" {
		t.Fatalf("中止应只收回未回执且未转交件: %v", ab.Parcels)
	}
	p1, _ := s.Query("P001")
	if p1.Status != statusDelivering || currentBatch(p1) != "B2" {
		t.Fatalf("已转交件不受中止影响: %+v", p1)
	}
	// 重启后校验仍通过（中止集合排除转交件）。
	s2, err := Open(s.path)
	if err != nil {
		t.Fatalf("重开台账失败: %v", err)
	}
	if got := s2.data.Aborts["A1"]; got == nil || len(got.Parcels) != 2 {
		t.Fatalf("重启后中止记录应保持: %+v", got)
	}
}

func TestTransferPartialReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	setupRelayPartialBase(t, s)
	t1 := tClock(2026, 10, 5, 10, 0)
	first := mustTransferParcels(t, s, "T1", "B1", "B2", "李四", "分批", []string{"P001", "P003"}, t1)

	// 后续作业：新批次回执、原批次再次分批续接。
	mustReceiptOK(t, s, "RC1", "B2", "P001", resultSigned, "", tClock(2026, 10, 5, 11, 0))
	mustTransferParcels(t, s, "T2", "B1", "B3", "王五", "再次分批", []string{"P002"}, tClock(2026, 10, 5, 11, 30))

	// 显式集合换序重放：返回首次信息、集合和时间，不检查现状、不改写台账。
	before, _ := os.ReadFile(s.path)
	got, replayed, err := s.Transfer("T1", "B1", "B2", "李四", "分批", []string{"P003", "P001"}, tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || got != first {
		t.Fatalf("显式集合换序重放应返回首次结果: replayed=%v err=%v got=%+v", replayed, err, got)
	}
	if len(got.Parcels) != 2 || got.Parcels[0] != "P001" || got.Parcels[1] != "P003" || !got.Time.Equal(t1) {
		t.Fatalf("重放应返回首次集合与时间: %+v", got)
	}
	after, _ := os.ReadFile(s.path)
	if string(before) != string(after) {
		t.Fatal("重放不得改写台账")
	}

	// 同号换选择方式或显式集合：冲突。
	if _, _, err := s.Transfer("T1", "B1", "B2", "李四", "分批", nil, tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("同号换选择方式（不选成员）应报冲突")
	}
	if _, _, err := s.Transfer("T1", "B1", "B2", "李四", "分批", []string{"P001"}, tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("同号换显式集合应报冲突")
	}
	// 不选方式的续接同号换显式方式也冲突。
	if _, _, err := s.Transfer("T3", "B1", "B4", "赵六", "整批", nil, tClock(2026, 10, 5, 12, 30)); err != nil {
		t.Fatalf("不选方式续接失败: %v", err)
	}
	if _, _, err := s.Transfer("T3", "B1", "B4", "赵六", "整批", []string{"P004"}, tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatal("不选方式同号换显式集合应报冲突")
	}
}

func TestTransferPartialFailures(t *testing.T) {
	s, path := openTempStore(t)
	setupRelayPartialBase(t, s)
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustTransferParcels(t, s, "T0", "B1", "B9", "李四", "先行分批", []string{"P002"}, tClock(2026, 10, 5, 10, 30))

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now := tClock(2026, 10, 5, 11, 0)
	cases := []struct {
		name    string
		parcels []string
	}{
		{"显式集合为空", []string{}},
		{"显式集合重复", []string{"P003", "P003"}},
		{"选中件不属于原批次", []string{"P003", "P009"}},
		{"选中件已有有效回执", []string{"P001"}},
		{"选中件已随前次续接转交", []string{"P002"}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := "TF" + string(rune('1'+i))
			if _, _, err := s.Transfer(req, "B1", "B8", "王五", "原因", c.parcels, now); err == nil {
				t.Fatal("条件不满足时续接必须失败")
			}
			if _, ok := s.data.Transfers[req]; ok {
				t.Fatalf("失败的首次续接不得占用请求号 %q", req)
			}
			if _, ok := s.data.Batches["B8"]; ok {
				t.Fatal("失败的首次续接不得占用新批次号")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("失败续接不得写入数据文件")
			}
		})
	}
	// 防御性校验：选中件不在原批次配送中时整次拒绝（公开操作流程不可达）。
	p3 := s.data.Parcels["P003"]
	oldStatus := p3.Status
	p3.Status = statusInStation
	if _, _, err := s.Transfer("TF9", "B1", "B8", "王五", "原因", []string{"P003"}, now); err == nil {
		t.Fatal("选中件不在原批次配送中时必须整次拒绝")
	}
	p3.Status = oldStatus
	if _, ok := s.data.Transfers["TF9"]; ok {
		t.Fatal("失败的首次续接不得占用请求号")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("失败续接不得写入数据文件")
	}
}

func TestTransferPartialReceiptImport(t *testing.T) {
	s, _ := openTempStore(t)
	setupRelayPartialBase(t, s)
	mustTransferParcels(t, s, "T1", "B1", "B2", "李四", "分批", []string{"P001"}, tClock(2026, 10, 5, 10, 0))

	// receipt-import 含转交件的迟到回执仍整份拒绝、仅撤销此次新增。
	_, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC1", Batch: "B1", Parcel: "P002", Result: resultSigned},
		{Request: "RC2", Batch: "B1", Parcel: "P001", Result: resultSigned},
	}, tClock(2026, 10, 5, 11, 0))
	if err == nil || !strings.Contains(err.Error(), "第 2 条") {
		t.Fatalf("含转交件回执的导入应整份拒绝并提示位置: %v", err)
	}
	if _, ok := s.data.Receipts["RC1"]; ok {
		t.Fatal("整份拒绝时此次新增必须撤销，新号不占用")
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusDelivering || currentBatch(p2) != "B1" {
		t.Fatalf("整份拒绝后台账保持导入前状态: %+v", p2)
	}
	// 保留件可正常导入回执。
	items, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC3", Batch: "B1", Parcel: "P002", Result: resultSigned},
	}, tClock(2026, 10, 5, 11, 30))
	if err != nil || len(items) != 1 || items[0].Replayed {
		t.Fatalf("保留件应可导入回执: items=%+v err=%v", items, err)
	}
}

func TestTransferPartialPersistenceAndCorrupt(t *testing.T) {
	s, path := openTempStore(t)
	setupRelayPartialBase(t, s)
	mustTransferParcels(t, s, "T1", "B1", "B2", "李四", "分批一", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	mustTransferParcels(t, s, "T2", "B1", "B3", "王五", "分批二", []string{"P002"}, tClock(2026, 10, 5, 11, 0))

	// 重启后规则不变：重放仍返回首次结果，余件可继续分批续接。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开台账失败: %v", err)
	}
	if got, replayed, err := s2.Transfer("T1", "B1", "B2", "李四", "分批一", []string{"P001"}, tClock(2026, 10, 5, 12, 0)); err != nil || !replayed ||
		len(got.Parcels) != 1 || got.Parcels[0] != "P001" {
		t.Fatalf("重启后重放应返回首次结果: replayed=%v err=%v got=%+v", replayed, err, got)
	}
	res := mustTransferParcels(t, s2, "T3", "B1", "B4", "赵六", "余件", nil, tClock(2026, 10, 5, 12, 30))
	if len(res.Parcels) != 2 || res.Parcels[0] != "P003" || res.Parcels[1] != "P004" {
		t.Fatalf("重启后余件续接集合不符: %+v", res)
	}
	if old := s2.data.Batches["B1"]; old.TransferredBy != "T3" || len(old.Relays) != 3 {
		t.Fatalf("重启后连续分批应正确关闭原批次: %+v", old)
	}

	// 同件从同批次重复转交：重载必须拒绝，不崩溃或覆盖。
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["transfers"].(map[string]any)["T2"].(map[string]any)["parcels"] = []any{"P001"}
	tampered, _ := json.Marshal(doc)
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("同件从同批次重复转交应拒绝读写: %v", err)
	}
	// 续接列表与续接结果矛盾（批次缺少续接列表条目）也应判为损坏。
	doc = nil
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["batches"].(map[string]any)["B1"].(map[string]any)["relays"] = []any{"T1"}
	tampered, _ = json.Marshal(doc)
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("续接列表与续接结果矛盾应拒绝读写: %v", err)
	}
	// 原数据文件未被覆盖。
	if got, _ := os.ReadFile(path); string(got) == string(raw) {
		t.Fatal("测试前置：篡改应已写入")
	}
}

func TestTransferLegacyDataCompat(t *testing.T) {
	s, path := openTempStore(t)
	setupRelayBase(t, s) // B1：P001 已签收，P002、P003 配送中
	mustTransfer(t, s, "T1", "B1", "B2", "李四", "车辆故障", tClock(2026, 10, 5, 11, 0))

	// 模拟旧版数据文件：批次没有 relays 字段、续接结果没有 explicit 字段。
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	b1 := doc["batches"].(map[string]any)["B1"].(map[string]any)
	if _, ok := b1["relays"]; !ok {
		t.Fatal("测试前置：新格式应写出 relays 字段")
	}
	delete(b1, "relays")
	if _, ok := doc["transfers"].(map[string]any)["T1"].(map[string]any)["explicit"]; ok {
		t.Fatal("测试前置：不选方式不应写出 explicit 字段")
	}
	legacy, _ := json.Marshal(doc)
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatal(err)
	}

	// 旧有效台账直接使用：已转交批次按仅含关闭续接一次处理。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("旧数据文件应直接打开: %v", err)
	}
	old, _ := s2.BatchQuery("B1")
	if old.TransferredBy != "T1" || len(old.Relays) != 1 || old.Relays[0] != "T1" {
		t.Fatalf("旧数据应按一次关闭续接处理: %+v", old)
	}
	if trs := s2.BatchTransfers("B1"); len(trs) != 1 || trs[0].Request != "T1" || trs[0].Explicit {
		t.Fatalf("旧续接应按不选方式处理: %+v", trs)
	}
	// 旧续接按不选方式重放；换显式方式报冲突。
	if _, replayed, err := s2.Transfer("T1", "B1", "B2", "李四", "车辆故障", nil, tClock(2026, 10, 5, 12, 0)); err != nil || !replayed {
		t.Fatalf("旧续接同内容重放应返回首次结果: replayed=%v err=%v", replayed, err)
	}
	if _, _, err := s2.Transfer("T1", "B1", "B2", "李四", "车辆故障", []string{"P002", "P003"}, tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("旧续接换显式方式应报冲突")
	}
	// 旧台账上的后续作业与保存照常。
	if _, _, err := s2.Receipt("RC2", "B2", "P002", resultSigned, "", tClock(2026, 10, 5, 12, 30)); err != nil {
		t.Fatalf("旧台账后续作业应正常: %v", err)
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("旧台账保存后重载应正常: %v", err)
	}
}

func TestCLIEndToEndRelayPartial(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"

	for _, id := range []string{"P001", "P002", "P003"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败: code=%d", id, code)
		}
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A", "--courier", "张三",
		"--parcel", "P001", "--parcel", "P002", "--parcel", "P003"); code != 0 {
		t.Fatalf("出站失败: code=%d", code)
	}

	// 分批续接：显式选件（换序给出），成功结果保留本次集合与时间。
	out, _, code := runCLI(t, dbPath, "relay", "--request", "T1", "--from", "B1", "--to", "B2",
		"--courier", "李四", "--reason", "分批换配送员", "--parcel", "P003", "--parcel", "P001")
	if code != 0 || !strings.Contains(out, "续接成功") || !strings.Contains(out, "转交包裹（2 件）:") ||
		!strings.Contains(out, "接手时间:") ||
		strings.Index(out, "P001") > strings.Index(out, "P003") {
		t.Fatalf("分批续接输出不符（应按原成员顺序列出集合）: code=%d out=%s", code, out)
	}

	// 显式集合换序重放：返回首次结果，不追加轨迹。
	out, _, code = runCLI(t, dbPath, "relay", "--request", "T1", "--from", "B1", "--to", "B2",
		"--courier", "李四", "--reason", "分批换配送员", "--parcel", "P001", "--parcel", "P003")
	if code != 0 || !strings.Contains(out, "重复提交") {
		t.Fatalf("显式集合换序重放应返回首次结果: code=%d out=%s", code, out)
	}
	// 同号换选择方式冲突。
	_, errText, code := runCLI(t, dbPath, "relay", "--request", "T1", "--from", "B1", "--to", "B2",
		"--courier", "李四", "--reason", "分批换配送员")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("同号换选择方式应报冲突(1): code=%d err=%s", code, errText)
	}

	// batch 区分待配送与各次转交去向，转交不计回执。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 配送中") ||
		!strings.Contains(out, "转交去向: 批次 B2") ||
		!strings.Contains(out, "P001    已转交    去向批次: B2") ||
		!strings.Contains(out, "P003    已转交    去向批次: B2") ||
		!strings.Contains(out, "P002    未回执") {
		t.Fatalf("原批次应区分待配送与转交去向: code=%d out=%s", code, out)
	}

	// 保留件仍可回执。
	if out, _, code := runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P002",
		"--result", "签收"); code != 0 || !strings.Contains(out, "回执成功") {
		t.Fatalf("保留件应可回执: code=%d out=%s", code, out)
	}
	// 余件经回执办结时已完成。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 已完成") {
		t.Fatalf("余件办结后原批次应已完成: code=%d out=%s", code, out)
	}
	// 撤销恢复配送后可再次续接，转走最后待配送件时永久关闭。
	if _, _, code := runCLI(t, dbPath, "receipt-revoke", "--request", "RV1", "--receipt", "RC1",
		"--reason", "误录签收"); code != 0 {
		t.Fatalf("撤销回执失败: code=%d", code)
	}
	out, _, code = runCLI(t, dbPath, "relay", "--request", "T2", "--from", "B1", "--to", "B3",
		"--courier", "王五", "--reason", "转走余件")
	if code != 0 || !strings.Contains(out, "转交包裹（1 件）:") || !strings.Contains(out, "P002") {
		t.Fatalf("不选成员应取当时全部余件: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 已转交") ||
		!strings.Contains(out, "转交去向: 批次 B2") || !strings.Contains(out, "转交去向: 批次 B3") ||
		!strings.Contains(out, "P002    已转交    去向批次: B3") {
		t.Fatalf("原批次应永久关闭并展示各次转交去向: code=%d out=%s", code, out)
	}
	// query 对配送中件展示当前批次、配送员及完整续接轨迹。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P002")
	if code != 0 || !strings.Contains(out, "当前状态: 配送中") ||
		!strings.Contains(out, "当前批次: B3") || !strings.Contains(out, "当前配送员: 王五") ||
		!strings.Contains(out, "操作: 撤销回执") || !strings.Contains(out, "操作: 续接") {
		t.Fatalf("query 应展示当前批次/配送员与完整轨迹: code=%d out=%s", code, out)
	}
}
