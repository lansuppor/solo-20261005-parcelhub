package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustReroute(t *testing.T, s *Store, request, shipment, expect, to, reason string, now time.Time) *RerouteResult {
	t.Helper()
	res, replayed, err := s.Reroute(request, shipment, expect, to, reason, now)
	if err != nil || replayed {
		t.Fatalf("Reroute(%q) 意外失败: %v replayed=%v", request, err, replayed)
	}
	return res
}

// 基本改址：全部未收件改送新目的站，成员仍归源站、保持站间在途，
// 逐件追加改址轨迹，原发运结果不变。
func TestRerouteSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	setupPartialShip(t, s) // S1: 站点A -> 站点B，成员 P001、P002、P003

	rt := tClock(2026, 10, 5, 11, 0)
	res := mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "站点B 暂停收货", rt)
	if res.From != "站点B" || res.To != "站点C" || res.Reason != "站点B 暂停收货" || !res.Time.Equal(rt) {
		t.Fatalf("改址结果不符: %+v", res)
	}
	if !sameOrder(res.Parcels, []string{"P001", "P002", "P003"}) {
		t.Fatalf("改址集合应为全部未收件、按发运顺序: %v", res.Parcels)
	}

	sh, err := s.ShipmentQuery("S1")
	if err != nil {
		t.Fatalf("运输单应存在: %v", err)
	}
	if sh.From != "站点A" || sh.To != "站点B" || sh.ReceivedBy != "" {
		t.Fatalf("原发运结果不应改变: %+v", sh)
	}
	if !sameOrder(sh.Reroutes, []string{"RR1"}) {
		t.Fatalf("运输单改址列表应为 [RR1]: %v", sh.Reroutes)
	}
	if eff := s.effectiveTo(sh); eff != "站点C" {
		t.Fatalf("当前有效目的站应为站点C: %q", eff)
	}

	for _, id := range []string{"P001", "P002", "P003"} {
		p, err := s.Query(id)
		if err != nil {
			t.Fatalf("包裹 %q 应存在: %v", id, err)
		}
		if p.Status != statusInTransit || p.Station != "站点A" {
			t.Fatalf("改址后包裹 %q 应仍归源站站间在途: status=%q station=%q", id, p.Status, p.Station)
		}
		last := p.Trail[len(p.Trail)-1]
		if last.Op != "改址" || last.Request != "RR1" || last.Shipment != "S1" ||
			last.From != "站点B" || last.To != "站点C" || last.Station != "站点A" ||
			last.Reason != "站点B 暂停收货" || !last.Time.Equal(rt) {
			t.Fatalf("包裹 %q 末条轨迹应为改址记录: %+v", id, last)
		}
	}
}

