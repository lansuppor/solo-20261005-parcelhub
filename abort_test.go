package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustAbort(t *testing.T, s *Store, request, batch, reason string, now time.Time) *AbortResult {
	t.Helper()
	res, replayed, err := s.Abort(request, batch, reason, now)
	if err != nil || replayed {
		t.Fatalf("Abort(%q) 意外失败: %v replayed=%v", request, err, replayed)
	}
	return res
}

func TestAbortSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002", "P003"}, tClock(2026, 10, 5, 9, 30))
	// P001 先签收：已回执成员及其回执完全保留。
	if _, _, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0)); err != nil {
		t.Fatal(err)
	}

	t2 := tClock(2026, 10, 5, 11, 0)
	res := mustAbort(t, s, "A1", "B1", "车辆故障", t2)
	if res.Request != "A1" || res.Batch != "B1" || res.Station != "站点A" ||
		res.Reason != "车辆故障" || !res.Time.Equal(t2) {
		t.Fatalf("中止结果不符: %+v", res)
	}
	if len(res.Parcels) != 2 || res.Parcels[0] != "P002" || res.Parcels[1] != "P003" {
		t.Fatalf("收回集合应为未回执成员、按原成员顺序: %v", res.Parcels)
	}

	// 收回件恢复在站、站点为出发站，各追加一条可辨认的收回轨迹。
	for _, pid := range []string{"P002", "P003"} {
		p, _ := s.Query(pid)
		if p.Station != "站点A" || p.Status != statusInStation {
			t.Fatalf("收回后 %s 应在出发站在站: %+v", pid, p)
		}
		if len(p.Trail) != 3 {
			t.Fatalf("%s 应有三条轨迹，得到 %d", pid, len(p.Trail))
		}
		e := p.Trail[2]
		if e.Op != "收回" || e.Station != "站点A" || e.Batch != "B1" ||
			e.Request != "A1" || e.Reason != "车辆故障" || !e.Time.Equal(t2) {
			t.Fatalf("%s 收回轨迹不符: %+v", pid, e)
		}
	}

	// 已回执成员不动：状态、轨迹、批次回执保留。
	p1, _ := s.Query("P001")
	if p1.Status != statusSigned || len(p1.Trail) != 3 || p1.Trail[2].Op != "回执" {
		t.Fatalf("已回执成员不得被改变: %+v", p1)
	}
	b, _ := s.BatchQuery("B1")
	if b.AbortedBy != "A1" || len(b.Receipts) != 1 || b.Receipts["P001"].Request != "RC1" {
		t.Fatalf("批次应标记中止且保留真实回执: %+v", b)
	}
	if b.Courier != "张三" || len(b.Parcels) != 3 || b.Parcels[0] != "P001" {
		t.Fatalf("批次原成员顺序与配送员必须保留: %+v", b)
	}
	ab := s.BatchAbort("B1")
	if ab != res {
		t.Fatalf("BatchAbort 应返回中止结果: %+v", ab)
	}
	if s.BatchAbort("NOPE") != nil {
		t.Fatal("不存在的批次应无中止结果")
	}
}

func TestAbortFailuresAtomic(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
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
		name    string
		request string
		batch   string
		reason  string
	}{
		{"批次不存在", "A1", "NOPE", "原因"},
		{"已完成批次不能中止", "A2", "B2", "原因"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Abort(c.request, c.batch, c.reason, tClock(2026, 10, 5, 11, 0)); err == nil {
				t.Fatal("条件不满足时中止必须失败")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("失败中止不得写入数据文件")
			}
			if _, ok := s.data.Aborts[c.request]; ok {
				t.Fatalf("失败的首次中止不得占用请求号 %q", c.request)
			}
		})
	}

	// 每批只能成功中止一次：换请求号再中止拒绝，且不占号。
	mustAbort(t, s, "A3", "B1", "车辆故障", tClock(2026, 10, 5, 11, 0))
	if _, _, err := s.Abort("A4", "B1", "再次中止", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("已中止批次换请求号再中止必须拒绝")
	}
	if _, ok := s.data.Aborts["A4"]; ok {
		t.Fatal("被拒绝的中止不得占用请求号")
	}
	// 失败不占号：A1/A2 之后仍可用于新的有效中止。
	mustRegister(t, s, "P003", "站点A", t1)
	mustDispatch(t, s, "B3", "站点A", "王五", []string{"P003"}, tClock(2026, 10, 5, 12, 30))
	if res := mustAbort(t, s, "A1", "B3", "原因", tClock(2026, 10, 5, 13, 0)); res.Batch != "B3" {
		t.Fatalf("失败不占号，纠正后同请求号应可成功: %+v", res)
	}
}

func TestAbortReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustDispatch(t, s, "B2", "站点A", "李四", []string{"P002"}, tClock(2026, 10, 5, 9, 40))
	t2 := tClock(2026, 10, 5, 10, 0)
	first := mustAbort(t, s, "A1", "B1", "车辆故障", t2)

	// 同号、同批次、同原因重放：返回首次集合与时间，不检查当前状态、不追加轨迹。
	// （先让收回件进入新批次，证明重放不重新计算成员。）
	mustDispatch(t, s, "B3", "站点A", "王五", []string{"P001"}, tClock(2026, 10, 5, 10, 30))
	res, replayed, err := s.Abort("A1", "B1", "车辆故障", tClock(2026, 10, 5, 11, 0))
	if err != nil || !replayed {
		t.Fatalf("同内容重放应成功: %v replayed=%v", err, replayed)
	}
	if res != first || !res.Time.Equal(t2) || len(res.Parcels) != 1 || res.Parcels[0] != "P001" {
		t.Fatalf("重放应返回首次集合与时间: %+v", res)
	}
	p, _ := s.Query("P001")
	count := 0
	for _, e := range p.Trail {
		if e.Op == "收回" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("重放不得追加收回轨迹，得到 %d 条", count)
	}

	// 同号换批次或换原因：冲突，已有结果不变。
	if _, _, err := s.Abort("A1", "B2", "车辆故障", tClock(2026, 10, 5, 11, 30)); err == nil {
		t.Fatal("同号换批次必须报冲突")
	}
	if _, _, err := s.Abort("A1", "B1", "别的原因", tClock(2026, 10, 5, 11, 45)); err == nil {
		t.Fatal("同号换原因必须报冲突")
	}
	if s.data.Aborts["A1"] != first {
		t.Fatal("冲突不得改写已有中止结果")
	}

	// 中止请求号独立去重：可与其他业务编号同名。
	if res := mustAbort(t, s, "B2", "B2", "同名请求号", tClock(2026, 10, 5, 12, 0)); res.Request != "B2" {
		t.Fatalf("中止请求号可与批次号同名: %+v", res)
	}
}

func TestAbortReceiptRejectedAfterAbort(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustAbort(t, s, "A1", "B1", "车辆故障", tClock(2026, 10, 5, 10, 0))

	// 收回件不能首次回执旧批次：receipt 拒绝。
	if _, _, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 30)); err == nil {
		t.Fatal("收回件不得首次回执旧批次")
	}
	if _, ok := s.data.Receipts["RC1"]; ok {
		t.Fatal("被拒绝的回执不得占用请求号")
	}

	// receipt-import 含迟到回执：整份拒绝，仅撤销此次新增，新号不占用。
	mustRegister(t, s, "P003", "站点A", t1)
	mustDispatch(t, s, "B2", "站点A", "李四", []string{"P003"}, tClock(2026, 10, 5, 11, 0))
	records := []ReceiptImportRecord{
		{Request: "RC2", Batch: "B2", Parcel: "P003", Result: resultSigned},
		{Request: "RC3", Batch: "B1", Parcel: "P002", Result: resultFailed, Reason: "迟到回执"},
	}
	if _, err := s.ImportReceipts(records, tClock(2026, 10, 5, 11, 30)); err == nil {
		t.Fatal("含迟到回执的导入必须整份拒绝")
	}
	if _, ok := s.data.Receipts["RC2"]; ok {
		t.Fatal("整份拒绝时此次新增必须撤销，新号不得占用")
	}
	if _, ok := s.data.Receipts["RC3"]; ok {
		t.Fatal("迟到回执不得占用请求号")
	}
	p3, _ := s.Query("P003")
	if p3.Status != statusDelivering || len(p3.Trail) != 2 {
		t.Fatalf("整份拒绝不得改动其他包裹: %+v", p3)
	}
	// 新号未占用：纠正后可重新导入。
	if _, err := s.ImportReceipts(records[:1], tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatalf("纠正后导入应成功: %v", err)
	}

	// 已有回执的同内容重放仍返回历史结果，不移动包裹或重开批次。
	if _, replayed, err := s.Receipt("RC2", "B2", "P003", resultSigned, "", tClock(2026, 10, 5, 12, 30)); err != nil || !replayed {
		t.Fatalf("已有回执同内容重放应返回历史结果: %v replayed=%v", err, replayed)
	}
	b1, _ := s.BatchQuery("B1")
	if b1.AbortedBy != "A1" {
		t.Fatal("重放不得重开已中止批次")
	}
}

