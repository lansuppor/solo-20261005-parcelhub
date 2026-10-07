package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 准备一个在站点A 出发、张三配送的批次 B1（P001、P002、P003、P004），
// 其中 P001 已签收；待配送件为 P002、P003、P004。
func setupPartialRelayBase(t *testing.T, s *Store) {
	t.Helper()
	t1 := tClock(2026, 10, 5, 9, 0)
	for _, id := range []string{"P001", "P002", "P003", "P004"} {
		mustRegister(t, s, id, "站点A", t1)
	}
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002", "P003", "P004"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
}

// 连续分批续接：按原成员顺序以选中件创建新批次，原批次有余件时保持配送中，
// 续接转走最后一件待配送件时才永久关闭为已转交；未选件不变。
func TestTransferPartialConsecutive(t *testing.T) {
	s, _ := openTempStore(t)
	setupPartialRelayBase(t, s)

	// 第一次显式选中 P003、P002（提交乱序）：按原成员顺序保存为 P002、P003。
	t1 := tClock(2026, 10, 5, 11, 0)
	res := mustTransferParcels(t, s, "T1", "B1", "B2", "李四", "分批换班", []string{"P003", "P002"}, t1)
	if !res.Explicit {
		t.Fatalf("显式续接应标记选择方式: %+v", res)
	}
	if !sameOrder(res.Parcels, []string{"P002", "P003"}) {
		t.Fatalf("本次集合应按原成员顺序: %v", res.Parcels)
	}
	old, _ := s.BatchQuery("B1")
	if old.TransferredBy != "" {
		t.Fatalf("仍有余件时原批次不应永久转交: %+v", old)
	}
	if !sameOrder(s.batchOpen(old), []string{"P004"}) {
		t.Fatalf("余件应为 P004: %v", s.batchOpen(old))
	}
	// 选中件切换批次与配送员、追加续接轨迹，保持配送中与站点。
	for _, pid := range []string{"P002", "P003"} {
		p, _ := s.Query(pid)
		if p.Status != statusDelivering || p.Station != "站点A" || currentBatch(p) != "B2" {
			t.Fatalf("%s 应在 B2 由李四配送中: %+v", pid, p)
		}
		last := p.Trail[len(p.Trail)-1]
		if last.Op != "续接" || last.FromBatch != "B1" || last.Batch != "B2" ||
			last.Courier != "李四" || last.Request != "T1" || !last.Time.Equal(t1) {
			t.Fatalf("%s 续接轨迹不符: %+v", pid, last)
		}
	}
	// 未选余件 P004 不变：仍在 B1、无续接轨迹。
	p4, _ := s.Query("P004")
	if p4.Status != statusDelivering || currentBatch(p4) != "B1" || len(p4.Trail) != 2 {
		t.Fatalf("未选余件应保持原批次配送中且无续接轨迹: %+v", p4)
	}
	// 新批次 B2 仅含选中件。
	nb, _ := s.BatchQuery("B2")
	if !sameOrder(nb.Parcels, []string{"P002", "P003"}) || nb.Courier != "李四" ||
		nb.RelayedFrom != "B1" || nb.RelayRequest != "T1" {
		t.Fatalf("新批次 B2 不符: %+v", nb)
	}

	// 第二次续接转走最后一件待配送件 P004：原批次永久关闭为已转交。
	t2 := tClock(2026, 10, 5, 12, 0)
	res2 := mustTransferParcels(t, s, "T2", "B1", "B3", "王五", "再次分批", []string{"P004"}, t2)
	if !sameOrder(res2.Parcels, []string{"P004"}) {
		t.Fatalf("第二次集合应为 P004: %v", res2.Parcels)
	}
	old, _ = s.BatchQuery("B1")
	if old.TransferredBy != "T2" {
		t.Fatalf("转走最后待配送件后原批次应永久转交: %+v", old)
	}
	if tr := s.BatchTransferSource("B1"); tr != res2 {
		t.Fatal("BatchTransferSource 应返回最后一次（永久关闭）续接")
	}
	all := s.BatchTransfers("B1")
	if len(all) != 2 || all[0].Request != "T1" || all[1].Request != "T2" {
		t.Fatalf("应按提交顺序保留各次续接: %+v", all)
	}
	if p, _ := s.Query("P004"); currentBatch(p) != "B3" {
		t.Fatal("P004 当前配送归属应切换到 B3")
	}
	// 已永久转交后不能再次续接或中止。
	if _, _, err := s.Transfer("T9", "B1", "B9", "赵六", "原因", nil, tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("已永久转交批次不能再次续接")
	}
	if _, _, err := s.Abort("A1", "B1", "原因", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("已永久转交批次不能中止")
	}
}

