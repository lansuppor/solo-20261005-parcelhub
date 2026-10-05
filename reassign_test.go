package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustReassign(t *testing.T, s *Store, request, fromBatch, toBatch, courier, reason string, now time.Time) *ReassignResult {
	t.Helper()
	res, replayed, err := s.Reassign(request, fromBatch, toBatch, courier, reason, now)
	if err != nil || replayed {
		t.Fatalf("Reassign(%q) 意外失败: %v replayed=%v", request, err, replayed)
	}
	return res
}

func TestReassignSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002", "P003"}, tClock(2026, 10, 5, 9, 30))
	// P001 先签收：已回执成员不检查也不变更，其回执完全保留。
	if _, _, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0)); err != nil {
		t.Fatal(err)
	}

	t2 := tClock(2026, 10, 5, 11, 0)
	res := mustReassign(t, s, "RA1", "B1", "B2", "李四", "原配送员车辆故障", t2)
	if res.Request != "RA1" || res.FromBatch != "B1" || res.ToBatch != "B2" ||
		res.Station != "站点A" || res.FromCourier != "张三" || res.ToCourier != "李四" ||
		res.Reason != "原配送员车辆故障" || !res.Time.Equal(t2) {
		t.Fatalf("续接结果不符: %+v", res)
	}
	if len(res.Parcels) != 2 || res.Parcels[0] != "P002" || res.Parcels[1] != "P003" {
		t.Fatalf("转交集合应为未回执成员、按原成员顺序: %v", res.Parcels)
	}

	// 转交件保持配送中及原站点，当前配送归属切换到新批次，各追加一条续接轨迹。
	for _, pid := range []string{"P002", "P003"} {
		p, _ := s.Query(pid)
		if p.Station != "站点A" || p.Status != statusDelivering {
			t.Fatalf("续接后 %s 应保持配送中、站点不变: %+v", pid, p)
		}
		if currentBatch(p) != "B2" {
			t.Fatalf("续接后 %s 当前批次应为 B2，得到 %q", pid, currentBatch(p))
		}
		if len(p.Trail) != 3 {
			t.Fatalf("%s 应有三条轨迹，得到 %d", pid, len(p.Trail))
		}
		e := p.Trail[2]
		if e.Op != "续接" || e.Station != "站点A" || e.FromBatch != "B1" || e.Batch != "B2" ||
			e.Courier != "李四" || e.Request != "RA1" || e.Reason != "原配送员车辆故障" || !e.Time.Equal(t2) {
			t.Fatalf("%s 续接轨迹不符: %+v", pid, e)
		}
	}

	// 已回执成员不动：状态、轨迹、批次回执保留。
	p1, _ := s.Query("P001")
	if p1.Status != statusSigned || len(p1.Trail) != 3 || p1.Trail[2].Op != "回执" {
		t.Fatalf("已回执成员不得被改变: %+v", p1)
	}

	// 原批次永久关闭为已转交，保留原成员、配送员、出站时间和真实回执。
	b1, _ := s.BatchQuery("B1")
	if b1.TransferredBy != "RA1" || len(b1.Receipts) != 1 || b1.Receipts["P001"].Request != "RC1" {
		t.Fatalf("原批次应标记转交且保留真实回执: %+v", b1)
	}
	if b1.Courier != "张三" || len(b1.Parcels) != 3 || b1.Parcels[0] != "P001" {
		t.Fatalf("原批次原成员顺序与配送员必须保留: %+v", b1)
	}

	// 新批次按原顺序接纳全部未回执件，沿用出发站，记录新配送员及接手时间。
	b2, _ := s.BatchQuery("B2")
	if b2.Station != "站点A" || b2.Courier != "李四" || !b2.Time.Equal(t2) || b2.FromReassign != "RA1" {
		t.Fatalf("新批次不符: %+v", b2)
	}
	if len(b2.Parcels) != 2 || b2.Parcels[0] != "P002" || b2.Parcels[1] != "P003" {
		t.Fatalf("新批次成员应为转交集合、按原顺序: %v", b2.Parcels)
	}
	if len(b2.Receipts) != 0 {
		t.Fatalf("新批次不应带回执: %+v", b2.Receipts)
	}

	if tr := s.BatchTransfer("B1"); tr != res {
		t.Fatalf("BatchTransfer 应返回续接结果: %+v", tr)
	}
	if og := s.BatchOrigin("B2"); og != res {
		t.Fatalf("BatchOrigin 应返回续接结果: %+v", og)
	}
	if s.BatchTransfer("B2") != nil || s.BatchOrigin("B1") != nil || s.BatchTransfer("NOPE") != nil {
		t.Fatal("未转交/非续接批次应无关联结果")
	}
}

