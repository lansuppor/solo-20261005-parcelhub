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

// mustReroute 提交一次运输途中改址，要求首次受理成功。
func mustReroute(t *testing.T, s *Store, request, shipment, expect, to, reason string, now time.Time) *RerouteResult {
	t.Helper()
	res, replayed, err := s.Reroute(request, shipment, expect, to, reason, now)
	if err != nil || replayed {
		t.Fatalf("Reroute(%q, %q -> %q) 意外失败: %v replayed=%v", request, expect, to, err, replayed)
	}
	return res
}

// 准备三件在站点A 在站的包裹 P001、P002、P003，并发运 S1（站点A -> 站点B）。
func setupRerouteShip(t *testing.T, s *Store) {
	t.Helper()
	t1 := tClock(2026, 10, 5, 9, 0)
	for _, id := range []string{"P001", "P002", "P003"} {
		mustRegister(t, s, id, "站点A", t1)
	}
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002", "P003"}, tClock(2026, 10, 5, 10, 0))
}

// 分批接收后改址：成员只取受理时全部未收件（按发运保存顺序），已收件不参与、
// 不变更，其后续合法作业不阻止改址；余件仍归源站、保持站间在途及作业限制；
// 此后按新目的站接收，旧目的站接收整次拒绝；改址前已成功的接收重放仍返回原站点。
func TestRerouteAfterPartialReceive(t *testing.T) {
	s, _ := openTempStore(t)
	setupRerouteShip(t, s)

	recvTime := tClock(2026, 10, 5, 12, 0)
	mustReceiveParcels(t, s, "RS1", "S1", "站点B", []string{"P001"}, recvTime)
	// 已收件的后续合法作业（交接）不阻止改址。
	mustHandoff(t, s, "R1", "站点B", "站点D", []string{"P001"}, tClock(2026, 10, 5, 12, 30))

	rrTime := tClock(2026, 10, 5, 13, 0)
	res := mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "目的站停收", rrTime)
	if res.From != "站点B" || res.To != "站点C" || res.Reason != "目的站停收" || !res.Time.Equal(rrTime) {
		t.Fatalf("改址结果不符: %+v", res)
	}
	// 本次集合为受理时全部未收件，按原发运顺序。
	if !sameOrder(res.Parcels, []string{"P002", "P003"}) {
		t.Fatalf("改址集合应为当时全部未收件（按发运顺序）: %v", res.Parcels)
	}
	if got := s.EffectiveDest("S1"); got != "站点C" {
		t.Fatalf("当前有效目的站应为站点C，得到 %q", got)
	}
	// 余件仍归源站、保持站间在途，逐件追加改址轨迹。
	for _, id := range []string{"P002", "P003"} {
		p, _ := s.Query(id)
		if p.Status != statusInTransit || p.Station != "站点A" {
			t.Fatalf("改址后 %q 应仍归源站在途: status=%q station=%q", id, p.Status, p.Station)
		}
		last := p.Trail[len(p.Trail)-1]
		if last.Op != "改址" || last.Request != "RR1" || last.Shipment != "S1" ||
			last.Station != "站点A" || last.From != "站点B" || last.To != "站点C" ||
			last.Reason != "目的站停收" || !last.Time.Equal(rrTime) {
			t.Fatalf("改址后 %q 缺少改址轨迹: %+v", id, last)
		}
		if sh := s.ActiveShipment(id); sh == nil || sh.Shipment != "S1" {
			t.Fatalf("改址后 %q 应仍归 S1 在途", id)
		}
		// 作业限制保持：在途件不能交接、出站、冻结。
		if _, _, err := s.Handoff("RH"+id, "站点A", "站点D", []string{id}, tClock(2026, 10, 5, 13, 30)); err == nil {
			t.Fatalf("改址后在途件 %q 应不能交接", id)
		}
		if _, _, err := s.Dispatch("RB"+id, "站点A", "张三", []string{id}, tClock(2026, 10, 5, 13, 30)); err == nil {
			t.Fatalf("改址后在途件 %q 应不能配送出站", id)
		}
		if _, _, err := s.Freeze("RE"+id, id, "站点A", "异常", tClock(2026, 10, 5, 13, 30)); err == nil {
			t.Fatalf("改址后在途件 %q 应不能冻结", id)
		}
	}
	// 已收件不参与、不变更：无改址轨迹，后续交接事实保留。
	p1, _ := s.Query("P001")
	if p1.Status != statusInStation || p1.Station != "站点D" {
		t.Fatalf("已收件不应被改址变更: %+v", p1)
	}
	for _, e := range p1.Trail {
		if e.Op == "改址" {
			t.Fatalf("已收件不应追加改址轨迹: %+v", p1.Trail)
		}
	}
	// 改址不算到站：运输单接收进度不变。
	sh, _ := s.ShipmentQuery("S1")
	if sh.ReceivedBy != "" {
		t.Fatalf("改址不应标记全部接收: %+v", sh)
	}
	// 改址前已成功的接收重放仍返回原站点、集合和时间。
	rv, replayed, err := s.Receive("RS1", "S1", "站点B", []string{"P001"}, tClock(2026, 10, 5, 14, 0))
	if err != nil || !replayed || rv.Station != "站点B" || !sameOrder(rv.Parcels, []string{"P001"}) || !rv.Time.Equal(recvTime) {
		t.Fatalf("改址前的接收重放应返回原站点、集合和时间: %+v replayed=%v err=%v", rv, replayed, err)
	}
	// 旧目的站的新接收整次拒绝；按新目的站接收成功。
	if _, _, err := s.Receive("RS2", "S1", "站点B", []string{"P002"}, tClock(2026, 10, 5, 14, 0)); err == nil {
		t.Fatalf("改址后按旧目的站接收应整次拒绝")
	}
	recv2 := mustReceiveParcels(t, s, "RS2", "S1", "站点C", []string{"P002"}, tClock(2026, 10, 5, 14, 0))
	if recv2.Station != "站点C" {
		t.Fatalf("改址后应按新目的站接收: %+v", recv2)
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusInStation || p2.Station != "站点C" {
		t.Fatalf("按新目的站接收后应归站点C 在站: %+v", p2)
	}
	// 不选成员方式也按当前有效目的站接收。
	mustReceive(t, s, "RS3", "S1", "站点C", tClock(2026, 10, 5, 15, 0))
	sh, _ = s.ShipmentQuery("S1")
	if sh.ReceivedBy != "RS3" {
		t.Fatalf("全部到齐后应标记完成接收: %+v", sh)
	}
	// 全部接收后拒绝新改址。
	if _, _, err := s.Reroute("RR9", "S1", "站点C", "站点D", "再次改址", tClock(2026, 10, 5, 16, 0)); err == nil {
		t.Fatalf("全部接收后应拒绝新改址")
	}
}

