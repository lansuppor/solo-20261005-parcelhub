package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func mustShip(t *testing.T, s *Store, shipment, from, to string, parcels []string, now time.Time) *ShipmentResult {
	t.Helper()
	res, replayed, err := s.Ship(shipment, from, to, parcels, now)
	if err != nil || replayed {
		t.Fatalf("Ship(%q) 意外失败: %v replayed=%v", shipment, err, replayed)
	}
	return res
}

func mustReceive(t *testing.T, s *Store, request, shipment, station string, now time.Time) *ReceiveResult {
	t.Helper()
	res, replayed, err := s.Receive(request, shipment, station, nil, now)
	if err != nil || replayed {
		t.Fatalf("Receive(%q) 意外失败: %v replayed=%v", request, err, replayed)
	}
	return res
}

// 准备两件在站点A 在站的包裹 P001、P002。
func setupShipBase(t *testing.T, s *Store) {
	t.Helper()
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
}

func TestShipSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	setupShipBase(t, s)

	shipTime := tClock(2026, 10, 5, 10, 0)
	res := mustShip(t, s, "S1", "站点A", "站点B", []string{"P002", "P001"}, shipTime)
	if res.Shipment != "S1" || res.From != "站点A" || res.To != "站点B" || !res.Time.Equal(shipTime) {
		t.Fatalf("发运结果不符: %+v", res)
	}
	// 成员按首次提交顺序永久保存。
	if !sameOrder(res.Parcels, []string{"P002", "P001"}) {
		t.Fatalf("成员顺序应为首次提交顺序: %v", res.Parcels)
	}
	if res.ReceivedBy != "" {
		t.Fatalf("新运输单不应标记已接收: %+v", res)
	}
	for _, id := range []string{"P001", "P002"} {
		p, err := s.Query(id)
		if err != nil {
			t.Fatalf("Query(%q) 失败: %v", id, err)
		}
		if p.Status != statusInTransit {
			t.Fatalf("包裹 %q 状态应为站间在途，得到 %q", id, p.Status)
		}
		if p.Station != "站点A" {
			t.Fatalf("在途包裹 %q 归属站点应暂记源站，得到 %q", id, p.Station)
		}
		last := p.Trail[len(p.Trail)-1]
		if last.Op != "发运" || last.Shipment != "S1" || last.From != "站点A" || last.To != "站点B" || !last.Time.Equal(shipTime) {
			t.Fatalf("包裹 %q 缺少含运输单、两站和时间的发运轨迹: %+v", id, last)
		}
		// 在途包裹应能查到当前运输单。
		if sh := s.ActiveShipment(id); sh == nil || sh.Shipment != "S1" {
			t.Fatalf("包裹 %q 应属于未接收运输单 S1", id)
		}
	}
}

func TestShipReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	setupShipBase(t, s)
	shipTime := tClock(2026, 10, 5, 10, 0)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002"}, shipTime)

	// 同号、同两站、同集合（换序）重放：返回首次成员顺序与发运时间，不追加轨迹。
	res, replayed, err := s.Ship("S1", "站点A", "站点B", []string{"P002", "P001"}, tClock(2026, 10, 5, 11, 0))
	if err != nil || !replayed {
		t.Fatalf("同内容重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	if !sameOrder(res.Parcels, []string{"P001", "P002"}) || !res.Time.Equal(shipTime) {
		t.Fatalf("重放应返回首次成员顺序与发运时间: %+v", res)
	}
	p, _ := s.Query("P001")
	if len(p.Trail) != 2 {
		t.Fatalf("重放不应追加轨迹，得到 %d 条", len(p.Trail))
	}

	// 换内容冲突：换目的站、换源站、换集合均拒绝。
	for _, tc := range []struct {
		from, to string
		parcels  []string
	}{
		{"站点A", "站点C", []string{"P001", "P002"}},
		{"站点C", "站点B", []string{"P001", "P002"}},
		{"站点A", "站点B", []string{"P001"}},
	} {
		if _, _, err := s.Ship("S1", tc.from, tc.to, tc.parcels, tClock(2026, 10, 5, 11, 0)); err == nil ||
			!strings.Contains(err.Error(), "冲突") {
			t.Fatalf("换内容应报冲突: from=%q to=%q parcels=%v err=%v", tc.from, tc.to, tc.parcels, err)
		}
	}

	// 接收后运输单号也不释放：同号重放仍返回首次结果，换内容仍冲突。
	mustReceive(t, s, "RS1", "S1", "站点B", tClock(2026, 10, 5, 12, 0))
	if _, replayed, err := s.Ship("S1", "站点A", "站点B", []string{"P002", "P001"}, tClock(2026, 10, 5, 13, 0)); err != nil || !replayed {
		t.Fatalf("接收后同内容重放仍应返回首次结果: %v replayed=%v", err, replayed)
	}
	if _, _, err := s.Ship("S1", "站点A", "站点C", []string{"P001", "P002"}, tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatalf("接收后换内容仍应报冲突")
	}
}