func TestReassignFailuresAtomic(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P003"}, tClock(2026, 10, 5, 9, 30))
	mustDispatch(t, s, "B2", "站点A", "李四", []string{"P002"}, tClock(2026, 10, 5, 9, 40))
	// B2 全部回执完成。
	if _, _, err := s.Receipt("RC1", "B2", "P002", resultSigned, "", tClock(2026, 10, 5, 10, 0)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		request   string
		fromBatch string
		toBatch   string
		courier   string
		reason    string
	}{
		{"原批次不存在", "RA1", "NOPE", "B9", "王五", "原因"},
		{"已完成批次不能续接", "RA2", "B2", "B9", "王五", "原因"},
		{"新批次号已被出站使用", "RA3", "B1", "B2", "王五", "原因"},
		{"新配送员与原配送员相同", "RA4", "B1", "B9", "张三", "原因"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Reassign(c.request, c.fromBatch, c.toBatch, c.courier, c.reason, tClock(2026, 10, 5, 11, 0)); err == nil {
				t.Fatal("条件不满足时续接必须失败")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("失败续接不得写入数据文件")
			}
			if _, ok := s.data.Reassigns[c.request]; ok {
				t.Fatalf("失败的首次续接不得占用请求号 %q", c.request)
			}
			if _, ok := s.data.Batches[c.toBatch]; c.toBatch == "B9" && ok {
				t.Fatalf("失败续接不得创建新批次 %q", c.toBatch)
			}
		})
	}

	// 已中止批次不能续接；已转交批次不能再次续接。
	mustAbort(t, s, "A1", "B1", "车辆故障", tClock(2026, 10, 5, 11, 30))
	if _, _, err := s.Reassign("RA6", "B1", "B9", "王五", "原因", tClock(2026, 10, 5, 11, 40)); err == nil {
		t.Fatal("已中止批次必须拒绝续接")
	}
	mustRegister(t, s, "P004", "站点A", t1)
	mustDispatch(t, s, "B3", "站点A", "张三", []string{"P004"}, tClock(2026, 10, 5, 12, 0))
	mustReassign(t, s, "RA7", "B3", "B4", "李四", "途中交接", tClock(2026, 10, 5, 12, 30))
	if _, _, err := s.Reassign("RA8", "B3", "B9", "王五", "再次续接", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("已转交批次必须拒绝再次续接")
	}
	if _, ok := s.data.Reassigns["RA8"]; ok {
		t.Fatal("被拒绝的续接不得占用请求号")
	}
	// 失败不占号：RA1 之后仍可用于新的有效续接。
	mustRegister(t, s, "P005", "站点A", t1)
	mustDispatch(t, s, "B5", "站点A", "张三", []string{"P005"}, tClock(2026, 10, 5, 13, 30))
	// 新批次号已被续接使用也不能复用。
	if _, _, err := s.Reassign("RA9", "B5", "B4", "王五", "原因", tClock(2026, 10, 5, 13, 45)); err == nil {
		t.Fatal("新批次号已被续接使用必须拒绝")
	}
	if _, ok := s.data.Reassigns["RA9"]; ok {
		t.Fatal("被拒绝的续接不得占用请求号")
	}
	if res := mustReassign(t, s, "RA1", "B5", "B6", "王五", "原因", tClock(2026, 10, 5, 14, 0)); res.ToBatch != "B6" {
		t.Fatalf("失败不占号，纠正后同请求号应可成功: %+v", res)
	}
}

func TestReassignReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	t2 := tClock(2026, 10, 5, 10, 0)
	first := mustReassign(t, s, "RA1", "B1", "B2", "李四", "途中交接", t2)

	// 后续回执、中止或再次续接后仍可重放：先让新批次继续流转。
	if _, _, err := s.Receipt("RC1", "B2", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 30)); err != nil {
		t.Fatal(err)
	}
	res, replayed, err := s.Reassign("RA1", "B1", "B2", "李四", "途中交接", tClock(2026, 10, 5, 11, 0))
	if err != nil || !replayed {
		t.Fatalf("同内容重放应成功: %v replayed=%v", err, replayed)
	}
	if res != first || !res.Time.Equal(t2) || len(res.Parcels) != 2 || res.Parcels[0] != "P001" {
		t.Fatalf("重放应返回首次集合、续接信息及时间: %+v", res)
	}
	p, _ := s.Query("P002")
	count := 0
	for _, e := range p.Trail {
		if e.Op == "续接" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("重放不得追加续接轨迹，得到 %d 条", count)
	}

	// 同号换内容：冲突，已有结果不变。
	if _, _, err := s.Reassign("RA1", "B1", "B2", "王五", "途中交接", tClock(2026, 10, 5, 11, 30)); err == nil {
		t.Fatal("同号换配送员必须报冲突")
	}
	if _, _, err := s.Reassign("RA1", "B1", "B2", "李四", "别的原因", tClock(2026, 10, 5, 11, 45)); err == nil {
		t.Fatal("同号换原因必须报冲突")
	}
	if s.data.Reassigns["RA1"] != first {
		t.Fatal("冲突不得改写已有续接结果")
	}

	// 续接请求号独立去重：可与其他业务编号同名。
	if res := mustReassign(t, s, "B1", "B2", "B3", "王五", "同名请求号", tClock(2026, 10, 5, 12, 0)); res.Request != "B1" {
		t.Fatalf("续接请求号可与批次号同名: %+v", res)
	}
}

func TestReassignOldBatchClosed(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustHandoff(t, s, "H1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 10))
	mustDispatch(t, s, "B1", "站点B", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustReassign(t, s, "RA1", "B1", "B2", "李四", "途中交接", tClock(2026, 10, 5, 10, 0))

	// 转交件不能首次回执旧批次。
	if _, _, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 30)); err == nil {
		t.Fatal("转交件不得首次回执旧批次")
	}
	if _, ok := s.data.Receipts["RC1"]; ok {
		t.Fatal("被拒绝的回执不得占用请求号")
	}

	// receipt-import 含迟到回执：整份拒绝，仅撤销此次新增，新号不占用。
	records := []ReceiptImportRecord{
		{Request: "RC2", Batch: "B2", Parcel: "P001", Result: resultSigned},
		{Request: "RC3", Batch: "B1", Parcel: "P002", Result: resultFailed, Reason: "迟到回执"},
	}
	if _, err := s.ImportReceipts(records, tClock(2026, 10, 5, 11, 0)); err == nil {
		t.Fatal("含迟到回执的导入必须整份拒绝")
	}
	if _, ok := s.data.Receipts["RC2"]; ok {
		t.Fatal("整份拒绝时此次新增必须撤销，新号不得占用")
	}
	if _, ok := s.data.Receipts["RC3"]; ok {
		t.Fatal("迟到回执不得占用请求号")
	}
	p1, _ := s.Query("P001")
	if currentBatch(p1) != "B2" || len(p1.Trail) != 4 {
		t.Fatalf("整份拒绝不得改动其他包裹: %+v", p1)
	}
	// 新号未占用：纠正后可重新导入。
	if _, err := s.ImportReceipts(records[:1], tClock(2026, 10, 5, 11, 30)); err != nil {
		t.Fatalf("纠正后导入应成功: %v", err)
	}

	// 旧批次不能首次中止。
	if _, _, err := s.Abort("A1", "B1", "尝试中止", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("已转交批次必须拒绝中止")
	}
	if _, ok := s.data.Aborts["A1"]; ok {
		t.Fatal("被拒绝的中止不得占用请求号")
	}

	// 续接算新流转：不能退回配送前的旧交接。
	if _, _, err := s.Return("RT1", "H1", "错发", tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatal("续接后不得退回配送前的旧交接")
	}

	// 原 dispatch 的同内容重放仍返回旧结果，不移动包裹或重开批次。
	if _, replayedD, err := s.Dispatch("B1", "站点B", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 13, 0)); err != nil || !replayedD {
		t.Fatalf("原批次同内容重放应返回历史结果: %v replayed=%v", err, replayedD)
	}
	if p, _ := s.Query("P002"); currentBatch(p) != "B2" {
		t.Fatalf("dispatch 重放不得移动包裹: %+v", p)
	}
	if b, _ := s.BatchQuery("B1"); b.TransferredBy != "RA1" {
		t.Fatal("dispatch 重放不得清除转交标记")
	}

	// dispatch 使用续接创建的批次号报冲突。
	if _, _, err := s.Dispatch("B2", "站点B", "李四", []string{"P002"}, tClock(2026, 10, 5, 13, 30)); err == nil {
		t.Fatal("dispatch 使用续接创建的批次号必须报冲突")
	}
}