// 连续改址：每次以当时余件为准，有效目的站逐次更新；ship 重放仍返回最初两站；
// 改址不恢复旧交接退回资格。
func TestRerouteChain(t *testing.T) {
	s, _ := openTempStore(t)
	setupShipBase(t, s)
	// 发运前的旧交接：发运后已不能退回，改址也不恢复该资格。
	mustHandoff(t, s, "R0", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	mustHandoff(t, s, "R1", "站点B", "站点A", []string{"P001"}, tClock(2026, 10, 5, 9, 40))
	shipTime := tClock(2026, 10, 5, 10, 0)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002"}, shipTime)

	mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "目的站停收", tClock(2026, 10, 5, 11, 0))
	res2 := mustReroute(t, s, "RR2", "S1", "站点C", "站点D", "路由调整", tClock(2026, 10, 5, 12, 0))
	if res2.From != "站点C" || res2.To != "站点D" || !sameOrder(res2.Parcels, []string{"P001", "P002"}) {
		t.Fatalf("第二次改址应以当时余件为准: %+v", res2)
	}
	if got := s.EffectiveDest("S1"); got != "站点D" {
		t.Fatalf("连续改址后有效目的站应为站点D，得到 %q", got)
	}
	sh, _ := s.ShipmentQuery("S1")
	if !sameOrder(sh.Reroutes, []string{"RR1", "RR2"}) {
		t.Fatalf("改址列表应按提交顺序保存: %v", sh.Reroutes)
	}
	// 原发运结果保留：ship 重放仍返回最初两站、顺序和时间。
	shipRes, replayed, err := s.Ship("S1", "站点A", "站点B", []string{"P002", "P001"}, tClock(2026, 10, 5, 13, 0))
	if err != nil || !replayed || shipRes.From != "站点A" || shipRes.To != "站点B" ||
		!sameOrder(shipRes.Parcels, []string{"P001", "P002"}) || !shipRes.Time.Equal(shipTime) {
		t.Fatalf("ship 重放应返回最初两站、顺序和时间: %+v replayed=%v err=%v", shipRes, replayed, err)
	}
	// 改址不恢复旧交接退回资格。
	if _, _, err := s.Return("RT1", "R1", "错发站点", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatalf("改址后仍应不能退回发运前的旧交接")
	}
	// 中间目的站接收整次拒绝；按最终有效目的站接收。
	if _, _, err := s.Receive("RS1", "S1", "站点C", nil, tClock(2026, 10, 5, 14, 0)); err == nil {
		t.Fatalf("按中间目的站接收应整次拒绝")
	}
	mustReceiveParcels(t, s, "RS1", "S1", "站点D", []string{"P001"}, tClock(2026, 10, 5, 14, 0))
	// 部分接收后还可再次改址，集合只剩余件。
	res3 := mustReroute(t, s, "RR3", "S1", "站点D", "站点E", "再次调整", tClock(2026, 10, 5, 15, 0))
	if !sameOrder(res3.Parcels, []string{"P002"}) {
		t.Fatalf("部分接收后的改址集合应只剩余件: %v", res3.Parcels)
	}
	p1, _ := s.Query("P001")
	// P001 轨迹：收件、交接(R0)、交接(R1)、发运、改址(RR1)、改址(RR2)、接收(RS1)。
	if len(p1.Trail) != 7 {
		t.Fatalf("P001 轨迹应为 7 条，得到 %d", len(p1.Trail))
	}
	mustReceive(t, s, "RS2", "S1", "站点E", tClock(2026, 10, 5, 16, 0))
	sh, _ = s.ShipmentQuery("S1")
	if sh.ReceivedBy != "RS2" {
		t.Fatalf("全部到齐后应标记完成接收: %+v", sh)
	}
}