// 余件继续原批次作业：可回执（办结则原批次已完成）、可撤销恢复配送、可中止；
// 已转交件的后续作业不妨碍余件，中止只收回仍在原批次配送的待配送件。
func TestTransferPartialRemainingWork(t *testing.T) {
	// 场景一：余件全部回执办结，原批次状态为已完成（非永久转交、非中止）。
	s, _ := openTempStore(t)
	setupPartialRelayBase(t, s)
	mustTransferParcels(t, s, "T1", "B1", "B2", "李四", "分批", []string{"P003"}, tClock(2026, 10, 5, 11, 0))
	mustReceiptOK(t, s, "RC2", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 11, 30))
	mustReceiptOK(t, s, "RC3", "B1", "P004", resultFailed, "收件人不在", tClock(2026, 10, 5, 11, 40))
	old, _ := s.BatchQuery("B1")
	if old.TransferredBy != "" || old.AbortedBy != "" {
		t.Fatalf("余件回执办结不应标记转交或中止: %+v", old)
	}
	if len(s.batchOpen(old)) != 0 {
		t.Fatalf("余件办结后应无待配送件: %v", s.batchOpen(old))
	}
	// 无待配送件：不能再次续接或中止。
	if _, _, err := s.Transfer("T8", "B1", "B8", "王五", "原因", nil, tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("无待配送件时不能续接")
	}
	if _, _, err := s.Abort("A1", "B1", "原因", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("无待配送件时不能中止")
	}
	// 已转交件 P003 在新批次仍可回执，不影响旧批次已完成的事实。
	mustReceiptOK(t, s, "RC4", "B2", "P003", resultSigned, "", tClock(2026, 10, 5, 12, 30))
	// P004 失败回站后可再次出站。
	mustDispatch(t, s, "B5", "站点A", "张三", []string{"P004"}, tClock(2026, 10, 5, 13, 0))

	// 场景二：余件撤销回执后恢复原批次配送，可再次回执或续接。
	s2, _ := openTempStore(t)
	setupPartialRelayBase(t, s2)
	mustTransferParcels(t, s2, "T1", "B1", "B2", "李四", "分批", []string{"P003"}, tClock(2026, 10, 5, 11, 0))
	mustReceiptOK(t, s2, "RC2", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 11, 30))
	mustRevoke(t, s2, "RV1", "RC2", "误录签收", tClock(2026, 10, 5, 12, 0))
	p2, _ := s2.Query("P002")
	if p2.Status != statusDelivering || currentBatch(p2) != "B1" {
		t.Fatalf("撤销后余件应恢复原批次配送中: %+v", p2)
	}
	// 撤销件可再次续接（与 P004 一起）。
	mustTransfer(t, s2, "T2", "B1", "B3", "王五", "余件续接", tClock(2026, 10, 5, 12, 30))
	b1, _ := s2.BatchQuery("B1")
	if b1.TransferredBy != "T2" {
		t.Fatalf("撤销件随余件续接、转走最后待配送件后应永久转交: %+v", b1)
	}

	// 场景三：分批后续接原批次中止，只收回仍在原批次配送的待配送件（不含已转交件）。
	s3, _ := openTempStore(t)
	setupPartialRelayBase(t, s3)
	mustTransferParcels(t, s3, "T1", "B1", "B2", "李四", "分批", []string{"P003"}, tClock(2026, 10, 5, 11, 0))
	ab := mustAbort(t, s3, "A1", "B1", "车辆故障收回余件", tClock(2026, 10, 5, 11, 30))
	if !sameOrder(ab.Parcels, []string{"P002", "P004"}) {
		t.Fatalf("中止只应收回落空在原批次的余件 P002、P004: %v", ab.Parcels)
	}
	for _, pid := range []string{"P002", "P004"} {
		p, _ := s3.Query(pid)
		if p.Status != statusInStation || p.Station != "站点A" {
			t.Fatalf("收回件应在出发站在站: %+v", p)
		}
	}
	// 已转交件 P003 不受中止影响，仍在 B2 配送中，并可正常回执。
	p3, _ := s3.Query("P003")
	if p3.Status != statusDelivering || currentBatch(p3) != "B2" {
		t.Fatalf("已转交件不应被旧批次中止收回: %+v", p3)
	}
	mustReceiptOK(t, s3, "RC2", "B2", "P003", resultSigned, "", tClock(2026, 10, 5, 12, 0))
}

