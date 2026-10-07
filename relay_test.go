package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func mustTransfer(t *testing.T, s *Store, request, fromBatch, toBatch, courier, reason string, now time.Time) *TransferResult {
	t.Helper()
	res, replayed, err := s.Transfer(request, fromBatch, toBatch, courier, reason, nil, now)
	if err != nil || replayed {
		t.Fatalf("Transfer(%q) 意外失败: %v replayed=%v", request, err, replayed)
	}
	return res
}

// mustTransferParcels 以显式集合方式续接，要求首次受理成功。
func mustTransferParcels(t *testing.T, s *Store, request, fromBatch, toBatch, courier, reason string, parcels []string, now time.Time) *TransferResult {
	t.Helper()
	res, replayed, err := s.Transfer(request, fromBatch, toBatch, courier, reason, parcels, now)
	if err != nil || replayed {
		t.Fatalf("Transfer(%q, %v) 意外失败: %v replayed=%v", request, parcels, err, replayed)
	}
	return res
}

func mustReceiptOK(t *testing.T, s *Store, request, batch, parcel, result, reason string, now time.Time) {
	t.Helper()
	if _, _, err := s.Receipt(request, batch, parcel, result, reason, now); err != nil {
		t.Fatalf("Receipt(%q) 意外失败: %v", request, err)
	}
}

// 准备一个在站点A 出发、张三配送的批次 B1（P001、P002、P003），P001 已签收。
func setupRelayBase(t *testing.T, s *Store) {
	t.Helper()
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002", "P003"}, tClock(2026, 10, 5, 9, 30))
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
}

func TestTransferSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	setupRelayBase(t, s)

	t2 := tClock(2026, 10, 5, 11, 0)
	res := mustTransfer(t, s, "T1", "B1", "B2", "李四", "原配送员车辆故障", t2)
	if res.Request != "T1" || res.FromBatch != "B1" || res.ToBatch != "B2" ||
		res.Station != "站点A" || res.FromCourier != "张三" || res.ToCourier != "李四" ||
		res.Reason != "原配送员车辆故障" || !res.Time.Equal(t2) {
		t.Fatalf("续接结果不符: %+v", res)
	}
	if len(res.Parcels) != 2 || res.Parcels[0] != "P002" || res.Parcels[1] != "P003" {
		t.Fatalf("转交集合应为未回执成员、按原成员顺序: %v", res.Parcels)
	}

	// 转交件保持配送中及原站点，当前配送归属切换到新批次，各追加续接轨迹。
	for _, pid := range []string{"P002", "P003"} {
		p, _ := s.Query(pid)
		if p.Station != "站点A" || p.Status != statusDelivering {
			t.Fatalf("续接后 %s 应保持配送中、站点不变: %+v", pid, p)
		}
		if currentBatch(p) != "B2" {
			t.Fatalf("%s 当前配送归属应切换到 B2", pid)
		}
		if len(p.Trail) != 3 {
			t.Fatalf("%s 应有三条轨迹，得到 %d", pid, len(p.Trail))
		}
		e := p.Trail[2]
		if e.Op != "续接" || e.Station != "站点A" || e.FromBatch != "B1" || e.Batch != "B2" ||
			e.Courier != "李四" || e.Request != "T1" || e.Reason != "原配送员车辆故障" || !e.Time.Equal(t2) {
			t.Fatalf("%s 续接轨迹不符: %+v", pid, e)
		}
	}

	// 已回执成员不动。
	p1, _ := s.Query("P001")
	if p1.Status != statusSigned || len(p1.Trail) != 3 || p1.Trail[2].Op != "回执" {
		t.Fatalf("已回执成员不得被改变: %+v", p1)
	}

	// 原批次永久关闭为已转交，原成员、配送员、出站时间与真实回执保留。
	old, _ := s.BatchQuery("B1")
	if old.TransferredBy != "T1" || old.Courier != "张三" || len(old.Parcels) != 3 ||
		len(old.Receipts) != 1 || old.Receipts["P001"].Request != "RC1" {
		t.Fatalf("原批次应标记转交且保留原信息: %+v", old)
	}

	// 新批次按原顺序接纳全部未回执件，沿用出发站，记录新配送员及接手时间。
	nb, _ := s.BatchQuery("B2")
	if nb.Station != "站点A" || nb.Courier != "李四" || !nb.Time.Equal(t2) ||
		nb.RelayedFrom != "B1" || nb.RelayRequest != "T1" ||
		len(nb.Parcels) != 2 || nb.Parcels[0] != "P002" || nb.Parcels[1] != "P003" {
		t.Fatalf("新批次不符: %+v", nb)
	}
	if s.BatchTransferSource("B1") != res || s.TransferOf("T1") != res {
		t.Fatal("BatchTransferSource/TransferOf 应返回续接结果")
	}
	if s.BatchTransferSource("B2") != nil || s.TransferOf("NOPE") != nil {
		t.Fatal("未转交批次或不存在的请求号应无续接结果")
	}
}

func TestTransferReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	setupRelayBase(t, s)
	t2 := tClock(2026, 10, 5, 11, 0)
	first := mustTransfer(t, s, "T1", "B1", "B2", "李四", "车辆故障", t2)

	// 续接后新批次继续流转：回执一件、再次续接。
	mustReceiptOK(t, s, "RC2", "B2", "P002", resultSigned, "", tClock(2026, 10, 5, 12, 0))
	mustTransfer(t, s, "T2", "B2", "B3", "王五", "再次转交", tClock(2026, 10, 5, 13, 0))

	// 同号同内容重放：返回首次集合、续接信息及时间，不检查现状、不重算成员、不改写台账。
	before, _ := os.ReadFile(s.path)
	got, replayed, err := s.Transfer("T1", "B1", "B2", "李四", "车辆故障", nil, tClock(2026, 10, 5, 14, 0))
	if err != nil || !replayed || got != first {
		t.Fatalf("同内容重放应返回首次结果: replayed=%v err=%v got=%+v", replayed, err, got)
	}
	if len(got.Parcels) != 2 || got.Parcels[0] != "P002" {
		t.Fatalf("重放应返回首次转交集合: %v", got.Parcels)
	}
	after, _ := os.ReadFile(s.path)
	if string(before) != string(after) {
		t.Fatal("重放不得改写台账")
	}

	// 换内容冲突，且不占新号以外的任何变更。
	for _, c := range []struct{ from, to, courier, reason string }{
		{"B9", "B2", "李四", "车辆故障"},
		{"B1", "B9", "李四", "车辆故障"},
		{"B1", "B2", "王五", "车辆故障"},
		{"B1", "B2", "李四", "别的原因"},
	} {
		if _, _, err := s.Transfer("T1", c.from, c.to, c.courier, c.reason, nil, tClock(2026, 10, 5, 15, 0)); err == nil {
			t.Fatalf("换内容应报冲突: %+v", c)
		}
	}
}