// 重放与冲突：同号同内容返回首次改址信息、集合与时间，不检查现状、不重算余件、
// 不改写台账（再次改址、接收和后续流转后仍成立）；换内容冲突；失败不占号。
func TestRerouteReplayAndConflict(t *testing.T) {
	s, path := openTempStore(t)
	setupRerouteShip(t, s)
	rrTime := tClock(2026, 10, 5, 11, 0)
	mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "目的站停收", rrTime)
	// 再次改址、接收和后续流转之后重放仍成立。
	mustReroute(t, s, "RR2", "S1", "站点C", "站点D", "路由调整", tClock(2026, 10, 5, 12, 0))
	mustReceiveParcels(t, s, "RS1", "S1", "站点D", []string{"P001"}, tClock(2026, 10, 5, 13, 0))
	mustHandoff(t, s, "R9", "站点D", "站点E", []string{"P001"}, tClock(2026, 10, 5, 13, 30))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	res, replayed, err := s.Reroute("RR1", "S1", "站点B", "站点C", "目的站停收", tClock(2026, 10, 5, 14, 0))
	if err != nil || !replayed || !sameOrder(res.Parcels, []string{"P001", "P002", "P003"}) ||
		res.From != "站点B" || res.To != "站点C" || !res.Time.Equal(rrTime) {
		t.Fatalf("重放应返回首次改址信息、集合与时间: %+v replayed=%v err=%v", res, replayed, err)
	}
	// 重放不追加轨迹、不改写文件。
	p2, _ := s.Query("P002")
	n := 0
	for _, e := range p2.Trail {
		if e.Op == "改址" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("重放不应追加改址轨迹，P002 应有 2 条改址轨迹，得到 %d", n)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("重放不应改写数据文件")
	}

	// 同号换内容（运输单、预期站、新目的站、原因任一不同）：冲突。
	mustShip(t, s, "S2", "站点E", "站点F", []string{"P001"}, tClock(2026, 10, 5, 14, 30))
	for _, tc := range []struct{ ship, expect, to, reason string }{
		{"S2", "站点E", "站点C", "目的站停收"},
		{"S1", "站点C", "站点C", "目的站停收"},
		{"S1", "站点B", "站点D", "目的站停收"},
		{"S1", "站点B", "站点C", "换个原因"},
	} {
		if _, _, err := s.Reroute("RR1", tc.ship, tc.expect, tc.to, tc.reason, tClock(2026, 10, 5, 15, 0)); err == nil ||
			!strings.Contains(err.Error(), "冲突") {
			t.Fatalf("同号换内容（%+v）应报冲突", tc)
		}
	}
	// 改址请求号可与其他业务编号同名。
	if _, _, err := s.Reroute("RS1", "S1", "站点D", "站点C", "与接收请求号同名", tClock(2026, 10, 5, 15, 0)); err != nil {
		t.Fatalf("改址请求号应可与接收请求号同名: %v", err)
	}
	// 失败的首次改址不占用请求号：预期站不符失败后，同号纠正可成功。
	if _, _, err := s.Reroute("RRX", "S1", "站点B", "站点F", "预期站不符", tClock(2026, 10, 5, 16, 0)); err == nil {
		t.Fatalf("预期站不符应失败")
	}
	if s.RerouteOf("RRX") != nil {
		t.Fatalf("失败的首次改址不应占用请求号")
	}
	mustReroute(t, s, "RRX", "S1", "站点C", "站点F", "纠正后重试", tClock(2026, 10, 5, 16, 30))
}