func TestShipFailuresAtomic(t *testing.T) {
	s, _ := openTempStore(t)
	setupShipBase(t, s)
	mustRegister(t, s, "P003", "站点C", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s, "P004", "站点A", tClock(2026, 10, 5, 9, 0))
	mustFreeze(t, s, "E1", "P004", "站点A", "外包装破损", tClock(2026, 10, 5, 9, 30))
	mustRegister(t, s, "P005", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P005"}, tClock(2026, 10, 5, 9, 40))

	// 未登记、不在源站、已冻结、配送中：任一不符整单拒绝。
	for _, parcels := range [][]string{
		{"P001", "P404"},
		{"P001", "P003"},
		{"P001", "P004"},
		{"P001", "P005"},
	} {
		if _, _, err := s.Ship("SX", "站点A", "站点B", parcels, tClock(2026, 10, 5, 10, 0)); err == nil {
			t.Fatalf("含不符成员 %v 的发运应整单拒绝", parcels)
		}
	}
	// 整单拒绝：其余包裹状态与轨迹不变，运输单号不占用。
	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		if p.Status != statusInStation || p.Station != "站点A" || len(p.Trail) != 1 {
			t.Fatalf("失败的首次发运不应改变包裹 %q: status=%q station=%q trail=%d", id, p.Status, p.Station, len(p.Trail))
		}
	}
	if _, err := s.ShipmentQuery("SX"); err == nil {
		t.Fatalf("失败的首次发运不应占用运输单号")
	}

	// 在途件不能再次发运（每件同时最多属于一张未接收运输单）。
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	if _, _, err := s.Ship("S2", "站点A", "站点C", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 30)); err == nil {
		t.Fatalf("在途件应不能再次发运")
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusInStation || len(p2.Trail) != 1 {
		t.Fatalf("整单拒绝不应改变其他包裹: %+v", p2)
	}
	if _, err := s.ShipmentQuery("S2"); err == nil {
		t.Fatalf("失败的首次发运不应占用运输单号 S2")
	}
}

func TestShipBlocksOtherOperations(t *testing.T) {
	s, _ := openTempStore(t)
	setupShipBase(t, s)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))

	// 在途件不能首次交接、配送出站或冻结，涉及它的批量操作整批不变。
	if _, _, err := s.Handoff("R1", "站点A", "站点C", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 30)); err == nil {
		t.Fatalf("在途件应不能首次交接")
	}
	if _, _, err := s.Dispatch("B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 30)); err == nil {
		t.Fatalf("在途件应不能首次配送出站")
	}
	if _, _, err := s.Freeze("E1", "P001", "站点A", "异常", tClock(2026, 10, 5, 10, 30)); err == nil {
		t.Fatalf("在途件应不能首次冻结")
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusInStation || p2.Station != "站点A" || len(p2.Trail) != 1 {
		t.Fatalf("批量操作整批不变：P002 不应受影响: %+v", p2)
	}

	// 在途件也不能退回旧交接：先交接再发运，发运后旧交接不能退回。
	mustHandoff(t, s, "R2", "站点A", "站点C", []string{"P002"}, tClock(2026, 10, 5, 11, 0))
	mustShip(t, s, "S2", "站点C", "站点D", []string{"P002"}, tClock(2026, 10, 5, 11, 30))
	if _, _, err := s.Return("RT1", "R2", "错发站点", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatalf("在途件应不能退回旧交接")
	}
}

func TestReceiveSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	setupShipBase(t, s)
	shipTime := tClock(2026, 10, 5, 10, 0)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P002", "P001"}, shipTime)

	recvTime := tClock(2026, 10, 5, 12, 0)
	res := mustReceive(t, s, "RS1", "S1", "站点B", recvTime)
	if res.Request != "RS1" || res.Shipment != "S1" || res.Station != "站点B" || !res.Time.Equal(recvTime) {
		t.Fatalf("接收结果不符: %+v", res)
	}
	// 成员按发运保存顺序。
	if !sameOrder(res.Parcels, []string{"P002", "P001"}) {
		t.Fatalf("接收成员应按发运保存顺序: %v", res.Parcels)
	}
	sh, err := s.ShipmentQuery("S1")
	if err != nil || sh.ReceivedBy != "RS1" {
		t.Fatalf("运输单应永久标记已接收: %+v err=%v", sh, err)
	}
	// 按原顺序全部改归目的站、恢复在站，各追加接收轨迹。
	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		if p.Status != statusInStation || p.Station != "站点B" {
			t.Fatalf("接收后包裹 %q 应归目的站在站: status=%q station=%q", id, p.Status, p.Station)
		}
		last := p.Trail[len(p.Trail)-1]
		if last.Op != "接收" || last.Shipment != "S1" || last.Request != "RS1" ||
			last.From != "站点A" || last.To != "站点B" || !last.Time.Equal(recvTime) {
			t.Fatalf("包裹 %q 缺少接收轨迹: %+v", id, last)
		}
		if sh := s.ActiveShipment(id); sh != nil {
			t.Fatalf("接收后包裹 %q 不应再属于未接收运输单", id)
		}
	}
	// 接收后可继续在站作业。
	mustHandoff(t, s, "R9", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 13, 0))
}

func TestReceiveReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	setupShipBase(t, s)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	recvTime := tClock(2026, 10, 5, 12, 0)
	mustReceive(t, s, "RS1", "S1", "站点B", recvTime)

	// 接收后包裹再流转，同号同内容重放仍返回首次接收结果与时间。
	mustHandoff(t, s, "R9", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 13, 0))
	res, replayed, err := s.Receive("RS1", "S1", "站点B", nil, tClock(2026, 10, 5, 14, 0))
	if err != nil || !replayed {
		t.Fatalf("同内容重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	if !sameOrder(res.Parcels, []string{"P001", "P002"}) || !res.Time.Equal(recvTime) {
		t.Fatalf("重放应返回首次成员与接收时间: %+v", res)
	}
	p1, _ := s.Query("P001")
	if p1.Station != "站点C" {
		t.Fatalf("重放不应改变后续流转结果: %+v", p1)
	}
	p2, _ := s.Query("P002")
	if len(p2.Trail) != 3 {
		t.Fatalf("重放不应追加轨迹，得到 %d 条", len(p2.Trail))
	}

	// 换内容冲突：换运输单或换接收站点均拒绝。
	mustShip(t, s, "S2", "站点B", "站点C", []string{"P002"}, tClock(2026, 10, 5, 14, 30))
	if _, _, err := s.Receive("RS1", "S2", "站点C", nil, tClock(2026, 10, 5, 15, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("换运输单应报冲突: %v", err)
	}
	if _, _, err := s.Receive("RS1", "S1", "站点C", nil, tClock(2026, 10, 5, 15, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("换接收站点应报冲突: %v", err)
	}

	// 换请求号再次接收同一运输单拒绝。
	if _, _, err := s.Receive("RS2", "S1", "站点B", nil, tClock(2026, 10, 5, 15, 0)); err == nil {
		t.Fatalf("换请求号再次接收同一运输单应拒绝")
	}

	// 接收请求号与运输单号及其他编号独立，可同名。
	mustReceive(t, s, "S2", "S2", "站点C", tClock(2026, 10, 5, 16, 0))
}

func TestReceiveFailuresAtomic(t *testing.T) {
	s, _ := openTempStore(t)
	setupShipBase(t, s)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))

	// 运输单不存在。
	if _, _, err := s.Receive("RS1", "S404", "站点B", nil, tClock(2026, 10, 5, 11, 0)); err == nil {
		t.Fatalf("接收不存在的运输单应拒绝")
	}
	// 接收站点不是目的站。
	if _, _, err := s.Receive("RS1", "S1", "站点C", nil, tClock(2026, 10, 5, 11, 0)); err == nil {
		t.Fatalf("接收站点不符应整单拒绝")
	}
	// 失败的首次接收不占用请求号，运输单仍未接收，包裹仍在途。
	if _, err := s.ShipmentQuery("S1"); err != nil {
		t.Fatalf("运输单应仍存在: %v", err)
	}
	if s.ReceiveOf("RS1") != nil {
		t.Fatalf("失败的首次接收不应占用请求号")
	}
	p, _ := s.Query("P001")
	if p.Status != statusInTransit || p.Station != "站点A" || len(p.Trail) != 2 {
		t.Fatalf("失败的接收不应改变包裹: %+v", p)
	}
	// 纠正后可重试成功。
	mustReceive(t, s, "RS1", "S1", "站点B", tClock(2026, 10, 5, 12, 0))
}

func TestShipReceiveAreNewFlow(t *testing.T) {
	s, _ := openTempStore(t)
	setupShipBase(t, s)
	// 交接 A->B 后发运 B->C 并接收：发运与接收都算新流转，旧交接不能退回。
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	mustShip(t, s, "S1", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 11, 0))
	mustReceive(t, s, "RS1", "S1", "站点C", tClock(2026, 10, 5, 12, 0))
	if _, _, err := s.Return("RT1", "R1", "错发站点", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatalf("发运接收后不应能退回此前旧交接")
	}
	// 新运输单不能作为退回的原交接。
	if _, _, err := s.Return("RT2", "S1", "错发站点", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatalf("运输单不应能作为退回的原交接")
	}
}