func TestReassignNewBatchFlows(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	for _, id := range []string{"P001", "P002", "P003"} {
		mustRegister(t, s, id, "站点A", t1)
	}
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002", "P003"}, tClock(2026, 10, 5, 9, 30))
	mustReassign(t, s, "RA1", "B1", "B2", "李四", "途中交接", tClock(2026, 10, 5, 10, 0))

	// 新批次可正常回执。
	if _, _, err := s.Receipt("RC1", "B2", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 30)); err != nil {
		t.Fatalf("新批次回执应正常: %v", err)
	}
	p1, _ := s.Query("P001")
	if p1.Status != statusSigned {
		t.Fatalf("新批次签收后应为已签收: %+v", p1)
	}

	// 新批次可再次续接。
	mustReassign(t, s, "RA2", "B2", "B3", "王五", "二次交接", tClock(2026, 10, 5, 11, 0))
	p2, _ := s.Query("P002")
	if currentBatch(p2) != "B3" || p2.Status != statusDelivering {
		t.Fatalf("再次续接后当前批次应为 B3: %+v", p2)
	}
	if b2, _ := s.BatchQuery("B2"); b2.TransferredBy != "RA2" {
		t.Fatalf("B2 应标记为已转交: %+v", b2)
	}

	// 再次续接后的新批次可中止。
	res := mustAbort(t, s, "A1", "B3", "车辆故障", tClock(2026, 10, 5, 12, 0))
	if len(res.Parcels) != 2 || res.Parcels[0] != "P002" || res.Parcels[1] != "P003" {
		t.Fatalf("B3 中止收回集合不符: %v", res.Parcels)
	}
	p3, _ := s.Query("P003")
	if p3.Status != statusInStation || p3.Station != "站点A" {
		t.Fatalf("收回后应在出发站在站: %+v", p3)
	}
}