// 首次受理失败：运输单不存在、预期站不符、新目的站等于当前目的站或源站、
// 全部接收后改址，整次拒绝且不改变任何包裹、不占用请求号。
func TestRerouteFailuresAtomic(t *testing.T) {
	s, _ := openTempStore(t)
	setupRerouteShip(t, s)

	cases := []struct {
		name   string
		req    string
		ship   string
		expect string
		to     string
	}{
		{"运输单不存在", "RR1", "S404", "站点B", "站点C"},
		{"预期站不符", "RR2", "S1", "站点C", "站点D"},
		{"新目的站等于当前目的站", "RR3", "S1", "站点B", "站点B"},
		{"新目的站等于源站", "RR4", "S1", "站点B", "站点A"},
	}
	for _, tc := range cases {
		if _, _, err := s.Reroute(tc.req, tc.ship, tc.expect, tc.to, "原因", tClock(2026, 10, 5, 11, 0)); err == nil {
			t.Fatalf("%s：应整次拒绝", tc.name)
		}
		if s.RerouteOf(tc.req) != nil {
			t.Fatalf("%s：失败的首次改址不应占用请求号 %q", tc.name, tc.req)
		}
	}
	// 整次拒绝：包裹状态与轨迹不变，有效目的站不变。
	for _, id := range []string{"P001", "P002", "P003"} {
		p, _ := s.Query(id)
		if p.Status != statusInTransit || p.Station != "站点A" || len(p.Trail) != 2 {
			t.Fatalf("失败的改址不应改变包裹 %q: %+v", id, p)
		}
	}
	if got := s.EffectiveDest("S1"); got != "站点B" {
		t.Fatalf("失败的改址不应改变有效目的站，得到 %q", got)
	}
	// 全部接收后拒绝新改址。
	mustReceive(t, s, "RS1", "S1", "站点B", tClock(2026, 10, 5, 12, 0))
	if _, _, err := s.Reroute("RR5", "S1", "站点B", "站点C", "全部接收后", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatalf("全部接收后应拒绝新改址")
	}
	if s.RerouteOf("RR5") != nil {
		t.Fatalf("全部接收后失败的改址不应占用请求号")
	}
}