func TestTransferFailuresAtomic(t *testing.T) {
	s, path := openTempStore(t)
	setupRelayBase(t, s)
	// B9：已完成批次；B8：已中止批次；B7：续接创建的新批次号。
	mustRegister(t, s, "P009", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B9", "站点A", "张三", []string{"P009"}, tClock(2026, 10, 5, 9, 40))
	mustReceiptOK(t, s, "RC9", "B9", "P009", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustRegister(t, s, "P008", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B8", "站点A", "张三", []string{"P008"}, tClock(2026, 10, 5, 9, 50))
	mustAbort(t, s, "A8", "B8", "车辆故障", tClock(2026, 10, 5, 10, 30))
	mustTransfer(t, s, "T0", "B1", "B7", "李四", "先行续接", tClock(2026, 10, 5, 10, 40))

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now := tClock(2026, 10, 5, 11, 0)
	cases := []struct {
		name    string
		request string
		from    string
		to      string
		courier string
		reason  string
	}{
		{"原批次不存在", "T1", "NOPE", "B2", "李四", "原因"},
		{"已中止批次不能续接", "T2", "B8", "B2", "李四", "原因"},
		{"已转交批次不能再次续接", "T3", "B1", "B2", "李四", "原因"},
		{"已完成批次没有未回执成员", "T4", "B9", "B2", "李四", "原因"},
		{"新批次号已被出站使用", "T5", "B7", "B9", "王五", "原因"},
		{"新批次号已被续接使用", "T6", "B7", "B7", "王五", "原因"},
		{"新配送员不能与原配送员相同", "T7", "B7", "B2", "李四", "原因"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Transfer(c.request, c.from, c.to, c.courier, c.reason, nil, now); err == nil {
				t.Fatal("条件不满足时续接必须失败")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("失败续接不得写入数据文件")
			}
			if _, ok := s.data.Transfers[c.request]; ok {
				t.Fatalf("失败的首次续接不得占用请求号 %q", c.request)
			}
		})
	}

	// 防御性校验：未回执件不全部在原批次配送中时整批拒绝
	// （公开操作流程不可达，直接构造内存状态验证）。
	p2 := s.data.Parcels["P002"]
	oldStatus := p2.Status
	p2.Status = statusInStation
	if _, _, err := s.Transfer("T8", "B7", "B10", "王五", "原因", nil, now); err == nil {
		t.Fatal("未回执件不在原批次配送中时必须整批拒绝")
	}
	p2.Status = oldStatus
	if _, ok := s.data.Transfers["T8"]; ok {
		t.Fatal("失败的首次续接不得占用请求号")
	}
	if _, ok := s.data.Batches["B10"]; ok {
		t.Fatal("失败的首次续接不得占用新批次号")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("失败续接不得写入数据文件")
	}
}

func TestTransferBlocksOldBatch(t *testing.T) {
	s, _ := openTempStore(t)
	setupRelayBase(t, s)
	mustTransfer(t, s, "T1", "B1", "B2", "李四", "车辆故障", tClock(2026, 10, 5, 11, 0))
	now := tClock(2026, 10, 5, 12, 0)

	// 转交件不能首次回执旧批次。
	if _, _, err := s.Receipt("RC2", "B1", "P002", resultSigned, "", now); err == nil {
		t.Fatal("转交件不能首次回执旧批次")
	}
	if _, ok := s.data.Receipts["RC2"]; ok {
		t.Fatal("失败回执不得占用请求号")
	}

	// receipt-import 含迟到回执须整份拒绝，只撤销此次新增，新号不占用。
	_, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC3", Batch: "B2", Parcel: "P002", Result: resultSigned},
		{Request: "RC4", Batch: "B1", Parcel: "P003", Result: resultSigned},
	}, now)
	if err == nil || !strings.Contains(err.Error(), "第 2 条") {
		t.Fatalf("含迟到回执的导入应整份拒绝并提示位置: %v", err)
	}
	if _, ok := s.data.Receipts["RC3"]; ok {
		t.Fatal("整份拒绝时此次新增必须撤销，新号不占用")
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusDelivering || len(p2.Trail) != 3 {
		t.Fatalf("整份拒绝后台账保持导入前状态: %+v", p2)
	}

	// 旧批次不能首次中止或再次续接。
	if _, _, err := s.Abort("A1", "B1", "原因", now); err == nil {
		t.Fatal("已转交批次不能中止")
	}
	if _, _, err := s.Transfer("T2", "B1", "B3", "王五", "原因", nil, now); err == nil {
		t.Fatal("已转交批次不能再次续接")
	}

	// dispatch 使用续接创建的批次号报冲突。
	if _, _, err := s.Dispatch("B2", "站点A", "李四", []string{"P002", "P003"}, now); err == nil {
		t.Fatal("dispatch 使用续接创建的批次号应报冲突")
	}

	// 原 dispatch 和已有回执同内容重放仍返回旧结果，不移动包裹或重开批次。
	if _, replayed, err := s.Dispatch("B1", "站点A", "张三", []string{"P001", "P002", "P003"}, now); err != nil || !replayed {
		t.Fatalf("原 dispatch 同内容重放应返回旧结果: replayed=%v err=%v", replayed, err)
	}
	if _, replayed, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", now); err != nil || !replayed {
		t.Fatalf("已有回执同内容重放应返回旧结果: replayed=%v err=%v", replayed, err)
	}
	old, _ := s.BatchQuery("B1")
	if old.TransferredBy != "T1" {
		t.Fatal("重放不得重开已转交批次")
	}
	if p, _ := s.Query("P002"); p.Status != statusDelivering || currentBatch(p) != "B2" {
		t.Fatal("重放不得移动包裹")
	}
}