// 分批接收后改址：已收件不参与受理条件检查或变更，集合只含当时未收件；
// 改址后按新目的站接收，旧目的站整次拒绝；改址前的接收重放仍返回原站点。
func TestRerouteAfterPartialReceive(t *testing.T) {
	s, _ := openTempStore(t)
	setupPartialShip(t, s)

	// P001 先在原目的站 站点B 接收。
	mustReceiveParcels(t, s, "RS1", "S1", "站点B", []string{"P001"}, tClock(2026, 10, 5, 11, 0))

	// 已收件 P001 随后发生合法作业（交接离开站点B），不阻止改址。
	mustHandoff(t, s, "H1", "站点B", "站点D", []string{"P001"}, tClock(2026, 10, 5, 11, 30))

	res := mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "站点B 暂停收货", tClock(2026, 10, 5, 12, 0))
	if !sameOrder(res.Parcels, []string{"P002", "P003"}) {
		t.Fatalf("改址集合应只含未收件: %v", res.Parcels)
	}
	p1, _ := s.Query("P001")
	if p1.Station != "站点D" || p1.Status != statusInStation {
		t.Fatalf("已收件不应被改址变更: %+v", p1)
	}
	for _, e := range p1.Trail {
		if e.Op == "改址" {
			t.Fatalf("已收件不应追加改址轨迹: %+v", p1.Trail)
		}
	}

	// 改址前的接收重放仍返回原站点、集合和时间。
	rs1, replayed, err := s.Receive("RS1", "S1", "站点B", []string{"P001"}, time.Now())
	if err != nil || !replayed || rs1.Station != "站点B" || !sameOrder(rs1.Parcels, []string{"P001"}) {
		t.Fatalf("改址前接收重放应返回原结果: %+v replayed=%v err=%v", rs1, replayed, err)
	}

	// 余件在旧目的站接收：整次拒绝。
	if _, _, err := s.Receive("RS2", "S1", "站点B", nil, time.Now()); err == nil {
		t.Fatalf("改址后按旧目的站接收应整次拒绝")
	}
	// 在新目的站接收成功（不选成员方式）。
	rs3 := mustReceive(t, s, "RS3", "S1", "站点C", tClock(2026, 10, 5, 13, 0))
	if !sameOrder(rs3.Parcels, []string{"P002", "P003"}) || rs3.Station != "站点C" {
		t.Fatalf("应按新目的站接收余件: %+v", rs3)
	}
	for _, id := range []string{"P002", "P003"} {
		p, _ := s.Query(id)
		if p.Station != "站点C" || p.Status != statusInStation {
			t.Fatalf("包裹 %q 应归新目的站在站: %+v", id, p)
		}
	}
	sh, _ := s.ShipmentQuery("S1")
	if sh.ReceivedBy != "RS3" {
		t.Fatalf("全部接收后应标记接收请求号: %+v", sh)
	}
	// 全部接收后拒绝新改址。
	if _, _, err := s.Reroute("RR2", "S1", "站点C", "站点E", "再次改址", time.Now()); err == nil {
		t.Fatalf("全部接收后应拒绝新改址")
	}
}

// 连续改址：每次以当时余件为准，链式推进有效目的站。
func TestRerouteConsecutive(t *testing.T) {
	s, _ := openTempStore(t)
	setupPartialShip(t, s)

	mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "第一次", tClock(2026, 10, 5, 11, 0))
	// 第一次改址后，旧预期不再成立。
	if _, _, err := s.Reroute("RRX", "S1", "站点B", "站点D", "预期过期", time.Now()); err == nil {
		t.Fatalf("预期目的站与当前有效目的站不符应拒绝")
	}
	// 第一次改址后接收一件于 站点C。
	mustReceiveParcels(t, s, "RS1", "S1", "站点C", []string{"P001"}, tClock(2026, 10, 5, 11, 30))
	// 再次改址：集合只含当时余件。
	res := mustReroute(t, s, "RR2", "S1", "站点C", "站点D", "第二次", tClock(2026, 10, 5, 12, 0))
	if !sameOrder(res.Parcels, []string{"P002", "P003"}) || res.From != "站点C" || res.To != "站点D" {
		t.Fatalf("第二次改址应以当时余件为准: %+v", res)
	}
	sh, _ := s.ShipmentQuery("S1")
	if !sameOrder(sh.Reroutes, []string{"RR1", "RR2"}) {
		t.Fatalf("改址列表应按提交顺序: %v", sh.Reroutes)
	}
	if eff := s.effectiveTo(sh); eff != "站点D" {
		t.Fatalf("连续改址后有效目的站应为站点D: %q", eff)
	}
	// 按最终有效目的站接收余件。
	rs := mustReceive(t, s, "RS2", "S1", "站点D", tClock(2026, 10, 5, 13, 0))
	if !sameOrder(rs.Parcels, []string{"P002", "P003"}) {
		t.Fatalf("应按最终有效目的站接收余件: %+v", rs)
	}
	// P001 的轨迹：发运 -> 改址(RR1) -> 接收（站点C），不含 RR2。
	p1, _ := s.Query("P001")
	var ops []string
	for _, e := range p1.Trail {
		ops = append(ops, e.Op)
	}
	if !sameOrder(ops, []string{"收件", "发运", "改址", "接收"}) {
		t.Fatalf("P001 轨迹顺序不符: %v", ops)
	}
	// P002 的轨迹：发运 -> 改址(RR1) -> 改址(RR2) -> 接收（站点D）。
	p2, _ := s.Query("P002")
	ops = ops[:0]
	for _, e := range p2.Trail {
		ops = append(ops, e.Op)
	}
	if !sameOrder(ops, []string{"收件", "发运", "改址", "改址", "接收"}) {
		t.Fatalf("P002 轨迹顺序不符: %v", ops)
	}
}