// 持久化与旧数据兼容：改址与分批接收后重载，目的站变更链、各次集合、轨迹与
// 接收站按保存顺序核对保持；无 reroutes 字段的旧台账直接使用，无需转换。
func TestReroutePersistenceAndLegacy(t *testing.T) {
	s, path := openTempStore(t)
	setupRerouteShip(t, s)
	mustReceiveParcels(t, s, "RS1", "S1", "站点B", []string{"P001"}, tClock(2026, 10, 5, 11, 0))
	mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "目的站停收", tClock(2026, 10, 5, 12, 0))
	mustReroute(t, s, "RR2", "S1", "站点C", "站点D", "路由调整", tClock(2026, 10, 5, 13, 0))
	mustReceiveParcels(t, s, "RS2", "S1", "站点D", []string{"P002"}, tClock(2026, 10, 5, 14, 0))

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开台账失败: %v", err)
	}
	if got := s2.EffectiveDest("S1"); got != "站点D" {
		t.Fatalf("重开后有效目的站应为站点D，得到 %q", got)
	}
	sh, _ := s2.ShipmentQuery("S1")
	if !sameOrder(sh.Reroutes, []string{"RR1", "RR2"}) {
		t.Fatalf("重开后改址列表应保持提交顺序: %v", sh.Reroutes)
	}
	rrs := s2.ShipmentReroutes("S1")
	if len(rrs) != 2 || !sameOrder(rrs[0].Parcels, []string{"P002", "P003"}) || !sameOrder(rrs[1].Parcels, []string{"P002", "P003"}) {
		t.Fatalf("重开后各次改址集合应保持: %+v", rrs)
	}
	// 接收站按当时有效目的站保留。
	if rv := s2.ShipmentReceiveOf("S1", "P002"); rv == nil || rv.Station != "站点D" {
		t.Fatalf("重开后 P002 的接收站应为站点D: %+v", rv)
	}
	if rv := s2.ShipmentReceiveOf("S1", "P001"); rv == nil || rv.Station != "站点B" {
		t.Fatalf("重开后 P001 的接收站应为改址前的站点B: %+v", rv)
	}
	// 重放与后续作业在重载后保持。
	if _, replayed, err := s2.Reroute("RR1", "S1", "站点B", "站点C", "目的站停收", tClock(2026, 10, 5, 15, 0)); err != nil || !replayed {
		t.Fatalf("重开后改址重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	mustReceive(t, s2, "RS3", "S1", "站点D", tClock(2026, 10, 5, 15, 30))
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("补齐接收后重开失败: %v", err)
	}
	sh, _ = s3.ShipmentQuery("S1")
	if sh.ReceivedBy != "RS3" {
		t.Fatalf("补齐接收并重载后应标记全部接收: %+v", sh)
	}

	// 旧数据兼容：没有 reroutes 字段的台账直接打开使用，无需手工修改。
	s4, path4 := openTempStore(t)
	setupShipBase(t, s4)
	mustShip(t, s4, "S1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	raw, err := os.ReadFile(path4)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "reroutes")
	buf, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path4, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	s5, err := Open(path4)
	if err != nil {
		t.Fatalf("无 reroutes 字段的旧台账应直接打开: %v", err)
	}
	if got := s5.EffectiveDest("S1"); got != "站点B" {
		t.Fatalf("旧台账有效目的站应为原目的站，得到 %q", got)
	}
	// 旧台账上改址、接收、ship 重放均正常。
	mustReroute(t, s5, "RR1", "S1", "站点B", "站点C", "目的站停收", tClock(2026, 10, 5, 11, 0))
	mustReceive(t, s5, "RS1", "S1", "站点C", tClock(2026, 10, 5, 12, 0))
	if _, replayed, err := s5.Ship("S1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 13, 0)); err != nil || !replayed {
		t.Fatalf("旧台账 ship 重放应返回最初两站: %v replayed=%v", err, replayed)
	}
}

// 改址关联为空、缺失或矛盾时明确拒绝载入，不崩溃、不覆盖。
func TestRerouteCorrupt(t *testing.T) {
	s, path := openTempStore(t)
	setupRerouteShip(t, s)
	mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "目的站停收", tClock(2026, 10, 5, 11, 0))
	mustReroute(t, s, "RR2", "S1", "站点C", "站点D", "路由调整", tClock(2026, 10, 5, 12, 0))
	mustReceiveParcels(t, s, "RS1", "S1", "站点D", []string{"P001", "P002"}, tClock(2026, 10, 5, 13, 0))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reroute := func(m map[string]any, req string) map[string]any {
		return m["reroutes"].(map[string]any)[req].(map[string]any)
	}
	shipment := func(m map[string]any) map[string]any {
		return m["shipments"].(map[string]any)["S1"].(map[string]any)
	}

	corruptLedger(t, path, raw, "改址结果为 null", func(m map[string]any) {
		m["reroutes"].(map[string]any)["RR1"] = nil
	})
	corruptLedger(t, path, raw, "运输单改址列表缺失", func(m map[string]any) {
		delete(shipment(m), "reroutes")
	})
	corruptLedger(t, path, raw, "改址结果未列入运输单", func(m map[string]any) {
		shipment(m)["reroutes"] = []any{"RR1"}
	})
	corruptLedger(t, path, raw, "改址表缺少运输单引用的请求", func(m map[string]any) {
		delete(m["reroutes"].(map[string]any), "RR2")
	})
	corruptLedger(t, path, raw, "目的站变更链断裂", func(m map[string]any) {
		reroute(m, "RR2")["from"] = "站点B"
	})
	corruptLedger(t, path, raw, "改址新目的站与源站相同", func(m map[string]any) {
		reroute(m, "RR1")["to"] = "站点A"
	})
	corruptLedger(t, path, raw, "改址集合缺失", func(m map[string]any) {
		delete(reroute(m, "RR1"), "parcels")
	})
	corruptLedger(t, path, raw, "改址集合与轨迹矛盾", func(m map[string]any) {
		reroute(m, "RR1")["parcels"] = []any{"P001", "P002"}
	})
	corruptLedger(t, path, raw, "改址集合顺序与发运矛盾", func(m map[string]any) {
		reroute(m, "RR1")["parcels"] = []any{"P003", "P002", "P001"}
	})
	corruptLedger(t, path, raw, "接收站与当时有效目的站矛盾", func(m map[string]any) {
		m["receives"].(map[string]any)["RS1"].(map[string]any)["station"] = "站点B"
	})
	corruptLedger(t, path, raw, "改址轨迹引用不存在的请求", func(m map[string]any) {
		p := ledgerParcel(m, "P001")
		trail := p["trail"].([]any)
		for _, e := range trail {
			ev := e.(map[string]any)
			if ev["op"] == "改址" {
				ev["request"] = "RR404"
			}
		}
	})
	corruptLedger(t, path, raw, "改址引用不存在的运输单", func(m map[string]any) {
		reroute(m, "RR1")["shipment"] = "S404"
	})
	restoreLedger(t, path, raw)
}