func TestAbortRecalledParcelFlows(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	// 三件包裹先交接到站点B，随后从站点B 出站。
	mustHandoff(t, s, "H1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 9, 10))
	mustHandoff(t, s, "H2", "站点A", "站点B", []string{"P002", "P003"}, tClock(2026, 10, 5, 9, 20))
	mustDispatch(t, s, "B1", "站点B", "张三", []string{"P001", "P002", "P003"}, tClock(2026, 10, 5, 9, 30))
	mustAbort(t, s, "A1", "B1", "车辆故障", tClock(2026, 10, 5, 10, 0))

	// 收回算新流转：不能因此退回出站前的旧交接。
	if _, _, err := s.Return("RT1", "H1", "错发", tClock(2026, 10, 5, 10, 30)); err == nil {
		t.Fatal("收回后不得退回出站前的旧交接")
	}

	// 收回件可交接。
	mustHandoff(t, s, "H3", "站点B", "站点C", []string{"P002"}, tClock(2026, 10, 5, 11, 0))
	// 收回件可冻结。
	mustFreeze(t, s, "E1", "P003", "站点B", "外包装破损", tClock(2026, 10, 5, 11, 30))
	// 收回件可再次出站。
	mustDispatch(t, s, "B2", "站点B", "李四", []string{"P001"}, tClock(2026, 10, 5, 12, 0))
	// 再次出站后仍不能首次回执旧批次。
	if _, _, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatal("再次出站后仍不得首次回执旧批次")
	}
	// 新批次回执正常。
	if _, _, err := s.Receipt("RC2", "B2", "P001", resultSigned, "", tClock(2026, 10, 5, 13, 0)); err != nil {
		t.Fatalf("新批次回执应正常: %v", err)
	}
}