// 改址重放与冲突：同号同内容返回首次信息、集合与时间，不检查现状、不重算余件、
// 不改写台账（再次改址、接收和后续流转后仍成立）；换内容冲突；失败不占号。
func TestRerouteReplayAndConflict(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialShip(t, s)
	first := mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "站点B 暂停收货", tClock(2026, 10, 5, 11, 0))

	// 后续流转：再次改址、接收一件。
	mustReroute(t, s, "RR2", "S1", "站点C", "站点D", "第二次", tClock(2026, 10, 5, 12, 0))
	mustReceiveParcels(t, s, "RS1", "S1", "站点D", []string{"P001"}, tClock(2026, 10, 5, 13, 0))

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取数据文件失败: %v", err)
	}
	// 同号同内容重放：返回首次改址信息、集合与时间，不改写台账。
	got, replayed, err := s.Reroute("RR1", "S1", "站点B", "站点C", "站点B 暂停收货", time.Now())
	if err != nil || !replayed {
		t.Fatalf("同内容重放应成功: replayed=%v err=%v", replayed, err)
	}
	if got.From != first.From || got.To != first.To || !got.Time.Equal(first.Time) ||
		!sameOrder(got.Parcels, first.Parcels) || !sameOrder(got.Parcels, []string{"P001", "P002", "P003"}) {
		t.Fatalf("重放应返回首次集合与时间: got=%+v first=%+v", got, first)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取数据文件失败: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("重放不应改写台账")
	}

	// 换内容冲突：换运输单、预期、新目的站或原因均拒绝。
	for _, tc := range [][4]string{
		{"S9", "站点B", "站点C", "站点B 暂停收货"},
		{"S1", "站点C", "站点C", "站点B 暂停收货"},
		{"S1", "站点B", "站点D", "站点B 暂停收货"},
		{"S1", "站点B", "站点C", "别的原因"},
	} {
		if _, _, err := s.Reroute("RR1", tc[0], tc[1], tc[2], tc[3], time.Now()); err == nil ||
			!strings.Contains(err.Error(), "冲突") {
			t.Fatalf("换内容 %v 应报冲突: %v", tc, err)
		}
	}

	// 失败的首次改址不占号：换请求号修正后可用同一请求号成功。
	if _, _, err := s.Reroute("RR9", "S1", "站点B", "站点E", "预期错误", time.Now()); err == nil {
		t.Fatalf("预期不符应失败")
	}
	if _, _, err := s.Reroute("RR9", "S1", "站点D", "站点E", "修正预期", tClock(2026, 10, 5, 14, 0)); err != nil {
		t.Fatalf("失败不占号，修正后应成功: %v", err)
	}

	// 改址请求号独立去重，可与运输单号及其他业务编号同名。
	if _, _, err := s.Reroute("S1", "S1", "站点E", "站点F", "与运输单同号", tClock(2026, 10, 5, 15, 0)); err != nil {
		t.Fatalf("改址请求号与运输单号同名应允许: %v", err)
	}
}

