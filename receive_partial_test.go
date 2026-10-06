package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mustReceiveParcels 以显式集合方式接收，要求首次受理成功。
func mustReceiveParcels(t *testing.T, s *Store, request, shipment, station string, parcels []string, now time.Time) *ReceiveResult {
	t.Helper()
	res, replayed, err := s.Receive(request, shipment, station, parcels, now)
	if err != nil || replayed {
		t.Fatalf("Receive(%q, %v) 意外失败: %v replayed=%v", request, parcels, err, replayed)
	}
	return res
}

// 准备三件在站点A 在站的包裹 P001、P002、P003，并发运 S1（站点A -> 站点B）。
func setupPartialShip(t *testing.T, s *Store) time.Time {
	t.Helper()
	t1 := tClock(2026, 10, 5, 9, 0)
	for _, id := range []string{"P001", "P002", "P003"} {
		mustRegister(t, s, id, "站点A", t1)
	}
	shipTime := tClock(2026, 10, 5, 10, 0)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002", "P003"}, shipTime)
	return shipTime
}

// 分批接收：显式集合先到件入站作业，余件保持在途及作业限制；
// 已收件的后续交接、冻结不妨碍余件补齐；全部到齐后运输单标记已接收。
func TestReceivePartialExplicit(t *testing.T) {
	s, _ := openTempStore(t)
	setupPartialShip(t, s)

	recvTime := tClock(2026, 10, 5, 12, 0)
	res := mustReceiveParcels(t, s, "RS1", "S1", "站点B", []string{"P003", "P001"}, recvTime)
	if !res.Explicit {
		t.Fatalf("显式集合接收应标记选择方式: %+v", res)
	}
	// 本次集合按原发运顺序保存，与提交顺序无关。
	if !sameOrder(res.Parcels, []string{"P001", "P003"}) {
		t.Fatalf("本次集合应按发运保存顺序: %v", res.Parcels)
	}
	sh, _ := s.ShipmentQuery("S1")
	if sh.ReceivedBy != "" {
		t.Fatalf("部分接收不应标记全部接收: %+v", sh)
	}
	// 已收件改归目的站、恢复在站，追加接收轨迹。
	for _, id := range []string{"P001", "P003"} {
		p, _ := s.Query(id)
		if p.Status != statusInStation || p.Station != "站点B" {
			t.Fatalf("已收件 %q 应归目的站在站: status=%q station=%q", id, p.Status, p.Station)
		}
		last := p.Trail[len(p.Trail)-1]
		if last.Op != "接收" || last.Request != "RS1" || last.Shipment != "S1" ||
			last.From != "站点A" || last.To != "站点B" || !last.Time.Equal(recvTime) {
			t.Fatalf("已收件 %q 缺少接收轨迹: %+v", id, last)
		}
		if s.ActiveShipment(id) != nil {
			t.Fatalf("已收件 %q 不应再归属原单在途", id)
		}
	}
	// 未选余件保持源站在途及作业限制。
	p2, _ := s.Query("P002")
	if p2.Status != statusInTransit || p2.Station != "站点A" || len(p2.Trail) != 2 {
		t.Fatalf("未选余件应保持源站在途: %+v", p2)
	}
	if sh := s.ActiveShipment("P002"); sh == nil || sh.Shipment != "S1" {
		t.Fatalf("未选余件应仍归 S1 在途")
	}
	if _, _, err := s.Handoff("R1", "站点A", "站点C", []string{"P002"}, tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatalf("在途余件应不能交接")
	}
	if _, _, err := s.Dispatch("B1", "站点A", "张三", []string{"P002"}, tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatalf("在途余件应不能配送出站")
	}
	if _, _, err := s.Freeze("E1", "P002", "站点A", "异常", tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatalf("在途余件应不能冻结")
	}

	// 已收件的后续交接、冻结、再次发运不妨碍余件接收。
	mustHandoff(t, s, "R2", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 13, 0))
	mustFreeze(t, s, "E2", "P003", "站点B", "外包装破损", tClock(2026, 10, 5, 13, 0))
	mustReceiveParcels(t, s, "RS2", "S1", "站点B", []string{"P002"}, tClock(2026, 10, 5, 14, 0))
	sh, _ = s.ShipmentQuery("S1")
	if sh.ReceivedBy != "RS2" {
		t.Fatalf("全部到齐后运输单应标记完成接收的请求号: %+v", sh)
	}
	p2, _ = s.Query("P002")
	if p2.Status != statusInStation || p2.Station != "站点B" {
		t.Fatalf("余件补齐后应归目的站在站: %+v", p2)
	}
	// 全部接收后新请求拒绝（两种方式都拒绝），运输单号不释放。
	if _, _, err := s.Receive("RS9", "S1", "站点B", nil, tClock(2026, 10, 5, 15, 0)); err == nil {
		t.Fatalf("全部接收后不选成员的新请求应拒绝")
	}
	if _, _, err := s.Receive("RS10", "S1", "站点B", []string{"P002"}, tClock(2026, 10, 5, 15, 0)); err == nil {
		t.Fatalf("全部接收后显式集合的新请求应拒绝")
	}
	if _, replayed, err := s.Ship("S1", "站点A", "站点B", []string{"P003", "P002", "P001"}, tClock(2026, 10, 5, 16, 0)); err != nil || !replayed {
		t.Fatalf("全部接收后运输单号不释放，同内容发运应重放: %v replayed=%v", err, replayed)
	}
}