func TestAbortReceiptedMembersUntouched(t *testing.T) {
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

	// 已回执成员被冻结或进入其他批次都不阻止中止，也不检查或改变其状态。
	res := mustAbort(t, s, "A1", "B1", "车辆故障", tClock(2026, 10, 5, 12, 0))
	if len(res.Parcels) != 1 || res.Parcels[0] != "P003" {
		t.Fatalf("收回集合应只含未回执成员 P003: %v", res.Parcels)
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

func TestAbortPersistenceAcrossRestart(t *testing.T) {
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
	mustAbort(t, s1, "A1", "B1", "车辆故障", tClock(2026, 10, 5, 11, 0))

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	p, _ := s2.Query("P002")
	if p.Status != statusInStation || len(p.Trail) != 3 || p.Trail[2].Op != "收回" || p.Trail[2].Request != "A1" {
		t.Fatalf("重启后收回轨迹不符: %+v", p)
	}
	// 重启后同内容重放仍成立。
	res, replayed, err := s2.Abort("A1", "B1", "车辆故障", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || len(res.Parcels) != 1 || res.Parcels[0] != "P002" {
		t.Fatalf("重启后中止重放失败: %v replayed=%v res=%+v", err, replayed, res)
	}
	// 重启后换请求号再中止仍拒绝。
	if _, _, err := s2.Abort("A2", "B1", "再来", tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatal("重启后已中止批次仍须拒绝再次中止")
	}
	// 重启后收回件仍不能首次回执旧批次。
	if _, _, err := s2.Receipt("RC2", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("重启后收回件仍须拒绝首次回执旧批次")
	}
	// 原 dispatch 的同内容重放仍返回历史结果，不移动包裹或重开批次。
	if _, replayedD, err := s2.Dispatch("B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 13, 15)); err != nil || !replayedD {
		t.Fatalf("原批次同内容重放应返回历史结果: %v replayed=%v", err, replayedD)
	}
	if p, _ := s2.Query("P002"); p.Status != statusInStation {
		t.Fatalf("dispatch 重放不得移动包裹或重开批次: %+v", p)
	}
	if b, _ := s2.BatchQuery("B1"); b.AbortedBy != "A1" {
		t.Fatal("dispatch 重放不得清除中止标记")
	}
	// 重启后收回件可再次出站。
	mustDispatch(t, s2, "B2", "站点A", "李四", []string{"P002"}, tClock(2026, 10, 5, 13, 30))
}

func TestCorruptAbortDataRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")

	parcel := `"id":"P1","station":"A","status":"在站","registered":"2026-10-05T09:00:00Z"`
	trail := `[{"op":"收件","station":"A","time":"2026-10-05T09:00:00Z"},` +
		`{"op":"出站","station":"A","time":"2026-10-05T09:30:00Z","batch":"B1","courier":"张三"},` +
		`{"op":"收回","station":"A","time":"2026-10-05T10:00:00Z","batch":"B1","request":"A1","reason":"车辆故障"}]`
	batch := `"batch":"B1","station":"A","courier":"张三","parcels":["P1"],"time":"2026-10-05T09:30:00Z","receipts":{},"abortedBy":"A1"`
	abort := `"request":"A1","batch":"B1","station":"A","reason":"车辆故障","parcels":["P1"],"time":"2026-10-05T10:00:00Z"`
	good := `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
		`"batches":{"B1":{` + batch + `}},"aborts":{"A1":{` + abort + `}}}`

	// 完好的中止台账可以直接打开。
	if err := os.WriteFile(path, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("有效中止台账应可打开: %v", err)
	}

	trailNoRecall := `[{"op":"收件","station":"A","time":"2026-10-05T09:00:00Z"},` +
		`{"op":"出站","station":"A","time":"2026-10-05T09:30:00Z","batch":"B1","courier":"张三"}]`
	cases := map[string]string{
		"中止记录缺少收回轨迹": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trailNoRecall + `}},"handoffs":{},` +
			`"batches":{"B1":{` + batch + `}},"aborts":{"A1":{` + abort + `}}}`,
		"收回轨迹缺少中止记录": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
			`"batches":{"B1":{` + batch + `}},"aborts":{}}`,
		"收回轨迹与中止记录不一致": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
			`"batches":{"B1":{` + batch + `}},"aborts":{"A1":{"request":"A1","batch":"B1","station":"A","reason":"别的原因","parcels":["P1"],"time":"2026-10-05T10:00:00Z"}}}`,
		"批次未标记中止": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
			`"batches":{"B1":{"batch":"B1","station":"A","courier":"张三","parcels":["P1"],"time":"2026-10-05T09:30:00Z","receipts":{}}},"aborts":{"A1":{` + abort + `}}}`,
		"批次标记了不存在的中止": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
			`"batches":{"B1":{"batch":"B1","station":"A","courier":"张三","parcels":["P1"],"time":"2026-10-05T09:30:00Z","receipts":{},"abortedBy":"NOPE"}},"aborts":{"A1":{` + abort + `}}}`,
		"收回集合与未回执成员不符": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
			`"batches":{"B1":{` + batch + `}},"aborts":{"A1":{"request":"A1","batch":"B1","station":"A","reason":"车辆故障","parcels":["P1","P2"],"time":"2026-10-05T10:00:00Z"}}}`,
		"中止结果字段不完整": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":` + trail + `}},"handoffs":{},` +
			`"batches":{"B1":{` + batch + `}},"aborts":{"A1":{"request":"A1","batch":"B1","station":"A","reason":"","parcels":["P1"],"time":"2026-10-05T10:00:00Z"}}}`,
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