func TestReassignReceiptedMembersUntouched(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	for _, id := range []string{"P001", "P002", "P003"} {
		mustRegister(t, s, id, "站点A", t1)
	}
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002", "P003"}, tClock(2026, 10, 5, 9, 30))
	// P001 失败回执回到出发站后被冻结；P002 失败回执后进入新批次。
	if _, _, err := s.Receipt("RC1", "B1", "P001", resultFailed, "收件人不在", tClock(2026, 10, 5, 10, 0)); err != nil {
		t.Fatal(err)
	}
	mustFreeze(t, s, "E1", "P001", "站点A", "破损", tClock(2026, 10, 5, 10, 15))
	if _, _, err := s.Receipt("RC2", "B1", "P002", resultFailed, "拒收", tClock(2026, 10, 5, 10, 30)); err != nil {
		t.Fatal(err)
	}
	mustDispatch(t, s, "B2", "站点A", "李四", []string{"P002"}, tClock(2026, 10, 5, 11, 0))

	// 已回执成员被冻结或进入其他批次都不阻止续接，也不检查或改变其状态。
	res := mustReassign(t, s, "RA1", "B1", "B3", "王五", "途中交接", tClock(2026, 10, 5, 12, 0))
	if len(res.Parcels) != 1 || res.Parcels[0] != "P003" {
		t.Fatalf("转交集合应只含未回执成员 P003: %v", res.Parcels)
	}
	p1, _ := s.Query("P001")
	if p1.Status != statusFrozen || len(p1.Trail) != 4 {
		t.Fatalf("被冻结的已回执成员不得被改变: %+v", p1)
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusDelivering || currentBatch(p2) != "B2" {
		t.Fatalf("已进入新批次的已回执成员不得被改变: %+v", p2)
	}
	b1, _ := s.BatchQuery("B1")
	if len(b1.Receipts) != 2 {
		t.Fatalf("已回执记录必须完全保留: %+v", b1.Receipts)
	}
}

func TestReassignPersistenceAcrossRestart(t *testing.T) {
	_, path := openTempStore(t)
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s1, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s1, "P002", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s1, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	if _, _, err := s1.Receipt("RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0)); err != nil {
		t.Fatal(err)
	}
	mustReassign(t, s1, "RA1", "B1", "B2", "李四", "途中交接", tClock(2026, 10, 5, 11, 0))

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	p, _ := s2.Query("P002")
	if p.Status != statusDelivering || currentBatch(p) != "B2" || len(p.Trail) != 3 || p.Trail[2].Op != "续接" || p.Trail[2].Request != "RA1" {
		t.Fatalf("重启后续接轨迹不符: %+v", p)
	}
	// 重启后同内容重放仍成立。
	res, replayed, err := s2.Reassign("RA1", "B1", "B2", "李四", "途中交接", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || len(res.Parcels) != 1 || res.Parcels[0] != "P002" {
		t.Fatalf("重启后续接重放失败: %v replayed=%v res=%+v", err, replayed, res)
	}
	// 重启后旧批次仍须拒绝再次续接与中止。
	if _, _, err := s2.Reassign("RA2", "B1", "B9", "王五", "再次续接", tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatal("重启后已转交批次仍须拒绝再次续接")
	}
	if _, _, err := s2.Abort("A1", "B1", "尝试中止", tClock(2026, 10, 5, 12, 45)); err == nil {
		t.Fatal("重启后已转交批次仍须拒绝中止")
	}
	// 重启后转交件仍不能首次回执旧批次。
	if _, _, err := s2.Receipt("RC2", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("重启后转交件仍须拒绝首次回执旧批次")
	}
	// 重启后新批次可正常回执。
	if _, _, err := s2.Receipt("RC3", "B2", "P002", resultSigned, "", tClock(2026, 10, 5, 13, 30)); err != nil {
		t.Fatalf("重启后新批次回执应正常: %v", err)
	}
}

func TestCorruptReassignDataRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")

	parcel := `"id":"P1","station":"A","status":"配送中","registered":"2026-10-05T09:00:00Z"`
	trail := `[{"op":"收件","station":"A","time":"2026-10-05T09:00:00Z"},` +
		`{"op":"出站","station":"A","time":"2026-10-05T09:30:00Z","batch":"B1","courier":"张三"},` +
		`{"op":"续接","station":"A","time":"2026-10-05T10:00:00Z","batch":"B2","fromBatch":"B1","courier":"李四","request":"RA1","reason":"途中交接"}]`
	oldBatch := `"batch":"B1","station":"A","courier":"张三","parcels":["P1"],"time":"2026-10-05T09:30:00Z","receipts":{},"transferredBy":"RA1"`
	newBatch := `"batch":"B2","station":"A","courier":"李四","parcels":["P1"],"time":"2026-10-05T10:00:00Z","receipts":{},"fromReassign":"RA1"`
	reassign := `"request":"RA1","fromBatch":"B1","toBatch":"B2","station":"A","fromCourier":"张三","toCourier":"李四","reason":"途中交接","parcels":["P1"],"time":"2026-10-05T10:00:00Z"`
	good := `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
		`"batches":{"B1":{` + oldBatch + `},"B2":{` + newBatch + `}},"reassigns":{"RA1":{` + reassign + `}}}`

	// 完好的续接台账可以直接打开。
	if err := os.WriteFile(path, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("有效续接台账应可打开: %v", err)
	}

	trailNoReassign := `[{"op":"收件","station":"A","time":"2026-10-05T09:00:00Z"},` +
		`{"op":"出站","station":"A","time":"2026-10-05T09:30:00Z","batch":"B1","courier":"张三"}]`
	cases := map[string]string{
		"续接记录缺少续接轨迹": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trailNoReassign + `}},"handoffs":{},` +
			`"batches":{"B1":{` + oldBatch + `},"B2":{` + newBatch + `}},"reassigns":{"RA1":{` + reassign + `}}}`,
		"续接轨迹缺少续接记录": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
			`"batches":{"B1":{` + oldBatch + `},"B2":{` + newBatch + `}},"reassigns":{}}`,
		"续接轨迹与续接记录不一致": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
			`"batches":{"B1":{` + oldBatch + `},"B2":{` + newBatch + `}},"reassigns":{"RA1":{"request":"RA1","fromBatch":"B1","toBatch":"B2","station":"A","fromCourier":"张三","toCourier":"李四","reason":"别的原因","parcels":["P1"],"time":"2026-10-05T10:00:00Z"}}}`,
		"原批次未标记转交": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
			`"batches":{"B1":{"batch":"B1","station":"A","courier":"张三","parcels":["P1"],"time":"2026-10-05T09:30:00Z","receipts":{}},"B2":{` + newBatch + `}},"reassigns":{"RA1":{` + reassign + `}}}`,
		"新批次未标记续接来源": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
			`"batches":{"B1":{` + oldBatch + `},"B2":{"batch":"B2","station":"A","courier":"李四","parcels":["P1"],"time":"2026-10-05T10:00:00Z","receipts":{}}},"reassigns":{"RA1":{` + reassign + `}}}`,
		"转交集合与未回执成员不符": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
			`"batches":{"B1":{` + oldBatch + `},"B2":{` + newBatch + `}},"reassigns":{"RA1":{"request":"RA1","fromBatch":"B1","toBatch":"B2","station":"A","fromCourier":"张三","toCourier":"李四","reason":"途中交接","parcels":["P1","P2"],"time":"2026-10-05T10:00:00Z"}}}`,
		"续接结果字段不完整": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
			`"batches":{"B1":{` + oldBatch + `},"B2":{` + newBatch + `}},"reassigns":{"RA1":{"request":"RA1","fromBatch":"B1","toBatch":"B2","station":"A","fromCourier":"张三","toCourier":"李四","reason":"","parcels":["P1"],"time":"2026-10-05T10:00:00Z"}}}`,
		"续接结果为空": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
			`"batches":{"B1":{` + oldBatch + `},"B2":{` + newBatch + `}},"reassigns":{"RA1":null}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil {
				t.Fatal("损坏文件必须被拒绝打开")
			} else if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("应返回 ErrCorrupt，得到 %v", err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != content {
				t.Fatal("损坏文件不得被覆盖")
			}
		})
	}
}