// 并发改址与接收：结果须符合某个先后顺序——改址先生效则旧目的站接收失败，
// 接收先生效则全部接收后改址失败；同号同内容的并发改址只生效一次。
func TestConcurrentRerouteAndReceive(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	for _, id := range []string{"P001", "P002"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败", id)
		}
	}
	if _, _, code := runCLI(t, dbPath, "ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002"); code != 0 {
		t.Fatalf("发运失败")
	}

	_, _, codes := runParallel(t, 2, func(i int) []string {
		if i == 0 {
			return []string{"--data", dbPath, "reroute", "--request", "RR1", "--shipment", "S1",
				"--expect", "站点B", "--to", "站点C", "--reason", "目的站停收"}
		}
		return []string{"--data", dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B"}
	})
	if got := countCodes(codes, exitOK); got != 1 {
		t.Fatalf("并发改址与旧目的站接收应恰好一方成功，成功 %d 次（codes=%v）", got, codes)
	}
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	if codes[0] == exitOK {
		// 改址先生效：接收因目的站不符失败，请求号不占用。
		if got := s.EffectiveDest("S1"); got != "站点C" {
			t.Fatalf("改址先生效时有效目的站应为站点C，得到 %q", got)
		}
		if s.ReceiveOf("RS1") != nil {
			t.Fatalf("失败的接收不应占用请求号 RS1")
		}
	} else {
		// 接收先生效：全部接收后改址失败，请求号不占用。
		if s.RerouteOf("RR1") != nil {
			t.Fatalf("失败的改址不应占用请求号 RR1")
		}
		sh, _ := s.ShipmentQuery("S1")
		if sh.ReceivedBy != "RS1" {
			t.Fatalf("接收先生效时运输单应已全部接收: %+v", sh)
		}
	}

	// 同号同内容的并发改址：只生效一次，其余返回首次结果。
	dbPath2 := filepath.Join(t.TempDir(), "ledger2.json")
	for _, id := range []string{"P001", "P002"} {
		if _, _, code := runCLI(t, dbPath2, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败", id)
		}
	}
	if _, _, code := runCLI(t, dbPath2, "ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002"); code != 0 {
		t.Fatalf("发运失败")
	}
	const n = 4
	outs, _, codes := runParallel(t, n, func(i int) []string {
		return []string{"--data", dbPath2, "reroute", "--request", "RR1", "--shipment", "S1",
			"--expect", "站点B", "--to", "站点C", "--reason", "目的站停收"}
	})
	if got := countCodes(codes, exitOK); got != n {
		t.Fatalf("同号同内容的并发改址应全部返回成功，成功 %d 次", got)
	}
	replays := 0
	for _, out := range outs {
		if strings.Contains(out, "返回首次保存的结果") {
			replays++
		}
	}
	if replays != n-1 {
		t.Fatalf("应有 %d 次重放首次结果，实际 %d 次", n-1, replays)
	}
	s2, err := Open(dbPath2)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	p1, _ := s2.Query("P001")
	reroutes := 0
	for _, e := range p1.Trail {
		if e.Op == "改址" {
			reroutes++
		}
	}
	if reroutes != 1 {
		t.Fatalf("同号同内容只应生效一次（一条改址轨迹），实际 %d 条", reroutes)
	}
}