func TestShipReceivePersistence(t *testing.T) {
	s, path := openTempStore(t)
	setupShipBase(t, s)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	mustReceive(t, s, "RS1", "S1", "站点B", tClock(2026, 10, 5, 12, 0))
	mustShip(t, s, "S2", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 13, 0))

	// 重启后规则不变：状态、成员顺序、接收标记与去重结果保持。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开台账失败: %v", err)
	}
	p, _ := s2.Query("P001")
	if p.Status != statusInTransit || p.Station != "站点B" {
		t.Fatalf("重开后 P001 应在 S2 在途: %+v", p)
	}
	p2, _ := s2.Query("P002")
	if p2.Status != statusInStation || p2.Station != "站点B" {
		t.Fatalf("重开后 P002 应在站点B 在站: %+v", p2)
	}
	sh, err := s2.ShipmentQuery("S1")
	if err != nil || sh.ReceivedBy != "RS1" || !sameOrder(sh.Parcels, []string{"P001", "P002"}) {
		t.Fatalf("重开后运输单不符: %+v err=%v", sh, err)
	}
	if _, replayed, err := s2.Ship("S1", "站点A", "站点B", []string{"P002", "P001"}, tClock(2026, 10, 5, 14, 0)); err != nil || !replayed {
		t.Fatalf("重开后发运重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	if _, replayed, err := s2.Receive("RS1", "S1", "站点B", nil, tClock(2026, 10, 5, 14, 0)); err != nil || !replayed {
		t.Fatalf("重开后接收重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	if _, _, err := s2.Receive("RS9", "S1", "站点B", nil, tClock(2026, 10, 5, 14, 0)); err == nil {
		t.Fatalf("重开后换请求号再次接收同单应拒绝")
	}
}

// 新增记录的关联为空、缺失或与包裹、轨迹矛盾时明确拒绝读写，不崩溃或覆盖。
func TestShipReceiveCorrupt(t *testing.T) {
	s, path := openTempStore(t)
	setupShipBase(t, s)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	mustReceive(t, s, "RS1", "S1", "站点B", tClock(2026, 10, 5, 12, 0))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取数据文件失败: %v", err)
	}
	mutate := func(name string, fn func(m map[string]any)) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("解析数据文件失败: %v", err)
		}
		fn(m)
		buf, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("编码失败: %v", err)
		}
		if err := os.WriteFile(path, buf, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s：应拒绝读写，得到 err=%v", name, err)
		}
	}
	shipment := func(m map[string]any) map[string]any { return m["shipments"].(map[string]any)["S1"].(map[string]any) }
	receive := func(m map[string]any) map[string]any { return m["receives"].(map[string]any)["RS1"].(map[string]any) }
	parcel := func(m map[string]any) map[string]any { return m["parcels"].(map[string]any)["P001"].(map[string]any) }

	mutate("运输单结果缺失", func(m map[string]any) { delete(m, "shipments") })
	mutate("接收结果缺失", func(m map[string]any) { delete(m, "receives") })
	mutate("运输单缺少目的站", func(m map[string]any) { shipment(m)["to"] = "" })
	mutate("运输单引用不存在的包裹", func(m map[string]any) { shipment(m)["parcels"] = []any{"P404"} })
	mutate("接收请求引用不存在的运输单", func(m map[string]any) { receive(m)["shipment"] = "S404" })
	mutate("接收站点与目的站矛盾", func(m map[string]any) { receive(m)["station"] = "站点C" })
	mutate("运输单接收标记缺失", func(m map[string]any) { delete(shipment(m), "receivedBy") })
	mutate("接收成员与运输单矛盾", func(m map[string]any) { receive(m)["parcels"] = []any{"P002"} })
	mutate("包裹缺少接收轨迹", func(m map[string]any) {
		trail := parcel(m)["trail"].([]any)
		parcel(m)["trail"] = trail[:len(trail)-1]
	})
	mutate("在途状态与运输单矛盾", func(m map[string]any) {
		// 去掉接收结果与接收轨迹，但包裹状态仍是在站而非站间在途。
		delete(m, "receives")
		delete(shipment(m), "receivedBy")
		trail := parcel(m)["trail"].([]any)
		parcel(m)["trail"] = trail[:len(trail)-1]
	})

	// 篡改不留下有效文件：还原后旧有效台账直接使用。
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("还原后应能正常打开: %v", err)
	}
}