func TestCLIEndToEndReassign(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	for _, id := range []string{"P001", "P002", "P003"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记失败: %d", code)
		}
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A", "--courier", "张三",
		"--parcel", "P001", "--parcel", "P002", "--parcel", "P003"); code != 0 {
		t.Fatal("出站失败")
	}
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001", "--result", "签收"); code != 0 {
		t.Fatal("回执失败")
	}

	// 续接成功输出：首次转交集合与续接信息。
	out, _, code := runCLI(t, dbPath, "reassign", "--request", " RA1 ", "--batch", "B1", "--new-batch", "B2",
		"--courier", "李四", "--reason", " 原配送员车辆故障途中交接 ")
	if code != 0 || !strings.Contains(out, "续接成功") ||
		!strings.Contains(out, "续接请求号: RA1") || !strings.Contains(out, "原批次号: B1") ||
		!strings.Contains(out, "新批次号: B2") || !strings.Contains(out, "出发站: 站点A") ||
		!strings.Contains(out, "原配送员: 张三") || !strings.Contains(out, "新配送员: 李四") ||
		!strings.Contains(out, "续接原因: 原配送员车辆故障途中交接") ||
		!strings.Contains(out, "转交包裹（2 件）") || !strings.Contains(out, "- P002") || !strings.Contains(out, "- P003") ||
		!strings.Contains(out, "接手时间: ") {
		t.Fatalf("续接输出不符: code=%d out=%s", code, out)
	}

	// batch：旧批次已转交，展示真实回执、转交件与转交去向，转交件不列为未回执。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 已转交") ||
		!strings.Contains(out, "转交去向: 批次 B2") || !strings.Contains(out, "续接请求号: RA1") ||
		!strings.Contains(out, "P001    已回执    结果: 签收") ||
		!strings.Contains(out, "P002    已转交") || !strings.Contains(out, "P003    已转交") ||
		strings.Contains(out, "未回执") {
		t.Fatalf("转交后原批次查询不符: code=%d out=%s", code, out)
	}

	// batch：新批次展示来源与接手时间。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B2")
	if code != 0 || !strings.Contains(out, "配送员: 李四") ||
		!strings.Contains(out, "批次来源: 续接自批次 B1") || !strings.Contains(out, "接手时间: ") ||
		!strings.Contains(out, "P002    未回执") || !strings.Contains(out, "P003    未回执") {
		t.Fatalf("新批次查询不符: code=%d out=%s", code, out)
	}

	// query：配送中包裹展示当前批次与配送员，续接轨迹完整可辨认。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P002")
	if code != 0 || !strings.Contains(out, "当前状态: 配送中") ||
		!strings.Contains(out, "当前批次: B2") || !strings.Contains(out, "配送员: 李四") ||
		!strings.Contains(out, "操作: 续接") || !strings.Contains(out, "原批次号: B1") ||
		!strings.Contains(out, "新批次号: B2") || !strings.Contains(out, "续接请求号: RA1") ||
		!strings.Contains(out, "原因: 原配送员车辆故障途中交接") {
		t.Fatalf("续接后查询不符: code=%d out=%s", code, out)
	}

	// 同号同内容重放：返回首次结果，不追加续接轨迹。
	out, _, code = runCLI(t, dbPath, "reassign", "--request", "RA1", "--batch", "B1", "--new-batch", "B2",
		"--courier", "李四", "--reason", "原配送员车辆故障途中交接")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("重复续接应重放: code=%d out=%s", code, out)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P002")
	if strings.Count(out, "操作: 续接") != 1 {
		t.Fatalf("重放不得追加续接轨迹:\n%s", out)
	}

	// 同号换内容：冲突。
	_, errText, code := runCLI(t, dbPath, "reassign", "--request", "RA1", "--batch", "B1", "--new-batch", "B2",
		"--courier", "李四", "--reason", "别的原因")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("续接请求号内容冲突应失败: code=%d err=%s", code, errText)
	}

	// 旧批次不能再次续接、不能中止。
	_, errText, code = runCLI(t, dbPath, "reassign", "--request", "RA2", "--batch", "B1", "--new-batch", "B9",
		"--courier", "王五", "--reason", "再次续接")
	if code != exitBusiness || !strings.Contains(errText, "已整批续接转交") {
		t.Fatalf("再次续接旧批次应失败: code=%d err=%s", code, errText)
	}
	_, errText, code = runCLI(t, dbPath, "abort", "--request", "A1", "--batch", "B1", "--reason", "尝试中止")
	if code != exitBusiness || !strings.Contains(errText, "已整批续接转交") {
		t.Fatalf("中止已转交批次应失败: code=%d err=%s", code, errText)
	}

	// 转交件不能首次回执旧批次。
	_, errText, code = runCLI(t, dbPath, "receipt", "--request", "RC2", "--batch", "B1", "--parcel", "P002", "--result", "签收")
	if code != exitBusiness || !strings.Contains(errText, "不在批次") {
		t.Fatalf("转交件回执旧批次应失败: code=%d err=%s", code, errText)
	}

	// dispatch 使用续接创建的批次号报冲突。
	_, errText, code = runCLI(t, dbPath, "dispatch", "--batch", "B2", "--station", "站点A", "--courier", "李四", "--parcel", "P002")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("dispatch 使用续接批次号应失败: code=%d err=%s", code, errText)
	}

	// 新批次可正常回执。
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC3", "--batch", "B2", "--parcel", "P002", "--result", "签收"); code != 0 {
		t.Fatal("新批次回执失败")
	}
}