// 不选成员方式：首次受理时取当时余件；先显式接收一批后，不选成员接收剩余全部。
func TestReceiveRemainingWithoutSelection(t *testing.T) {
	s, _ := openTempStore(t)
	setupPartialShip(t, s)

	mustReceiveParcels(t, s, "RS1", "S1", "站点B", []string{"P002"}, tClock(2026, 10, 5, 12, 0))
	recvTime := tClock(2026, 10, 5, 13, 0)
	res := mustReceive(t, s, "RS2", "S1", "站点B", recvTime)
	if res.Explicit {
		t.Fatalf("不选成员接收不应标记显式方式: %+v", res)
	}
	// 本次集合为当时的余件（P001、P003），按发运保存顺序。
	if !sameOrder(res.Parcels, []string{"P001", "P003"}) {
		t.Fatalf("不选成员应取当时余件: %v", res.Parcels)
	}
	sh, _ := s.ShipmentQuery("S1")
	if sh.ReceivedBy != "RS2" {
		t.Fatalf("全部到齐后应标记完成接收: %+v", sh)
	}
	for _, id := range []string{"P001", "P003"} {
		p, _ := s.Query(id)
		if p.Status != statusInStation || p.Station != "站点B" {
			t.Fatalf("余件 %q 应归目的站在站: %+v", id, p)
		}
	}
}