// 转交件不能首次回执旧批次；receipt-import 含它整份拒绝、仅撤销此次新增。
func TestTransferPartialTransferredCannotReceiptOldBatch(t *testing.T) {
	s, _ := openTempStore(t)
	setupPartialRelayBase(t, s)
	mustTransferParcels(t, s, "T1", "B1", "B2", "李四", "分批", []string{"P002", "P003"}, tClock(2026, 10, 5, 11, 0))
	now := tClock(2026, 10, 5, 12, 0)

	if _, _, err := s.Receipt("RC2", "B1", "P002", resultSigned, "", now); err == nil ||
		!strings.Contains(err.Error(), "不在批次") {
		t.Fatalf("转交件不能首次回执旧批次: %v", err)
	}
	// 余件 P004 仍可回执旧批次。
	mustReceiptOK(t, s, "RC3", "B1", "P004", resultSigned, "", now)

	// 导入含转交件对旧批次的迟到回执：整份拒绝、仅撤销此次新增，新号不占用。
	_, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC5", Batch: "B2", Parcel: "P002", Result: resultSigned},
		{Request: "RC6", Batch: "B1", Parcel: "P003", Result: resultSigned},
	}, now)
	if err == nil || !strings.Contains(err.Error(), "第 2 条") {
		t.Fatalf("含迟到回执的导入应整份拒绝并提示位置: %v", err)
	}
	if s.ReceiptOf("RC5") != nil || s.ReceiptOf("RC6") != nil {
		t.Fatal("整份拒绝后此次新增的回执请求号均不得占用")
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusDelivering || len(p2.Trail) != 3 {
		t.Fatalf("整份拒绝后转交件保持续接后状态: %+v", p2)
	}
}