func TestCLIReassignValidation(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	if _, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A"); code != 0 {
		t.Fatal("登记失败")
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A", "--courier", "张三", "--parcel", "P001"); code != 0 {
		t.Fatal("出站失败")
	}

	cases := []struct {
		name string
		args []string
	}{
		{"请求号为空", []string{"reassign", "--request", "  ", "--batch", "B1", "--new-batch", "B2", "--courier", "李四", "--reason", "原因"}},
		{"原批次号为空", []string{"reassign", "--request", "RA1", "--batch", " ", "--new-batch", "B2", "--courier", "李四", "--reason", "原因"}},
		{"新批次号为空", []string{"reassign", "--request", "RA1", "--batch", "B1", "--new-batch", "  ", "--courier", "李四", "--reason", "原因"}},
		{"新配送员为空", []string{"reassign", "--request", "RA1", "--batch", "B1", "--new-batch", "B2", "--courier", " ", "--reason", "原因"}},
		{"原因为空", []string{"reassign", "--request", "RA1", "--batch", "B1", "--new-batch", "B2", "--courier", "李四", "--reason", "  "}},
		{"原新批次号相同", []string{"reassign", "--request", "RA1", "--batch", "B1", "--new-batch", "B1", "--courier", "李四", "--reason", "原因"}},
		{"原批次不存在", []string{"reassign", "--request", "RA1", "--batch", "NOPE", "--new-batch", "B2", "--courier", "李四", "--reason", "原因"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, code := runCLI(t, dbPath, c.args...); code != exitBusiness {
				t.Fatalf("应返回业务失败退出码 1，得到 %d", code)
			}
		})
	}
	// 失败不占号：纠正后同请求号可成功。
	out, _, code := runCLI(t, dbPath, "reassign", "--request", "RA1", "--batch", "B1", "--new-batch", "B2",
		"--courier", "李四", "--reason", "途中交接")
	if code != 0 || !strings.Contains(out, "续接成功") {
		t.Fatalf("纠正后同请求号续接应成功: code=%d out=%s", code, out)
	}
}