// 改址受理失败：运输单不存在、新目的站等于当前或源站、全部接收后，均整次拒绝且台账不变。
func TestRerouteFailuresAtomic(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialShip(t, s)
	mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "第一次", tClock(2026, 10, 5, 11, 0))

	cases := []struct {
		name                          string
		request, shipment, expect, to string
	}{
		{"运输单不存在", "RRF1", "S9", "站点C", "站点D"},
		{"预期与当前有效目的站不符", "RRF2", "S1", "站点B", "站点D"},
		{"新目的站等于当前有效目的站", "RRF3", "S1", "站点C", "站点C"},
		{"新目的站等于源站", "RRF4", "S1", "站点C", "站点A"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.Reroute(tc.request, tc.shipment, tc.expect, tc.to, "原因", time.Now()); err == nil {
				t.Fatalf("%s：应整次拒绝", tc.name)
			}
			if s.RerouteOf(tc.request) != nil {
				t.Fatalf("%s：失败的首次改址不应占用请求号", tc.name)
			}
		})
	}
	// 全部接收后拒绝新改址。
	mustReceive(t, s, "RS1", "S1", "站点C", tClock(2026, 10, 5, 12, 0))
	if _, _, err := s.Reroute("RRF5", "S1", "站点C", "站点D", "全部接收后", time.Now()); err == nil {
		t.Fatalf("全部接收后应拒绝新改址")
	}
	if s.RerouteOf("RRF5") != nil {
		t.Fatalf("失败的首次改址不应占用请求号")
	}

	// 失败不留部分变更：重新打开后只有成功的 RR1 与 RS1。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	sh, _ := s2.ShipmentQuery("S1")
	if sh.ReceivedBy != "RS1" || len(sh.Reroutes) != 1 {
		t.Fatalf("失败不应留下部分变更: %+v", sh)
	}
}