// CLI 端到端：改址、query 展示当前有效目的站与改址轨迹、shipment 区分原目的站
// 与当前有效目的站并列出改址记录及集合、逐件实际接收站；输入校验失败退出 1。
func TestCLReroute(t *testing.T) {
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
	if _, _, code := runCLI(t, dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B",
		"--parcel", "P001"); code != 0 {
		t.Fatalf("分批接收失败")
	}

	// 输入清理后为空：退出 1。
	if _, _, code := runCLI(t, dbPath, "reroute", "--request", "RR1", "--shipment", "S1",
		"--expect", "站点B", "--to", "站点C", "--reason", "   "); code != exitBusiness {
		t.Fatalf("原因为空白应以 1 退出，得到 %d", code)
	}
	if _, _, code := runCLI(t, dbPath, "reroute", "--request", "RR1", "--shipment", "S1",
		"--expect", "站点B", "--reason", "目的站停收"); code != exitBusiness {
		t.Fatalf("缺少新目的站应以 1 退出，得到 %d", code)
	}

	out, _, code := runCLI(t, dbPath, "reroute", "--request", " RR1 ", "--shipment", " S1 ",
		"--expect", " 站点B ", "--to", " 站点C ", "--reason", " 目的站停收 ")
	if code != 0 || !strings.Contains(out, "改址成功") || !strings.Contains(out, "改址请求号: RR1") ||
		!strings.Contains(out, "原目的站: 站点B") || !strings.Contains(out, "新目的站: 站点C") ||
		!strings.Contains(out, "P002") || !strings.Contains(out, "P003") || strings.Contains(out, "P001") {
		t.Fatalf("改址输出不符: code=%d out=%s", code, out)
	}
	// 同号同内容（含两端空白）重放。
	out, _, code = runCLI(t, dbPath, "reroute", "--request", "RR1", "--shipment", "S1",
		"--expect", "站点B", "--to", "站点C", "--reason", "目的站停收")
	if code != 0 || !strings.Contains(out, "重复提交") {
		t.Fatalf("同号同内容应重放首次结果: code=%d out=%s", code, out)
	}

	// query：在途件展示当前有效目的站与完整改址轨迹。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P002")
	if code != 0 || !strings.Contains(out, "当前状态: 站间在途") ||
		!strings.Contains(out, "当前运输单: S1") || !strings.Contains(out, "当前目的站: 站点C") ||
		!strings.Contains(out, "操作: 改址") || !strings.Contains(out, "改址请求号: RR1") {
		t.Fatalf("query 应展示当前有效目的站与改址轨迹: code=%d out=%s", code, out)
	}
	// shipment：区分原目的站与当前有效目的站，保留接收进度，列出改址记录及集合。
	out, _, code = runCLI(t, dbPath, "shipment", "--id", "S1")
	if code != 0 || !strings.Contains(out, "目的站点: 站点B") || !strings.Contains(out, "当前目的站: 站点C") ||
		!strings.Contains(out, "接收进度: 1/3") || !strings.Contains(out, "改址记录（1 条") ||
		!strings.Contains(out, "改址请求号: RR1") || !strings.Contains(out, "P001    已接收    接收请求号: RS1    接收站点: 站点B") {
		t.Fatalf("shipment 应区分原目的站与当前目的站并列出改址记录: code=%d out=%s", code, out)
	}

	// 旧目的站接收整次拒绝（退出 1），新目的站接收成功并展示实际接收站。
	if _, _, code := runCLI(t, dbPath, "receive", "--request", "RS2", "--shipment", "S1", "--station", "站点B"); code != exitBusiness {
		t.Fatalf("改址后按旧目的站接收应以 1 退出，得到 %d", code)
	}
	if _, _, code := runCLI(t, dbPath, "receive", "--request", "RS2", "--shipment", "S1", "--station", "站点C"); code != 0 {
		t.Fatalf("按新目的站接收失败")
	}
	out, _, code = runCLI(t, dbPath, "shipment", "--id", "S1")
	if code != 0 || !strings.Contains(out, "运输单状态: 已接收") ||
		!strings.Contains(out, "P002    已接收    接收请求号: RS2    接收站点: 站点C") {
		t.Fatalf("shipment 应逐件显示实际接收站: code=%d out=%s", code, out)
	}
	// 全部接收后新改址拒绝（退出 1）。
	if _, _, code := runCLI(t, dbPath, "reroute", "--request", "RR9", "--shipment", "S1",
		"--expect", "站点C", "--to", "站点D", "--reason", "再次改址"); code != exitBusiness {
		t.Fatalf("全部接收后新改址应以 1 退出，得到 %d", code)
	}
}

// 确保改址篡改场景经 errors.Is 识别为 ErrCorrupt（防止包装丢失）。
func TestRerouteCorruptIsErrCorrupt(t *testing.T) {
	s, path := openTempStore(t)
	setupRerouteShip(t, s)
	mustReroute(t, s, "RR1", "S1", "站点B", "站点C", "目的站停收", tClock(2026, 10, 5, 11, 0))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["reroutes"].(map[string]any)["RR1"].(map[string]any)["to"] = "站点D"
	buf, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("改址记录与轨迹矛盾应识别为数据损坏: %v", err)
	}
}