func TestCLIReassignHelpVariants(t *testing.T) {
	for _, args := range [][]string{
		{"reassign", "--help"},
		{"reassign", "-h"},
	} {
		out, _, code := runCLI(t, "", args...)
		if code != 0 || !strings.Contains(out, "reassign") || !strings.Contains(out, "--request") ||
			!strings.Contains(out, "--batch") || !strings.Contains(out, "--new-batch") ||
			!strings.Contains(out, "--courier") || !strings.Contains(out, "--reason") {
			t.Fatalf("reassign 帮助不符: args=%v code=%d out=%s", args, code, out)
		}
	}
	// 总帮助包含 reassign。
	out, _, code := runCLI(t, "")
	if code != 0 || !strings.Contains(out, "reassign") {
		t.Fatalf("总帮助应包含 reassign: code=%d", code)
	}
}

func TestLegacyLedgerWithoutReassignsLoads(t *testing.T) {
	// 早期版本的数据文件没有 reassigns 字段与批次转交标记，应无需手工修改即可使用。
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	legacy := `{"version":1,"parcels":{"P1":{"id":"P1","station":"A","status":"配送中",` +
		`"registered":"2026-10-05T09:00:00Z","trail":[{"op":"收件","station":"A","time":"2026-10-05T09:00:00Z"},` +
		`{"op":"出站","station":"A","time":"2026-10-05T09:30:00Z","batch":"B1","courier":"张三"}]}},` +
		`"handoffs":{},"batches":{"B1":{"batch":"B1","station":"A","courier":"张三","parcels":["P1"],` +
		`"time":"2026-10-05T09:30:00Z","receipts":{}}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("旧台账应可直接打开: %v", err)
	}
	res, replayed, err := s.Reassign("RA1", "B1", "B2", "李四", "途中交接", tClock(2026, 10, 5, 10, 0))
	if err != nil || replayed || len(res.Parcels) != 1 || res.Parcels[0] != "P1" {
		t.Fatalf("旧台账升级后应可续接: %v replayed=%v res=%+v", err, replayed, res)
	}
}