// 改址不算到站、不恢复旧交接退回资格；在途件作业限制不变；ship 重放仍返回最初两站。
func TestRerouteKeepsRestrictions(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustHandoff(t, s, "H1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	mustShip(t, s, "S1", "站点B", "站点C", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	mustReroute(t, s, "RR1", "S1", "站点C", "站点D", "改址", tClock(2026, 10, 5, 11, 0))

	// 改址不恢复旧交接退回资格（发运已是新流转）。
	if _, _, err := s.Return("RT1", "H1", "错发", time.Now()); err == nil {
		t.Fatalf("改址后仍不能退回发运前的旧交接")
	}
	// 在途件作业限制不变：不能交接、出站、冻结。
	if _, _, err := s.Handoff("H9", "站点B", "站点E", []string{"P001"}, time.Now()); err == nil {
		t.Fatalf("在途件不能交接")
	}
	if _, _, err := s.Dispatch("B9", "站点B", "张三", []string{"P001"}, time.Now()); err == nil {
		t.Fatalf("在途件不能出站")
	}
	if _, _, err := s.Freeze("E9", "P001", "站点B", "异常", time.Now()); err == nil {
		t.Fatalf("在途件不能冻结")
	}
	// ship 重放仍返回最初两站、顺序和时间。
	sh, replayed, err := s.Ship("S1", "站点B", "站点C", []string{"P002", "P001"}, time.Now())
	if err != nil || !replayed || sh.From != "站点B" || sh.To != "站点C" ||
		!sameOrder(sh.Parcels, []string{"P001", "P002"}) {
		t.Fatalf("ship 重放应返回最初两站与顺序: %+v replayed=%v err=%v", sh, replayed, err)
	}
}

// 改址结果持久化：重启后规则不变；旧有效台账（无 reroutes 字段）直接使用。
func TestReroutePersistenceAndLegacy(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialShip(t, s)
	mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "第一次", tClock(2026, 10, 5, 11, 0))
	mustReceiveParcels(t, s, "RS1", "S1", "站点C", []string{"P001"}, tClock(2026, 10, 5, 12, 0))
	mustReroute(t, s, "RR2", "S1", "站点C", "站点D", "第二次", tClock(2026, 10, 5, 13, 0))

	// 重启后：有效目的站、集合、轨迹与接收站均按保存顺序核对通过。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重启后应能打开: %v", err)
	}
	sh, _ := s2.ShipmentQuery("S1")
	if eff := s2.effectiveTo(sh); eff != "站点D" {
		t.Fatalf("重启后有效目的站应为站点D: %q", eff)
	}
	rrs := s2.ShipmentReroutes("S1")
	if len(rrs) != 2 || rrs[0].Request != "RR1" || rrs[1].Request != "RR2" ||
		!sameOrder(rrs[0].Parcels, []string{"P001", "P002", "P003"}) ||
		!sameOrder(rrs[1].Parcels, []string{"P002", "P003"}) {
		t.Fatalf("重启后改址记录不符: %+v", rrs)
	}
	// 重启后重放仍返回首次结果。
	if got, replayed, err := s2.Reroute("RR1", "S1", "站点B", "站点C", "第一次", time.Now()); err != nil || !replayed ||
		!sameOrder(got.Parcels, []string{"P001", "P002", "P003"}) {
		t.Fatalf("重启后重放应返回首次集合: %+v replayed=%v err=%v", got, replayed, err)
	}
	// 重启后按有效目的站接收余件。
	if _, _, err := s2.Receive("RS2", "S1", "站点D", nil, tClock(2026, 10, 5, 14, 0)); err != nil {
		t.Fatalf("重启后应按有效目的站接收: %v", err)
	}

	// 旧有效台账（无 reroutes 字段）直接使用：能打开、能改址、能按新目的站接收。
	s4, path4 := openTempStore(t)
	setupPartialShip(t, s4)
	mustReceiveParcels(t, s4, "RS1", "S1", "站点B", []string{"P001"}, tClock(2026, 10, 5, 12, 0))
	raw, err := os.ReadFile(path4)
	if err != nil {
		t.Fatalf("读取数据文件失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	delete(m, "reroutes") // 旧版数据文件没有该字段
	buf, _ := json.Marshal(m)
	if err := os.WriteFile(path4, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(path4)
	if err != nil {
		t.Fatalf("旧台账（无 reroutes 字段）应能直接打开: %v", err)
	}
	res := mustReroute(t, s3, "RR9", "S1", "站点B", "站点E", "旧台账改址", tClock(2026, 10, 5, 15, 0))
	if !sameOrder(res.Parcels, []string{"P002", "P003"}) {
		t.Fatalf("旧台账改址集合应为当时未收件: %v", res.Parcels)
	}
	if _, _, err := s3.Receive("RS2", "S1", "站点E", nil, tClock(2026, 10, 5, 16, 0)); err != nil {
		t.Fatalf("旧台账改址后应按新目的站接收: %v", err)
	}
}

// 改址关联为空、缺失或矛盾时明确拒绝载入（ErrCorrupt，不崩溃、不覆盖）。
func TestRerouteCorrupt(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialShip(t, s)
	mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "第一次", tClock(2026, 10, 5, 11, 0))
	mustReceiveParcels(t, s, "RS1", "S1", "站点C", []string{"P001"}, tClock(2026, 10, 5, 12, 0))
	mustReroute(t, s, "RR2", "S1", "站点C", "站点D", "第二次", tClock(2026, 10, 5, 13, 0))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取数据文件失败: %v", err)
	}

	shipment := func(m map[string]any) map[string]any { return m["shipments"].(map[string]any)["S1"].(map[string]any) }
	reroute := func(m map[string]any, id string) map[string]any {
		return m["reroutes"].(map[string]any)[id].(map[string]any)
	}
	parcelTrail := func(m map[string]any, id string) []any {
		return m["parcels"].(map[string]any)[id].(map[string]any)["trail"].([]any)
	}

	mutate := func(name string, fn func(m map[string]any)) {
		t.Helper()
		corruptLedger(t, path, raw, name, fn)
		restoreLedger(t, path, raw)
	}

	mutate("运输单改址列表为空但存在改址结果", func(m map[string]any) {
		delete(shipment(m), "reroutes")
	})
	mutate("运输单改址列表引用不存在的请求", func(m map[string]any) {
		shipment(m)["reroutes"] = []any{"RR1", "RRX"}
	})
	mutate("改址结果缺失但运输单引用", func(m map[string]any) {
		delete(m["reroutes"].(map[string]any), "RR2")
	})
	mutate("改址链断裂（前目的站不符）", func(m map[string]any) {
		reroute(m, "RR2")["from"] = "站点B"
	})
	mutate("改址新目的站等于源站", func(m map[string]any) {
		reroute(m, "RR1")["to"] = "站点A"
	})
	mutate("改址集合缺少成员", func(m map[string]any) {
		reroute(m, "RR1")["parcels"] = []any{"P001", "P002"}
	})
	mutate("改址集合顺序与发运顺序不符", func(m map[string]any) {
		reroute(m, "RR1")["parcels"] = []any{"P002", "P001", "P003"}
	})
	mutate("改址结果与轨迹不一致", func(m map[string]any) {
		reroute(m, "RR1")["reason"] = "别的原因"
	})
	mutate("包裹缺少改址轨迹", func(m map[string]any) {
		trail := parcelTrail(m, "P002")
		kept := trail[:0]
		for _, ev := range trail {
			if ev.(map[string]any)["op"] != "改址" {
				kept = append(kept, ev)
			}
		}
		m["parcels"].(map[string]any)["P002"].(map[string]any)["trail"] = kept
	})
	mutate("接收站点与当时有效目的站矛盾", func(m map[string]any) {
		m["receives"].(map[string]any)["RS1"].(map[string]any)["station"] = "站点B"
	})
	mutate("接收轨迹目的站与当时有效目的站矛盾", func(m map[string]any) {
		trail := parcelTrail(m, "P001")
		for _, ev := range trail {
			e := ev.(map[string]any)
			if e["op"] == "接收" {
				e["to"] = "站点B"
				e["station"] = "站点B"
			}
		}
	})
	mutate("未收件缺少改址轨迹", func(m map[string]any) {
		// P003 未接收，应含全部两次改址轨迹；删掉最后一条改址。
		trail := parcelTrail(m, "P003")
		m["parcels"].(map[string]any)["P003"].(map[string]any)["trail"] = trail[:len(trail)-1]
	})

	// 篡改后查询、修改与重放均明确拒绝（Open 即失败）。
	corruptLedger(t, path, raw, "改址关联矛盾", func(m map[string]any) {
		delete(m["reroutes"].(map[string]any), "RR1")
	})
	var outErr strings.Builder
	if code := run([]string{"--data", path, "query", "--id", "P001"}, &strings.Builder{}, &outErr); code != exitBusiness {
		t.Fatalf("损坏台账 query 应以 1 退出: code=%d", code)
	}
	restoreLedger(t, path, raw)
}