// 重放：两种方式共用请求号范围；同号同方式同内容返回首次集合与时间，
// 不检查现状、不重算余件、不追加轨迹或改写文件；换集合或切换方式冲突。
func TestReceivePartialReplayAndConflict(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialShip(t, s)
	recvTime := tClock(2026, 10, 5, 12, 0)
	mustReceiveParcels(t, s, "RS1", "S1", "站点B", []string{"P001", "P002"}, recvTime)
	mustReceive(t, s, "RS2", "S1", "站点B", tClock(2026, 10, 5, 13, 0)) // 不选成员，收下 P003，全部到齐
	// 全部接收及后续流转之后重放仍成立。
	mustHandoff(t, s, "R9", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 14, 0))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 显式集合换序无关：返回首次集合与时间。
	res, replayed, err := s.Receive("RS1", "S1", "站点B", []string{"P002", "P001"}, tClock(2026, 10, 5, 15, 0))
	if err != nil || !replayed || !sameOrder(res.Parcels, []string{"P001", "P002"}) || !res.Time.Equal(recvTime) {
		t.Fatalf("显式集合换序重放应返回首次集合与时间: %+v replayed=%v err=%v", res, replayed, err)
	}
	// 不选成员重放：返回首次集合（当时的余件），不检查现状、不重算余件。
	res2, replayed, err := s.Receive("RS2", "S1", "站点B", nil, tClock(2026, 10, 5, 15, 0))
	if err != nil || !replayed || !sameOrder(res2.Parcels, []string{"P003"}) {
		t.Fatalf("不选成员重放应返回首次集合: %+v replayed=%v err=%v", res2, replayed, err)
	}
	// 重放不追加轨迹、不改写文件。
	p1, _ := s.Query("P001")
	if len(p1.Trail) != 4 { // 收件、发运、接收、交接
		t.Fatalf("重放不应追加轨迹，得到 %d 条", len(p1.Trail))
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("重放不应改写数据文件")
	}

	// 同号改变显式集合：冲突。
	if _, _, err := s.Receive("RS1", "S1", "站点B", []string{"P001"}, tClock(2026, 10, 5, 16, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同号改变集合应报冲突: %v", err)
	}
	// 同号切换选择方式：冲突（显式 -> 不选、不选 -> 显式）。
	if _, _, err := s.Receive("RS1", "S1", "站点B", nil, tClock(2026, 10, 5, 16, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同号切换为不选成员应报冲突: %v", err)
	}
	if _, _, err := s.Receive("RS2", "S1", "站点B", []string{"P003"}, tClock(2026, 10, 5, 16, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同号切换为显式集合应报冲突: %v", err)
	}
	// 同号换运输单或接收站：冲突。
	mustShip(t, s, "S2", "站点B", "站点C", []string{"P002"}, tClock(2026, 10, 5, 16, 30))
	if _, _, err := s.Receive("RS1", "S2", "站点C", []string{"P002"}, tClock(2026, 10, 5, 17, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同号换运输单应报冲突: %v", err)
	}
	if _, _, err := s.Receive("RS1", "S1", "站点C", []string{"P001", "P002"}, tClock(2026, 10, 5, 17, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同号换接收站应报冲突: %v", err)
	}
}

// 首次受理失败：非原单成员、已收件、空显式集合、接收站不符、运输单不存在，
// 整次拒绝且不占用请求号、不改变任何包裹。
func TestReceivePartialFailuresAtomic(t *testing.T) {
	s, _ := openTempStore(t)
	setupPartialShip(t, s)
	mustReceiveParcels(t, s, "RS1", "S1", "站点B", []string{"P001"}, tClock(2026, 10, 5, 12, 0))
	mustRegister(t, s, "P404", "站点A", tClock(2026, 10, 5, 9, 0))

	cases := []struct {
		name    string
		req     string
		ship    string
		station string
		parcels []string
	}{
		{"选中件不属于原单", "RSX", "S1", "站点B", []string{"P002", "P404"}},
		{"选中件已接收", "RSX", "S1", "站点B", []string{"P001", "P002"}},
		{"空显式集合", "RSX", "S1", "站点B", []string{}},
		{"接收站不是目的站", "RSX", "S1", "站点C", []string{"P002"}},
		{"运输单不存在", "RSX", "S404", "站点B", []string{"P002"}},
	}
	for _, tc := range cases {
		parcels := tc.parcels
		if tc.name == "空显式集合" {
			parcels = []string{} // 非 nil 空集合
		}
		if _, _, err := s.Receive(tc.req, tc.ship, tc.station, parcels, tClock(2026, 10, 5, 13, 0)); err == nil {
			t.Fatalf("%s：应整次拒绝", tc.name)
		}
	}
	// 整次拒绝：请求号不占用，包裹状态与轨迹不变。
	if s.ReceiveOf("RSX") != nil {
		t.Fatalf("失败的首次接收不应占用请求号")
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusInTransit || p2.Station != "站点A" || len(p2.Trail) != 2 {
		t.Fatalf("失败的接收不应改变包裹: %+v", p2)
	}
	// 纠正后可重试成功。
	mustReceiveParcels(t, s, "RSX", "S1", "站点B", []string{"P002"}, tClock(2026, 10, 5, 14, 0))
}

// 持久化：分批接收后重载，状态、进度、去重与重放保持；旧格式台账
// （接收记录无选择方式字段、整单一次接收）无需转换，重放保留原结果。
func TestReceivePartialPersistenceAndLegacy(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialShip(t, s)
	recvTime := tClock(2026, 10, 5, 12, 0)
	mustReceiveParcels(t, s, "RS1", "S1", "站点B", []string{"P001"}, recvTime)

	// 重载后：已收件在站、余件在途，进度与去重保持。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开台账失败: %v", err)
	}
	p1, _ := s2.Query("P001")
	if p1.Status != statusInStation || p1.Station != "站点B" {
		t.Fatalf("重开后 P001 应在站点B 在站: %+v", p1)
	}
	p2, _ := s2.Query("P002")
	if p2.Status != statusInTransit || p2.Station != "站点A" {
		t.Fatalf("重开后 P002 应保持源站在途: %+v", p2)
	}
	sh, _ := s2.ShipmentQuery("S1")
	if sh.ReceivedBy != "" {
		t.Fatalf("重开后部分接收不应标记全部接收: %+v", sh)
	}
	if _, replayed, err := s2.Receive("RS1", "S1", "站点B", []string{"P001"}, tClock(2026, 10, 5, 13, 0)); err != nil || !replayed {
		t.Fatalf("重开后显式接收重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	// 重载后余件可补齐。
	mustReceive(t, s2, "RS2", "S1", "站点B", tClock(2026, 10, 5, 14, 0))
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("再次重开失败: %v", err)
	}
	sh, _ = s3.ShipmentQuery("S1")
	if sh.ReceivedBy != "RS2" {
		t.Fatalf("补齐并重载后应标记全部接收: %+v", sh)
	}

	// 旧格式台账：不选成员整单接收的记录没有选择方式字段，无需转换直接使用。
	s4, path4 := openTempStore(t)
	setupShipBase(t, s4)
	shipTime := tClock(2026, 10, 5, 10, 0)
	mustShip(t, s4, "S1", "站点A", "站点B", []string{"P001", "P002"}, shipTime)
	oldRecvTime := tClock(2026, 10, 5, 12, 0)
	mustReceive(t, s4, "RS1", "S1", "站点B", oldRecvTime)
	raw, err := os.ReadFile(path4)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	rv := m["receives"].(map[string]any)["RS1"].(map[string]any)
	if _, has := rv["explicit"]; has {
		t.Fatalf("不选成员方式的接收记录不应写入选择方式字段: %v", rv)
	}
	s5, err := Open(path4)
	if err != nil {
		t.Fatalf("旧格式台账应直接打开: %v", err)
	}
	// 旧整单接收请求视为不选成员方式：重放保留原结果，不检查现状。
	res, replayed, err := s5.Receive("RS1", "S1", "站点B", nil, tClock(2026, 10, 6, 9, 0))
	if err != nil || !replayed || !sameOrder(res.Parcels, []string{"P001", "P002"}) || !res.Time.Equal(oldRecvTime) {
		t.Fatalf("旧整单接收重放应保留原结果: %+v replayed=%v err=%v", res, replayed, err)
	}
	// 旧记录与显式方式不同：同号显式提交报冲突。
	if _, _, err := s5.Receive("RS1", "S1", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 6, 9, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("旧不选成员记录与显式提交应报冲突: %v", err)
	}
	sh, _ = s5.ShipmentQuery("S1")
	if sh.ReceivedBy != "RS1" {
		t.Fatalf("旧台账运输单应保持已接收: %+v", sh)
	}
}

// 分批接收的新增关联为空、缺失、同单同件重复接收或与轨迹矛盾时明确拒绝载入。
func TestReceivePartialCorrupt(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialShip(t, s)
	mustReceiveParcels(t, s, "RS1", "S1", "站点B", []string{"P001"}, tClock(2026, 10, 5, 12, 0))
	mustReceive(t, s, "RS2", "S1", "站点B", tClock(2026, 10, 5, 13, 0))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	receive := func(m map[string]any, req string) map[string]any {
		return m["receives"].(map[string]any)[req].(map[string]any)
	}
	shipment := func(m map[string]any) map[string]any {
		return m["shipments"].(map[string]any)["S1"].(map[string]any)
	}

	corruptLedger(t, path, raw, "同单同件重复接收", func(m map[string]any) {
		receive(m, "RS2")["parcels"] = []any{"P001", "P002", "P003"}
	})
	corruptLedger(t, path, raw, "接收成员为空", func(m map[string]any) {
		receive(m, "RS1")["parcels"] = []any{}
	})
	corruptLedger(t, path, raw, "接收成员缺失", func(m map[string]any) {
		delete(receive(m, "RS1"), "parcels")
	})
	corruptLedger(t, path, raw, "接收成员不属原单", func(m map[string]any) {
		receive(m, "RS1")["parcels"] = []any{"P404"}
	})
	corruptLedger(t, path, raw, "接收成员顺序与发运矛盾", func(m map[string]any) {
		receive(m, "RS2")["parcels"] = []any{"P003", "P002"}
	})
	corruptLedger(t, path, raw, "全部接收标记缺失", func(m map[string]any) {
		delete(shipment(m), "receivedBy")
	})
	corruptLedger(t, path, raw, "部分接收却标记全部接收", func(m map[string]any) {
		// 只保留 RS1（部分接收），但运输单仍标记全部接收。
		delete(m["receives"].(map[string]any), "RS2")
		shipment(m)["receivedBy"] = "RS1"
		p := m["parcels"].(map[string]any)
		for _, id := range []string{"P002", "P003"} {
			pm := p[id].(map[string]any)
			trail := pm["trail"].([]any)
			pm["trail"] = trail[:len(trail)-1] // 去掉接收轨迹
			pm["station"] = "站点A"
			pm["status"] = "站间在途"
		}
	})
	corruptLedger(t, path, raw, "接收请求引用不存在的运输单", func(m map[string]any) {
		receive(m, "RS1")["shipment"] = "S404"
	})
	corruptLedger(t, path, raw, "接收结果为 null", func(m map[string]any) {
		m["receives"].(map[string]any)["RS1"] = nil
	})
	restoreLedger(t, path, raw)
}

// 并发接收：重叠成员至多一次生效，不重叠的合法接收全部保留。
func TestConcurrentReceiveOverlapping(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	for _, id := range []string{"P001", "P002", "P003"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败", id)
		}
	}
	if _, _, code := runCLI(t, dbPath, "ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002", "--parcel", "P003"); code != 0 {
		t.Fatalf("发运失败")
	}

	// 两个请求都包含 P002：至多一个生效。
	_, _, codes := runParallel(t, 2, func(i int) []string {
		if i == 0 {
			return []string{"--data", dbPath, "receive", "--request", "RS1", "--shipment", "S1",
				"--station", "站点B", "--parcel", "P001", "--parcel", "P002"}
		}
		return []string{"--data", dbPath, "receive", "--request", "RS2", "--shipment", "S1",
			"--station", "站点B", "--parcel", "P002", "--parcel", "P003"}
	})
	if got := countCodes(codes, exitOK); got != 1 {
		t.Fatalf("重叠成员的并发接收应至多一次生效，成功 %d 次（codes=%v）", got, codes)
	}
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	gotP002 := s.ShipmentReceiveOf("S1", "P002")
	if gotP002 == nil {
		t.Fatalf("P002 应被恰好一个请求接收")
	}
	other := "RS2"
	if gotP002.Request == "RS2" {
		other = "RS1"
	}
	if s.ReceiveOf(other) != nil {
		t.Fatalf("失败的重叠接收不应占用请求号 %q", other)
	}

	// 不重叠的合法并发接收全部保留。
	_, _, codes = runParallel(t, 2, func(i int) []string {
		if i == 0 {
			return []string{"--data", dbPath, "receive", "--request", "RS3", "--shipment", "S1",
				"--station", "站点B", "--parcel", "P001"}
		}
		return []string{"--data", dbPath, "receive", "--request", "RS4", "--shipment", "S1",
			"--station", "站点B", "--parcel", "P003"}
	})
	// P001 或 P003 可能已在上一步被接收：只统计仍合法的请求。
	s, err = Open(dbPath)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	want := 0
	if s.ShipmentReceiveOf("S1", "P001") == nil || s.ShipmentReceiveOf("S1", "P001").Request == "RS3" {
		want++
	}
	if s.ShipmentReceiveOf("S1", "P003") == nil || s.ShipmentReceiveOf("S1", "P003").Request == "RS4" {
		want++
	}
	if got := countCodes(codes, exitOK); got != want {
		t.Fatalf("不重叠的合法接收应全部保留，应成功 %d 次，实际 %d 次（codes=%v）", want, got, codes)
	}
	s, err = Open(dbPath)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	// 全部成员最终都被接收且运输单标记完成。
	sh, _ := s.ShipmentQuery("S1")
	if sh.ReceivedBy == "" {
		t.Fatalf("全部接收后运输单应标记完成: %+v", sh)
	}
	for _, pid := range []string{"P001", "P002", "P003"} {
		if s.ShipmentReceiveOf("S1", pid) == nil {
			t.Fatalf("包裹 %q 应已被接收", pid)
		}
		p, _ := s.Query(pid)
		if p.Station != "站点B" || p.Status != statusInStation {
			t.Fatalf("包裹 %q 应归目的站在站: %+v", pid, p)
		}
	}
}

// CLI 端到端：分批接收、进度展示、余件与已收件的不同查询表现。
func TestCLIPartialReceive(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	for _, id := range []string{"P001", "P002", "P003"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败", id)
		}
	}
	if _, _, code := runCLI(t, dbPath, "ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002", "--parcel", "P003"); code != 0 {
		t.Fatalf("发运失败")
	}

	// 显式集合接收 P001、P003。
	out, _, code := runCLI(t, dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B",
		"--parcel", "P003", "--parcel", "P001")
	if code != 0 || !strings.Contains(out, "接收成功") ||
		!strings.Contains(out, "P001") || !strings.Contains(out, "P003") || strings.Contains(out, "P002") {
		t.Fatalf("显式集合接收输出不符: code=%d out=%s", code, out)
	}

	// 运输单查询：部分接收、进度、逐件接收信息。
	out, _, code = runCLI(t, dbPath, "shipment", "--id", "S1")
	if code != 0 || !strings.Contains(out, "运输单状态: 部分接收") ||
		!strings.Contains(out, "接收进度: 2/3") ||
		!strings.Contains(out, "P001    已接收    接收请求号: RS1") ||
		!strings.Contains(out, "P002    待接收") {
		t.Fatalf("运输单查询应展示部分接收进度: code=%d out=%s", code, out)
	}

	// query：余件仍显示运输单与目的站；已收件不误认仍归原单在途。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P002")
	if code != 0 || !strings.Contains(out, "当前状态: 站间在途") ||
		!strings.Contains(out, "当前运输单: S1") || !strings.Contains(out, "目的站: 站点B") {
		t.Fatalf("余件 query 应显示运输单与目的站: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前状态: 在站") || !strings.Contains(out, "当前站点: 站点B") ||
		strings.Contains(out, "当前运输单") || !strings.Contains(out, "操作: 接收") {
		t.Fatalf("已收件 query 不应误认仍归原单在途: code=%d out=%s", code, out)
	}

	// 同号同内容（换序）重放；不选成员接收全部余件。
	out, _, code = runCLI(t, dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B",
		"--parcel", "P001", "--parcel", "P003")
	if code != 0 || !strings.Contains(out, "重复提交") {
		t.Fatalf("显式集合换序重放应返回首次结果: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, dbPath, "receive", "--request", "RS2", "--shipment", "S1", "--station", "站点B")
	if code != 0 || !strings.Contains(out, "接收成功") || !strings.Contains(out, "P002") ||
		strings.Contains(out, "P001") {
		t.Fatalf("不选成员应接收全部余件: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, dbPath, "shipment", "--id", "S1")
	if code != 0 || !strings.Contains(out, "运输单状态: 已接收") ||
		!strings.Contains(out, "接收进度: 3/3") || !strings.Contains(out, "接收请求号: RS2") {
		t.Fatalf("全部接收后运输单查询不符: code=%d out=%s", code, out)
	}
	// 全部接收后新请求拒绝，退出码 1。
	if _, _, code := runCLI(t, dbPath, "receive", "--request", "RS9", "--shipment", "S1", "--station", "站点B"); code != exitBusiness {
		t.Fatalf("全部接收后新请求应拒绝(1): code=%d", code)
	}
}

// 确保 ErrCorrupt 在分批接收篡改场景下经 errors.Is 识别（防止包装丢失）。
func TestReceivePartialCorruptIsErrCorrupt(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialShip(t, s)
	mustReceiveParcels(t, s, "RS1", "S1", "站点B", []string{"P001"}, tClock(2026, 10, 5, 12, 0))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["receives"].(map[string]any)["RS1"].(map[string]any)["parcels"] = []any{"P002"}
	buf, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("接收成员与轨迹矛盾应识别为数据损坏: %v", err)
	}
}