func TestCLIEndToEndAbort(t *testing.T) {
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

	// 中止成功输出：首次收回集合与中止信息。
	out, _, code := runCLI(t, dbPath, "abort", "--request", " A1 ", "--batch", "B1", "--reason", " 车辆故障全部收回 ")
	if code != 0 || !strings.Contains(out, "中止成功") ||
		!strings.Contains(out, "中止请求号: A1") || !strings.Contains(out, "批次号: B1") ||
		!strings.Contains(out, "出发站: 站点A") || !strings.Contains(out, "中止原因: 车辆故障全部收回") ||
		!strings.Contains(out, "收回包裹（2 件）") || !strings.Contains(out, "- P002") || !strings.Contains(out, "- P003") {
		t.Fatalf("中止输出不符: code=%d out=%s", code, out)
	}

	// batch：已中止，展示真实回执、收回成员与中止信息，收回件不列为未回执。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 已中止") ||
		!strings.Contains(out, "中止请求号: A1") || !strings.Contains(out, "中止原因: 车辆故障全部收回") ||
		!strings.Contains(out, "P001    已回执    结果: 签收") ||
		!strings.Contains(out, "P002    已收回") || !strings.Contains(out, "P003    已收回") ||
		strings.Contains(out, "未回执") {
		t.Fatalf("中止后批次查询不符: code=%d out=%s", code, out)
	}

	// query：收回轨迹完整可辨认。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P002")
	if code != 0 || !strings.Contains(out, "当前状态: 在站") ||
		!strings.Contains(out, "操作: 收回") || !strings.Contains(out, "批次号: B1") ||
		!strings.Contains(out, "中止请求号: A1") || !strings.Contains(out, "原因: 车辆故障全部收回") {
		t.Fatalf("收回后查询不符: code=%d out=%s", code, out)
	}

	// 同号同内容重放：返回首次结果，不追加收回轨迹。
	out, _, code = runCLI(t, dbPath, "abort", "--request", "A1", "--batch", "B1", "--reason", "车辆故障全部收回")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("重复中止应重放: code=%d out=%s", code, out)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P002")
	if strings.Count(out, "操作: 收回") != 1 {
		t.Fatalf("重放不得追加收回轨迹:\n%s", out)
	}

	// 同号换内容：冲突。
	_, errText, code := runCLI(t, dbPath, "abort", "--request", "A1", "--batch", "B1", "--reason", "别的原因")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("中止请求号内容冲突应失败: code=%d err=%s", code, errText)
	}

	// 换请求号再中止：拒绝。
	_, errText, code = runCLI(t, dbPath, "abort", "--request", "A2", "--batch", "B1", "--reason", "再次中止")
	if code != exitBusiness || !strings.Contains(errText, "已中止") {
		t.Fatalf("重复中止批次应失败: code=%d err=%s", code, errText)
	}

	// 收回件不能首次回执旧批次。
	_, errText, code = runCLI(t, dbPath, "receipt", "--request", "RC2", "--batch", "B1", "--parcel", "P002", "--result", "签收")
	if code != exitBusiness || !strings.Contains(errText, "不在批次") {
		t.Fatalf("收回件回执旧批次应失败: code=%d err=%s", code, errText)
	}

	// 收回件可再次出站并正常回执新批次。
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B2", "--station", "站点A", "--courier", "李四",
		"--parcel", "P002", "--parcel", "P003"); code != 0 {
		t.Fatal("收回件再次出站失败")
	}
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC3", "--batch", "B2", "--parcel", "P002", "--result", "签收"); code != 0 {
		t.Fatal("新批次回执失败")
	}
}

func TestCLIAbortValidation(t *testing.T) {
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
		{"请求号为空", []string{"abort", "--request", "  ", "--batch", "B1", "--reason", "原因"}},
		{"批次号为空", []string{"abort", "--request", "A1", "--batch", " ", "--reason", "原因"}},
		{"原因为空", []string{"abort", "--request", "A1", "--batch", "B1", "--reason", "  "}},
		{"批次不存在", []string{"abort", "--request", "A1", "--batch", "NOPE", "--reason", "原因"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, code := runCLI(t, dbPath, c.args...); code != exitBusiness {
				t.Fatalf("应返回业务失败退出码 1，得到 %d", code)
			}
		})
	}
	// 失败不占号：纠正后同请求号可成功。
	out, _, code := runCLI(t, dbPath, "abort", "--request", "A1", "--batch", "B1", "--reason", "车辆故障")
	if code != 0 || !strings.Contains(out, "中止成功") {
		t.Fatalf("纠正后同请求号中止应成功: code=%d out=%s", code, out)
	}
}

func TestCLIAbortHelpVariants(t *testing.T) {
	for _, args := range [][]string{
		{"abort", "--help"},
		{"abort", "-h"},
	} {
		out, _, code := runCLI(t, "", args...)
		if code != 0 || !strings.Contains(out, "abort") || !strings.Contains(out, "--request") ||
			!strings.Contains(out, "--batch") || !strings.Contains(out, "--reason") {
			t.Fatalf("abort 帮助不符: args=%v code=%d out=%s", args, code, out)
		}
	}
	// 总帮助包含 abort。
	out, _, code := runCLI(t, "")
	if code != 0 || !strings.Contains(out, "abort") {
		t.Fatalf("总帮助应包含 abort: code=%d", code)
	}
}

func TestLegacyLedgerWithoutAbortsLoads(t *testing.T) {
	// 早期版本的数据文件没有 aborts 字段与批次中止标记，应无需手工修改即可使用。
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
	res, replayed, err := s.Abort("A1", "B1", "车辆故障", tClock(2026, 10, 5, 10, 0))
	if err != nil || replayed || len(res.Parcels) != 1 || res.Parcels[0] != "P1" {
		t.Fatalf("旧台账升级后应可中止: %v replayed=%v res=%+v", err, replayed, res)
	}
}