// 并发改址：同一运输单并发提交不同改址，至多一个成功，结果符合某个先后顺序。
func TestConcurrentReroute(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	for _, args := range [][]string{
		{"register", "--id", "P001", "--station", "站点A"},
		{"register", "--id", "P002", "--station", "站点A"},
		{"ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B", "--parcel", "P001", "--parcel", "P002"},
	} {
		if _, errText, code := runCLI(t, dbPath, args...); code != 0 {
			t.Fatalf("准备台账失败: %v code=%d err=%s", args, code, errText)
		}
	}

	// 两个并发改址（不同新目的站）：恰好一个成功；随后以另一个新目的站、
	// 用胜者造成的新有效目的站为预期再改址，可能成功也可能因预期不符失败——
	// 最终结果必须符合某个先后顺序。
	const n = 2
	_, _, codes := runParallel(t, n, func(i int) []string {
		to := []string{"站点C", "站点D"}[i]
		return []string{"--data", dbPath, "reroute", "--request", fmt.Sprintf("RR%d", i+1), "--shipment", "S1",
			"--expect", "站点B", "--to", to, "--reason", "并发改址"}
	})
	if got := countCodes(codes, exitOK); got != 1 {
		t.Fatalf("并发改址同一运输单应恰好成功一次，成功 %d 次（codes=%v）", got, codes)
	}
	if got := countCodes(codes, exitBusiness); got != n-1 {
		t.Fatalf("其余改址应以状态码 1 退出，得到 %d 个（codes=%v）", got, codes)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	sh, _ := s.ShipmentQuery("S1")
	if len(sh.Reroutes) != 1 {
		t.Fatalf("应只有一次改址生效: %v", sh.Reroutes)
	}
	eff := s.effectiveTo(sh)
	if eff != "站点C" && eff != "站点D" {
		t.Fatalf("有效目的站应为胜者的新目的站: %q", eff)
	}
	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		last := p.Trail[len(p.Trail)-1]
		if last.Op != "改址" || last.Request != sh.Reroutes[0] {
			t.Fatalf("包裹 %q 末条轨迹应为胜出的改址: %+v", id, last)
		}
	}
}