func TestTransferNewBatchLifecycle(t *testing.T) {
	s, _ := openTempStore(t)
	setupRelayBase(t, s)
	mustTransfer(t, s, "T1", "B1", "B2", "李四", "车辆故障", tClock(2026, 10, 5, 11, 0))

	// 新批次可回执（含导入）、中止或再次续接。
	mustReceiptOK(t, s, "RC2", "B2", "P002", resultSigned, "", tClock(2026, 10, 5, 12, 0))
	items, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC3", Batch: "B2", Parcel: "P003", Result: resultFailed, Reason: "收件人不在"},
	}, tClock(2026, 10, 5, 12, 30))
	if err != nil || len(items) != 1 || items[0].Replayed {
		t.Fatalf("新批次应可导入回执: items=%+v err=%v", items, err)
	}
	// P003 失败回站后可再次出站并续接。
	mustDispatch(t, s, "B5", "站点A", "张三", []string{"P003"}, tClock(2026, 10, 5, 13, 0))
	mustTransfer(t, s, "T2", "B5", "B6", "李四", "再次续接", tClock(2026, 10, 5, 13, 30))
	// B2 剩余成员全部回执后已完成，不能中止；另建批次验证新批次可中止。
	mustRegister(t, s, "P010", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B8", "站点A", "张三", []string{"P010"}, tClock(2026, 10, 5, 14, 0))
	mustTransfer(t, s, "T3", "B8", "B9", "李四", "续接", tClock(2026, 10, 5, 14, 30))
	mustAbort(t, s, "A9", "B9", "车辆故障", tClock(2026, 10, 5, 15, 0))
	p10, _ := s.Query("P010")
	if p10.Status != statusInStation || p10.Station != "站点A" {
		t.Fatalf("续接新批次中止后应收回出发站: %+v", p10)
	}
}

func TestTransferIsNewFlow(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustHandoff(t, s, "H1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustDispatch(t, s, "B1", "站点B", "张三", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	mustTransfer(t, s, "T1", "B1", "B2", "李四", "车辆故障", tClock(2026, 10, 5, 11, 0))

	// 续接算新流转，不能退回配送前的旧交接。
	if _, _, err := s.Return("RT1", "H1", "错发站点", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("续接后不能退回配送前的旧交接")
	}
}

func TestTransferPersistenceAndCorrupt(t *testing.T) {
	s, path := openTempStore(t)
	setupRelayBase(t, s)
	mustTransfer(t, s, "T1", "B1", "B2", "李四", "车辆故障", tClock(2026, 10, 5, 11, 0))

	// 重启后规则不变：重放仍返回首次结果，旧批次仍不能回执转交件。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开台账失败: %v", err)
	}
	if got, replayed, err := s2.Transfer("T1", "B1", "B2", "李四", "车辆故障", nil, tClock(2026, 10, 5, 12, 0)); err != nil || !replayed ||
		got.ToCourier != "李四" || len(got.Parcels) != 2 {
		t.Fatalf("重启后重放应返回首次结果: replayed=%v err=%v got=%+v", replayed, err, got)
	}
	if _, _, err := s2.Receipt("RC2", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("重启后转交件仍不能首次回执旧批次")
	}

	// 续接关联结果缺失或与批次及轨迹不一致时拒绝读写。
	raw, _ := os.ReadFile(path)
	var tampered []byte
	// 删除台账顶层 transfers 表（批次与轨迹仍引用续接请求），必须判为损坏。
	// 注意批次内也有嵌套的 "transfers" 数组，这里须精确匹配顶层对象键。
	tampered = []byte(strings.Replace(string(raw), `"transfers": {`, `"xtransfers": {`, 1))
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("续接结果缺失应拒绝读写: %v", err)
	}
	// 篡改续接结果内容（新配送员不一致）也应判为损坏。
	tampered = []byte(strings.Replace(string(raw), `"toCourier": "李四"`, `"toCourier": "王五"`, 1))
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("续接结果与轨迹不一致应拒绝读写: %v", err)
	}
}