// 两种选择方式共用独立续接请求号：去重内容含原输入、选择方式和显式集合，
// 集合换序无关；同号换内容或方式冲突。重放返回首次信息、集合和时间，不检查
// 现状、不重算余件、不追加轨迹或改写文件。
func TestTransferPartialReplayAndConflict(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialRelayBase(t, s)
	tm := tClock(2026, 10, 5, 11, 0)
	first := mustTransferParcels(t, s, "T1", "B1", "B2", "李四", "分批", []string{"P003", "P002"}, tm)
	// 不选方式：取当时全部余件 P004（P002、P003 已转交），转走后 B1 永久转交。
	all := mustTransfer(t, s, "T2", "B1", "B3", "王五", "全取", tClock(2026, 10, 5, 11, 30))
	if !sameOrder(all.Parcels, []string{"P004"}) || all.Explicit {
		t.Fatalf("不选方式应取当时余件 P004: %+v", all)
	}
	// 后续作业：新批次回执。
	mustReceiptOK(t, s, "RC2", "B2", "P002", resultSigned, "", tClock(2026, 10, 5, 12, 0))
	before, _ := os.ReadFile(path)

	// 显式集合换序重放：返回首次集合（原成员顺序）与时间。
	got, replayed, err := s.Transfer("T1", "B1", "B2", "李四", "分批", []string{"P002", "P003"}, tClock(2026, 10, 5, 13, 0))
	if err != nil || !replayed || got != first || !got.Explicit || !sameOrder(got.Parcels, []string{"P002", "P003"}) {
		t.Fatalf("显式集合换序重放应返回首次结果: replayed=%v err=%v got=%+v", replayed, err, got)
	}
	// 不选方式重放：不检查现状、不重算余件，仍返回当时集合 P004。
	got2, replayed, err := s.Transfer("T2", "B1", "B3", "王五", "全取", nil, tClock(2026, 10, 5, 13, 0))
	if err != nil || !replayed || got2 != all || !sameOrder(got2.Parcels, []string{"P004"}) {
		t.Fatalf("不选方式重放应返回首次结果: replayed=%v err=%v got=%+v", replayed, err, got2)
	}
	// 重放不追加轨迹、不改写文件。
	p2, _ := s.Query("P002")
	if len(p2.Trail) != 4 { // 收件、出站、续接、回执
		t.Fatalf("重放不应追加轨迹，得到 %d 条", len(p2.Trail))
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("重放不应改写数据文件")
	}

	// 同号切换选择方式：冲突（显式 -> 不选、不选 -> 显式）。
	if _, _, err := s.Transfer("T1", "B1", "B2", "李四", "分批", nil, tClock(2026, 10, 5, 14, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同号由显式切换为不选应报冲突: %v", err)
	}
	if _, _, err := s.Transfer("T2", "B1", "B3", "王五", "全取", []string{"P004"}, tClock(2026, 10, 5, 14, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同号由不选切换为显式应报冲突: %v", err)
	}
	// 同号换显式集合：冲突。
	if _, _, err := s.Transfer("T1", "B1", "B2", "李四", "分批", []string{"P002"}, tClock(2026, 10, 5, 14, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同号换集合应报冲突: %v", err)
	}
	// 同号换其他内容：冲突。
	if _, _, err := s.Transfer("T1", "B1", "B9", "李四", "分批", []string{"P002", "P003"}, tClock(2026, 10, 5, 14, 0)); err == nil {
		t.Fatal("同号换新批次应报冲突")
	}
}

// 首次续接的各类失败整次拒绝、原子保存：不占请求号或新批次号、不改原数据。
func TestTransferPartialFailuresAtomic(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialRelayBase(t, s)
	mustTransferParcels(t, s, "T0", "B1", "B7", "李四", "先行分批", []string{"P003"}, tClock(2026, 10, 5, 10, 40))
	mustRegister(t, s, "P404", "站点A", tClock(2026, 10, 5, 9, 0))

	before, _ := os.ReadFile(path)
	now := tClock(2026, 10, 5, 11, 0)
	cases := []struct {
		name    string
		request string
		from    string
		to      string
		courier string
		reason  string
		parcels []string
	}{
		{"显式集合为空", "T1", "B1", "B2", "王五", "原因", []string{}},
		{"选中件不属于原批次", "T2", "B1", "B2", "王五", "原因", []string{"P404"}},
		{"选中件已有有效回执", "T3", "B1", "B2", "王五", "原因", []string{"P001"}},
		{"选中件已转交", "T4", "B1", "B2", "王五", "原因", []string{"P003"}},
		{"显式集合重复", "T5", "B1", "B2", "王五", "原因", []string{"P002", "P002"}},
		{"原批次不存在", "T6", "NOPE", "B2", "王五", "原因", []string{"P002"}},
		{"新批次号已被续接使用", "T7", "B1", "B7", "王五", "原因", []string{"P002"}},
		{"新配送员与原配送员相同", "T8", "B1", "B2", "张三", "原因", []string{"P002"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Transfer(c.request, c.from, c.to, c.courier, c.reason, c.parcels, now); err == nil {
				t.Fatal("条件不满足时续接必须失败")
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Fatal("失败续接不得写入数据文件")
			}
			if s.TransferOf(c.request) != nil {
				t.Fatalf("失败的首次续接不得占用请求号 %q", c.request)
			}
			if _, ok := s.data.Batches[c.to]; c.to != "B7" && ok {
				t.Fatalf("失败的首次续接不得占用新批次号 %q", c.to)
			}
		})
	}

	// 原批次已中止：不能续接。
	s2, _ := openTempStore(t)
	setupPartialRelayBase(t, s2)
	mustAbort(t, s2, "A1", "B1", "车辆故障", tClock(2026, 10, 5, 10, 45))
	if _, _, err := s2.Transfer("T1", "B1", "B2", "李四", "原因", []string{"P002"}, now); err == nil {
		t.Fatal("已中止批次不能续接")
	}

	// 防御性校验：选中件不在原批次配送中（公开流程不可达，直接构造内存状态）。
	s3, _ := openTempStore(t)
	setupPartialRelayBase(t, s3)
	p2 := s3.data.Parcels["P002"]
	oldStatus := p2.Status
	p2.Status = statusInStation
	if _, _, err := s3.Transfer("T1", "B1", "B2", "李四", "原因", []string{"P002"}, now); err == nil {
		t.Fatal("选中件不在原批次配送中时必须整次拒绝")
	}
	p2.Status = oldStatus
	if s3.TransferOf("T1") != nil {
		t.Fatal("失败续接不得占用请求号")
	}
	if _, ok := s3.data.Batches["B2"]; ok {
		t.Fatal("失败续接不得占用新批次号")
	}

	// 纠正后可重试成功。
	mustTransferParcels(t, s, "T1", "B1", "B2", "王五", "纠正后续接", []string{"P002"}, tClock(2026, 10, 5, 12, 0))
}

// 旧有效台账直接使用：旧版整批续接只有批次上的 transferredBy、没有 transfers 列表，
// 载入时自动迁移，批次状态、去向展示与去重重放均保持。
func TestTransferPartialLegacyLedger(t *testing.T) {
	s, path := openTempStore(t)
	setupRelayBase(t, s) // P001 已签收，P002、P003 待配送
	mustTransfer(t, s, "T1", "B1", "B2", "李四", "车辆故障", tClock(2026, 10, 5, 11, 0))

	// 删去批次 B1 内嵌的 transfers 数组，模拟旧版台账（仅 transferredBy）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	b1 := m["batches"].(map[string]any)["B1"].(map[string]any)
	if _, has := b1["transfers"]; !has {
		t.Fatal("当前台账应含批次级 transfers 列表")
	}
	delete(b1, "transfers")
	buf, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("旧版台账应直接打开并迁移: %v", err)
	}
	old, _ := s2.BatchQuery("B1")
	if old.TransferredBy != "T1" || len(old.Transfers) != 1 || old.Transfers[0] != "T1" {
		t.Fatalf("应把旧 transferredBy 迁移为 transfers 列表: %+v", old)
	}
	all := s2.BatchTransfers("B1")
	if len(all) != 1 || all[0].Request != "T1" {
		t.Fatalf("迁移后应能列出各次转交: %+v", all)
	}
	if s2.BatchTransferSource("B1") == nil {
		t.Fatal("迁移后旧整批续接仍应判定为永久转交")
	}
	// 旧续接按不选方式去重：同内容（nil）重放成立，显式方式冲突。
	if _, replayed, err := s2.Transfer("T1", "B1", "B2", "李四", "车辆故障", nil, tClock(2026, 10, 5, 12, 0)); err != nil || !replayed {
		t.Fatalf("旧续接应按不选方式重放: replayed=%v err=%v", replayed, err)
	}
	if _, _, err := s2.Transfer("T1", "B1", "B2", "李四", "车辆故障", []string{"P002", "P003"}, tClock(2026, 10, 5, 12, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("旧不选方式记录与显式提交应报冲突: %v", err)
	}
}

// 分批续接的关联缺失、同件从同批次重复转交或与轨迹、新批次矛盾时明确拒绝载入。
func TestTransferPartialCorrupt(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialRelayBase(t, s)
	mustTransferParcels(t, s, "T1", "B1", "B2", "李四", "分批", []string{"P003"}, tClock(2026, 10, 5, 11, 0))
	mustTransferParcels(t, s, "T2", "B1", "B3", "王五", "再分批", []string{"P002"}, tClock(2026, 10, 5, 11, 30))
	raw, _ := os.ReadFile(path)
	batch := func(m map[string]any, id string) map[string]any {
		return m["batches"].(map[string]any)[id].(map[string]any)
	}
	transfer := func(m map[string]any, req string) map[string]any {
		return m["transfers"].(map[string]any)[req].(map[string]any)
	}

	corruptLedger(t, path, raw, "续接列表引用不存在的请求", func(m map[string]any) {
		batch(m, "B1")["transfers"] = []any{"T1", "NOPE"}
	})
	corruptLedger(t, path, raw, "同件从同批次重复转交", func(m map[string]any) {
		// 把 T2 的集合改成 P003（与 T1 重复），并同步新批次 B3 成员。
		transfer(m, "T2")["parcels"] = []any{"P003"}
		batch(m, "B3")["parcels"] = []any{"P003"}
	})
	corruptLedger(t, path, raw, "续接与新批次来源矛盾", func(m map[string]any) {
		batch(m, "B2")["relayRequest"] = "T2"
	})
	corruptLedger(t, path, raw, "续接列表请求号重复", func(m map[string]any) {
		batch(m, "B1")["transfers"] = []any{"T1", "T1"}
	})
	corruptLedger(t, path, raw, "部分续接却标记永久转交", func(m map[string]any) {
		// 移除第二次续接的全部关联（结果表、新批次、P002 的续接轨迹），
		// 只保留 T1（仅转走 P003），再把 B1 标为永久转交；此时 P002、P004 仍待配送。
		delete(m["transfers"].(map[string]any), "T2")
		delete(m["batches"].(map[string]any), "B3")
		p2 := m["parcels"].(map[string]any)["P002"].(map[string]any)
		trail := p2["trail"].([]any)
		p2["trail"] = trail[:len(trail)-1] // 去掉 T2 续接轨迹
		b := batch(m, "B1")
		b["transfers"] = []any{"T1"}
		b["transferredBy"] = "T1"
	})
	corruptLedger(t, path, raw, "续接结果为 null", func(m map[string]any) {
		m["transfers"].(map[string]any)["T1"] = nil
	})
	restoreLedger(t, path, raw)
}

// CLI 端到端：分批续接、batch 各次转交去向与待配送区分、query 当前批次、
// 选择方式去重与退出码。
func TestCLIEndToEndPartialRelay(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	for _, id := range []string{"P001", "P002", "P003", "P004"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败: %d", id, code)
		}
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A", "--courier", "张三",
		"--parcel", "P001", "--parcel", "P002", "--parcel", "P003", "--parcel", "P004"); code != 0 {
		t.Fatal("出站失败")
	}
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001", "--result", "签收"); code != 0 {
		t.Fatal("回执失败")
	}

	// 显式分批：只转 P002、P003（乱序提交）。
	out, _, code := runCLI(t, dbPath, "relay", "--request", "T1", "--from", "B1", "--to", "B2",
		"--courier", "李四", "--reason", "分批换班", "--parcel", "P003", "--parcel", "P002")
	if code != 0 || !strings.Contains(out, "续接成功") || !strings.Contains(out, "选择方式: 显式选择") ||
		!strings.Contains(out, "P002") || !strings.Contains(out, "P003") || strings.Contains(out, "P004") {
		t.Fatalf("显式分批续接输出不符: code=%d out=%s", code, out)
	}

	// 原批次仍配送中：P001 已回执、P002/P003 已转交（列去向）、P004 待配送，
	// 另列转交记录，转交不计回执。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 配送中") ||
		!strings.Contains(out, "P001    已回执") ||
		!strings.Contains(out, "P002    已转交") || !strings.Contains(out, "去向批次: B2") ||
		!strings.Contains(out, "P003    已转交") ||
		!strings.Contains(out, "P004    未回执") ||
		!strings.Contains(out, "转交记录（1 次") || strings.Contains(out, "转交去向:") {
		t.Fatalf("分批后原批次展示不符: code=%d out=%s", code, out)
	}

	// query：余件 P004 仍显示当前批次 B1 与张三；转交件 P002 显示 B2 与李四。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P004")
	if code != 0 || !strings.Contains(out, "当前批次: B1") || !strings.Contains(out, "当前配送员: 张三") {
		t.Fatalf("余件 query 应显示原批次/配送员: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, dbPath, "query", "--id", "P002")
	if code != 0 || !strings.Contains(out, "当前批次: B2") || !strings.Contains(out, "当前配送员: 李四") ||
		!strings.Contains(out, "操作: 续接") {
		t.Fatalf("转交件 query 应显示新批次/配送员与续接轨迹: code=%d out=%s", code, out)
	}

	// 显式集合换序重放：返回首次结果。
	out, _, code = runCLI(t, dbPath, "relay", "--request", "T1", "--from", "B1", "--to", "B2",
		"--courier", "李四", "--reason", "分批换班", "--parcel", "P002", "--parcel", "P003")
	if code != 0 || !strings.Contains(out, "重复提交") {
		t.Fatalf("显式集合换序重放应返回首次结果: code=%d out=%s", code, out)
	}
	// 同号切换选择方式冲突，退出码 1。
	if _, errText, code := runCLI(t, dbPath, "relay", "--request", "T1", "--from", "B1", "--to", "B2",
		"--courier", "李四", "--reason", "分批换班"); code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("同号切换选择方式应冲突(1): code=%d err=%s", code, errText)
	}
	// 选中已回执件失败，退出码 1。
	if _, _, code := runCLI(t, dbPath, "relay", "--request", "T8", "--from", "B1", "--to", "B8",
		"--courier", "王五", "--reason", "原因", "--parcel", "P001"); code != exitBusiness {
		t.Fatalf("选中已回执件应失败(1): code=%d", code)
	}

	// 不选方式转走最后余件 P004：原批次永久关闭为已转交，batch 列各次去向。
	out, _, code = runCLI(t, dbPath, "relay", "--request", "T2", "--from", "B1", "--to", "B3",
		"--courier", "王五", "--reason", "余件换班")
	if code != 0 || !strings.Contains(out, "选择方式: 不选成员") || !strings.Contains(out, "P004") ||
		strings.Contains(out, "P002") {
		t.Fatalf("不选方式应只转余件 P004: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 已转交") ||
		!strings.Contains(out, "转交去向: 批次 B3") ||
		!strings.Contains(out, "转交记录（2 次") ||
		!strings.Contains(out, "P004    已转交    去向批次: B3") {
		t.Fatalf("永久转交后 batch 展示不符: code=%d out=%s", code, out)
	}

	// 损坏文件经 errors.Is 识别为 ErrCorrupt（关联缺失）。
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = s
	raw, _ := os.ReadFile(dbPath)
	var mm map[string]any
	if err := json.Unmarshal(raw, &mm); err != nil {
		t.Fatal(err)
	}
	mm["batches"].(map[string]any)["B1"].(map[string]any)["transfers"] = []any{"T1", "NOPE"}
	bad, _ := json.Marshal(mm)
	if err := os.WriteFile(dbPath, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dbPath); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("续接关联缺失应识别为数据损坏: %v", err)
	}
}