// 并发改址与接收：改址成功后旧目的站接收必失败，二者至多一方成功。
func TestConcurrentRerouteAndReceive(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	for _, args := range [][]string{
		{"register", "--id", "P001", "--station", "站点A"},
		{"register", "--id", "P002", "--station", "站点A"},
		{"ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B", "--parcel", "P001", "--parcel", "P002"},
	} {
		if _, errText, code := runCLI(t, dbPath, args...); code != 0 {
			t.Fatalf("准备台账失败: %v code=%d err=%s", args, code, errText)
		}
	}

	_, _, codes := runParallel(t, 2, func(i int) []string {
		if i == 0 {
			return []string{"--data", dbPath, "reroute", "--request", "RR1", "--shipment", "S1",
				"--expect", "站点B", "--to", "站点C", "--reason", "改址"}
		}
		return []string{"--data", dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B"}
	})
	if got := countCodes(codes, exitOK); got != 1 {
		t.Fatalf("改址与旧目的站接收并发应恰好一方成功，成功 %d 次（codes=%v）", got, codes)
	}
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	sh, _ := s.ShipmentQuery("S1")
	rr := s.RerouteOf("RR1")
	rs := s.ReceiveOf("RS1")
	if (rr != nil) == (rs != nil) {
		t.Fatalf("应恰好改址或接收一方生效: rr=%+v rs=%+v", rr, rs)
	}
	if rr != nil && len(sh.Reroutes) != 1 {
		t.Fatalf("改址生效时运输单应记录改址: %+v", sh)
	}
}

// 改址后 receive 显式集合与不选成员均按受理时有效目的站接收。
func TestRerouteReceiveModes(t *testing.T) {
	s, _ := openTempStore(t)
	setupPartialShip(t, s)
	mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "改址", tClock(2026, 10, 5, 11, 0))

	// 显式集合在新目的站接收。
	mustReceiveParcels(t, s, "RS1", "S1", "站点C", []string{"P002"}, tClock(2026, 10, 5, 12, 0))
	// 显式集合在旧目的站：整次拒绝。
	if _, _, err := s.Receive("RS2", "S1", "站点B", []string{"P001"}, time.Now()); err == nil {
		t.Fatalf("显式集合在旧目的站接收应整次拒绝")
	}
	// 不选成员在新目的站接收余件。
	rs := mustReceive(t, s, "RS3", "S1", "站点C", tClock(2026, 10, 5, 13, 0))
	if !sameOrder(rs.Parcels, []string{"P001", "P003"}) {
		t.Fatalf("不选成员应接收当时全部未收件: %v", rs.Parcels)
	}
}

// 改址后接收事实永久保留：接收站为当时有效目的站，再次改址不影响已有接收。
func TestRerouteAfterReceiveKeepsFacts(t *testing.T) {
	s, _ := openTempStore(t)
	setupPartialShip(t, s)
	mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "第一次", tClock(2026, 10, 5, 11, 0))
	mustReceiveParcels(t, s, "RS1", "S1", "站点C", []string{"P001"}, tClock(2026, 10, 5, 12, 0))
	mustReroute(t, s, "RR2", "S1", "站点C", "站点D", "第二次", tClock(2026, 10, 5, 13, 0))

	// 已有接收事实与轨迹永久保留：P001 仍归站点C，接收记录不变。
	p1, _ := s.Query("P001")
	if p1.Station != "站点C" || p1.Status != statusInStation {
		t.Fatalf("再次改址不应改变已收件: %+v", p1)
	}
	rv := s.ShipmentReceiveOf("S1", "P001")
	if rv == nil || rv.Station != "站点C" || rv.Request != "RS1" {
		t.Fatalf("已有接收事实应保留: %+v", rv)
	}
	// RS1 重放仍返回站点C。
	if got, replayed, err := s.Receive("RS1", "S1", "站点C", []string{"P001"}, time.Now()); err != nil || !replayed ||
		got.Station != "站点C" {
		t.Fatalf("接收重放应返回原站点: %+v replayed=%v err=%v", got, replayed, err)
	}
	// 再次改址后新接收按旧有效目的站应拒绝。
	if _, _, err := s.Receive("RS9", "S1", "站点C", nil, time.Now()); err == nil {
		t.Fatalf("新接收按旧有效目的站应拒绝")
	}
}
