package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// mustReceiveSelected 显式选择集合的分批接收，应首次受理成功。
func mustReceiveSelected(t *testing.T, s *Store, request, shipment, station string, parcels []string, now time.Time) *ReceiveResult {
	t.Helper()
	res, replayed, err := s.ReceiveSelected(request, shipment, station, parcels, now)
	if err != nil || replayed {
		t.Fatalf("ReceiveSelected(%q, %v) 意外失败: %v replayed=%v", request, parcels, err, replayed)
	}
	return res
}

// 准备三件在站点A 在站的包裹 P001、P002、P003 并整单发运 S1 到站点B。
func setupPartialShip(t *testing.T, s *Store) time.Time {
	t.Helper()
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	shipTime := tClock(2026, 10, 5, 10, 0)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002", "P003"}, shipTime)
	return shipTime
}

// 分批接收：先到件入站作业、未到件继续在途；显式集合按原发运顺序保存。
func TestPartialReceiveExplicitBatch(t *testing.T) {
	s, _ := openTempStore(t)
	setupPartialShip(t, s)

	recvTime := tClock(2026, 10, 5, 12, 0)
	// 显式集合换序提交，结果按原发运顺序保存。
	res := mustReceiveSelected(t, s, "RS1", "S1", "站点B", []string{"P003", "P001"}, recvTime)
	if !res.Explicit {
		t.Fatalf("显式集合接收应标记 Explicit: %+v", res)
	}
	if !sameOrder(res.Parcels, []string{"P001", "P003"}) {
		t.Fatalf("本次集合应按原发运顺序保存: %v", res.Parcels)
	}
	// 已收件改归目的站在站，各追加接收轨迹。
	for _, id := range []string{"P001", "P003"} {
		p, _ := s.Query(id)
		if p.Status != statusInStation || p.Station != "站点B" {
			t.Fatalf("已收件 %q 应归目的站在站: status=%q station=%q", id, p.Status, p.Station)
		}
		last := p.Trail[len(p.Trail)-1]
		if last.Op != "接收" || last.Shipment != "S1" || last.Request != "RS1" ||
			last.From != "站点A" || last.To != "站点B" || !last.Time.Equal(recvTime) {
			t.Fatalf("已收件 %q 缺少接收轨迹: %+v", id, last)
		}
		if sh := s.ActiveShipment(id); sh != nil {
			t.Fatalf("已收件 %q 不应再显示当前运输单", id)
		}
	}
	// 未选余件保持源站在途及作业限制。
	p2, _ := s.Query("P002")
	if p2.Status != statusInTransit || p2.Station != "站点A" || len(p2.Trail) != 2 {
		t.Fatalf("余件 P002 应保持源站在途: %+v", p2)
	}
	if sh := s.ActiveShipment("P002"); sh == nil || sh.Shipment != "S1" {
		t.Fatalf("余件 P002 应仍归 S1 在途")
	}
	if _, _, err := s.Handoff("R1", "站点A", "站点C", []string{"P002"}, tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatalf("余件在途期间应不能交接")
	}
	// 运输单部分接收：原成员与发运时间不变，尚未标记全部接收。
	sh, err := s.ShipmentQuery("S1")
	if err != nil {
		t.Fatalf("运输单查询失败: %v", err)
	}
	if sh.ReceivedBy != "" || !sameOrder(sh.Parcels, []string{"P001", "P002", "P003"}) ||
		!sh.Time.Equal(tClock(2026, 10, 5, 10, 0)) {
		t.Fatalf("部分接收时运输单不应标记已接收，原成员与发运时间不变: %+v", sh)
	}
}

// 已收件的后续交接、冻结、配送或再次发运不妨碍余件接收；余件补齐后全部接收。
func TestPartialReceiveRemainingAfterFollowup(t *testing.T) {
	s, _ := openTempStore(t)
	setupPartialShip(t, s)
	mustReceiveSelected(t, s, "RS1", "S1", "站点B", []string{"P001", "P003"}, tClock(2026, 10, 5, 12, 0))

	// 已收件后续作业：P001 交接，P003 冻结后解除、出站并再次发运。
	mustHandoff(t, s, "R1", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 13, 0))
	mustFreeze(t, s, "E1", "P003", "站点B", "外包装破损", tClock(2026, 10, 5, 13, 0))
	if _, _, err := s.Unfreeze("U1", "E1", "已核实放行", tClock(2026, 10, 5, 13, 30)); err != nil {
		t.Fatalf("Unfreeze 意外失败: %v", err)
	}
	mustDispatch(t, s, "B1", "站点B", "张三", []string{"P003"}, tClock(2026, 10, 5, 14, 0))
	mustReceiptOK(t, s, "RC1", "B1", "P003", "失败", "收件人不在", tClock(2026, 10, 5, 15, 0))
	mustShip(t, s, "S2", "站点B", "站点D", []string{"P003"}, tClock(2026, 10, 5, 16, 0))

	// 不选成员方式补齐余件：首次受理取当时余件（仅 P002）。
	recvTime := tClock(2026, 10, 5, 17, 0)
	res := mustReceive(t, s, "RS2", "S1", "站点B", recvTime)
	if res.Explicit {
		t.Fatalf("不选成员方式不应标记 Explicit: %+v", res)
	}
	if !sameOrder(res.Parcels, []string{"P002"}) {
		t.Fatalf("余件集合应为首次受理时的当时余件: %v", res.Parcels)
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusInStation || p2.Station != "站点B" {
		t.Fatalf("补齐后 P002 应归目的站在站: %+v", p2)
	}
	// 全部接收：运输单永久标记，新请求拒绝，运输单号不释放。
	sh, _ := s.ShipmentQuery("S1")
	if sh.ReceivedBy != "RS2" {
		t.Fatalf("全部接收后运输单应标记完成接收的请求号: %+v", sh)
	}
	if _, _, err := s.Receive("RS9", "S1", "站点B", tClock(2026, 10, 5, 18, 0)); err == nil {
		t.Fatalf("全部接收后不选成员的新请求应拒绝")
	}
	if _, _, err := s.ReceiveSelected("RS10", "S1", "站点B", []string{"P002"}, tClock(2026, 10, 5, 18, 0)); err == nil {
		t.Fatalf("全部接收后显式集合的新请求应拒绝")
	}
	if _, _, err := s.Ship("S1", "站点A", "站点C", []string{"P001"}, tClock(2026, 10, 5, 18, 0)); err == nil {
		t.Fatalf("全部接收后运输单号不释放，换内容仍应冲突")
	}
}

// 两种选择方式共用接收请求号去重范围：同号同方式同内容重放返回首次结果；
// 换集合或切换方式冲突；重放不检查现状、不重算余件、不改写文件。
func TestPartialReceiveReplayAndConflict(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialShip(t, s)
	recvTime := tClock(2026, 10, 5, 12, 0)
	mustReceiveSelected(t, s, "RS1", "S1", "站点B", []string{"P001", "P003"}, recvTime)
	mustReceive(t, s, "RS2", "S1", "站点B", tClock(2026, 10, 5, 13, 0)) // 补齐 P002

	// 已收件后续流转：P001 交接离开目的站。
	mustHandoff(t, s, "R1", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 14, 0))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 显式集合换序重放：返回首次集合与时间，不检查现状。
	res, replayed, err := s.ReceiveSelected("RS1", "S1", "站点B", []string{"P003", "P001"}, tClock(2026, 10, 5, 15, 0))
	if err != nil || !replayed || !sameOrder(res.Parcels, []string{"P001", "P003"}) || !res.Time.Equal(recvTime) {
		t.Fatalf("显式集合换序重放应返回首次结果: %+v replayed=%v err=%v", res, replayed, err)
	}
	// 不选成员重放：全部接收及后续流转后仍返回首次集合（当时余件 P002）与时间。
	res2, replayed, err := s.Receive("RS2", "S1", "站点B", tClock(2026, 10, 5, 15, 0))
	if err != nil || !replayed || !sameOrder(res2.Parcels, []string{"P002"}) {
		t.Fatalf("不选成员重放应返回首次集合: %+v replayed=%v err=%v", res2, replayed, err)
	}
	// 同号改变集合或切换方式冲突。
	if _, _, err := s.ReceiveSelected("RS1", "S1", "站点B", []string{"P001"}, tClock(2026, 10, 5, 15, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同号改变显式集合应报冲突: %v", err)
	}
	if _, _, err := s.Receive("RS1", "S1", "站点B", tClock(2026, 10, 5, 15, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同号从显式切换为不选成员应报冲突: %v", err)
	}
	if _, _, err := s.ReceiveSelected("RS2", "S1", "站点B", []string{"P002"}, tClock(2026, 10, 5, 15, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同号从不选成员切换为显式集合应报冲突: %v", err)
	}
	// 重放不追加轨迹、不改写文件。
	p1, _ := s.Query("P001")
	if len(p1.Trail) != 4 {
		t.Fatalf("重放不应追加轨迹，得到 %d 条", len(p1.Trail))
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("重放不应改写数据文件")
	}
}

// 显式集合的失败情形：非原单成员、已收件、目的站不符，整次拒绝且不占用请求号。
func TestPartialReceiveExplicitFailures(t *testing.T) {
	s, _ := openTempStore(t)
	setupPartialShip(t, s)
	mustRegister(t, s, "P009", "站点A", tClock(2026, 10, 5, 9, 0))
	mustReceiveSelected(t, s, "RS1", "S1", "站点B", []string{"P001"}, tClock(2026, 10, 5, 12, 0))

	// 选中件不属于原单。
	if _, _, err := s.ReceiveSelected("RS2", "S1", "站点B", []string{"P002", "P009"}, tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatalf("选中件不属于原单应整次拒绝")
	}
	// 选中件已接收。
	if _, _, err := s.ReceiveSelected("RS2", "S1", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatalf("选中件已接收应整次拒绝")
	}
	// 接收站不是目的站。
	if _, _, err := s.ReceiveSelected("RS2", "S1", "站点C", []string{"P002"}, tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatalf("接收站不符应整次拒绝")
	}
	// 运输单不存在。
	if _, _, err := s.ReceiveSelected("RS2", "S404", "站点B", []string{"P002"}, tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatalf("运输单不存在应拒绝")
	}
	// 整次拒绝：请求号不占用，余件保持在途，纠正后可重试成功。
	if s.ReceiveOf("RS2") != nil {
		t.Fatalf("失败的首次接收不应占用请求号")
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusInTransit || p2.Station != "站点A" || len(p2.Trail) != 2 {
		t.Fatalf("失败的接收不应改变余件: %+v", p2)
	}
	res := mustReceiveSelected(t, s, "RS2", "S1", "站点B", []string{"P002", "P003"}, tClock(2026, 10, 5, 14, 0))
	if !sameOrder(res.Parcels, []string{"P002", "P003"}) {
		t.Fatalf("纠正后重试应按原发运顺序接收余件: %v", res.Parcels)
	}
	sh, _ := s.ShipmentQuery("S1")
	if sh.ReceivedBy != "RS2" {
		t.Fatalf("补齐后运输单应标记已接收: %+v", sh)
	}
}

// 接收算新流转：分批接收后不能退回发运前旧交接。
func TestPartialReceiveIsNewFlow(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s, "P002", "站点A", tClock(2026, 10, 5, 9, 0))
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	mustShip(t, s, "S1", "站点B", "站点C", []string{"P001", "P002"}, tClock(2026, 10, 5, 11, 0))
	mustReceiveSelected(t, s, "RS1", "S1", "站点C", []string{"P001"}, tClock(2026, 10, 5, 12, 0))
	if _, _, err := s.Return("RT1", "R1", "错发站点", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatalf("分批接收后不应能退回发运前旧交接")
	}
}

// 分批接收与后续流转持久化：重开后状态、进度、去重与重放保持。
func TestPartialReceivePersistence(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialShip(t, s)
	recvTime := tClock(2026, 10, 5, 12, 0)
	mustReceiveSelected(t, s, "RS1", "S1", "站点B", []string{"P003", "P001"}, recvTime)

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
		t.Fatalf("重开后部分接收不应标记已接收: %+v", sh)
	}
	// 重开后重放与冲突规则不变。
	if _, replayed, err := s2.ReceiveSelected("RS1", "S1", "站点B", []string{"P001", "P003"}, tClock(2026, 10, 5, 13, 0)); err != nil || !replayed {
		t.Fatalf("重开后显式重放应返回首次结果: replayed=%v err=%v", replayed, err)
	}
	if _, _, err := s2.Receive("RS1", "S1", "站点B", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatalf("重开后同号切换方式应报冲突")
	}
	// 重开后可继续受理余件补齐。
	mustReceive(t, s2, "RS2", "S1", "站点B", tClock(2026, 10, 5, 14, 0))
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("再次载入失败: %v", err)
	}
	sh3, _ := s3.ShipmentQuery("S1")
	if sh3.ReceivedBy != "RS2" {
		t.Fatalf("补齐后重开应标记已接收: %+v", sh3)
	}
}

// 旧有效台账无需转换：旧整单接收记录（无 explicit 字段）视为不选成员方式，
// 重放保留原结果，原发运及接收历史重放保持可用。
func TestPartialReceiveLegacyLedgerCompat(t *testing.T) {
	s, path := openTempStore(t)
	setupShipBase(t, s)
	shipTime := tClock(2026, 10, 5, 10, 0)
	recvTime := tClock(2026, 10, 5, 12, 0)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002"}, shipTime)
	mustReceive(t, s, "RS1", "S1", "站点B", recvTime)

	// 不选成员方式写出的记录不含 explicit 字段，与旧版整单接收格式一致。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "explicit") {
		t.Fatalf("不选成员方式的接收记录不应包含 explicit 字段: %s", raw)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	rv := ledgerReceive(m, "RS1")
	if rv["request"] != "RS1" || rv["shipment"] != "S1" || rv["station"] != "站点B" {
		t.Fatalf("旧格式接收记录不符: %v", rv)
	}

	// 旧台账直接打开：状态、轨迹、接收标记可用，历史重放保持。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("旧台账应能直接打开: %v", err)
	}
	sh, _ := s2.ShipmentQuery("S1")
	if sh.ReceivedBy != "RS1" {
		t.Fatalf("旧台账接收标记应保持: %+v", sh)
	}
	res, replayed, err := s2.Receive("RS1", "S1", "站点B", tClock(2026, 10, 6, 9, 0))
	if err != nil || !replayed || !sameOrder(res.Parcels, []string{"P001", "P002"}) || !res.Time.Equal(recvTime) {
		t.Fatalf("旧整单接收应按不选成员方式重放原结果: %+v replayed=%v err=%v", res, replayed, err)
	}
	if _, replayed, err := s2.Ship("S1", "站点A", "站点B", []string{"P002", "P001"}, tClock(2026, 10, 6, 9, 0)); err != nil || !replayed {
		t.Fatalf("原发运历史重放应保持可用: replayed=%v err=%v", replayed, err)
	}
	// 旧记录视为不选成员方式：同号显式集合提交冲突，全部接收后新请求拒绝。
	if _, _, err := s2.ReceiveSelected("RS1", "S1", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 6, 9, 0)); err == nil {
		t.Fatalf("旧不选成员记录与同号显式集合应报冲突")
	}
	if _, _, err := s2.Receive("RS9", "S1", "站点B", tClock(2026, 10, 6, 9, 0)); err == nil {
		t.Fatalf("全部接收后新请求应拒绝")
	}
}

// 重载核对逐件接收事实：同单同件重复接收、成员顺序或接收标记与事实矛盾时
// 明确拒绝载入，不崩溃或覆盖；还原后旧有效台账直接使用。
func TestPartialReceiveCorruptRejected(t *testing.T) {
	s, path := openTempStore(t)
	setupPartialShip(t, s)
	mustReceiveSelected(t, s, "RS1", "S1", "站点B", []string{"P001"}, tClock(2026, 10, 5, 12, 0))
	mustReceiveSelected(t, s, "RS2", "S1", "站点B", []string{"P002"}, tClock(2026, 10, 5, 13, 0))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 同单同件重复接收：RS2 的集合改为包含已接收的 P001。
	corruptLedger(t, path, raw, "同单同件重复接收", func(m map[string]any) {
		ledgerReceive(m, "RS2")["parcels"] = []any{"P001", "P002"}
	})
	// 接收成员未按原发运顺序。
	corruptLedger(t, path, raw, "接收成员顺序矛盾", func(m map[string]any) {
		ledgerReceive(m, "RS2")["parcels"] = []any{"P003", "P002"}
	})
	// 接收成员不属于原单。
	corruptLedger(t, path, raw, "接收成员不属于原单", func(m map[string]any) {
		ledgerReceive(m, "RS2")["parcels"] = []any{"P404"}
	})
	// 接收结果为空关联。
	corruptLedger(t, path, raw, "接收成员为空", func(m map[string]any) {
		ledgerReceive(m, "RS2")["parcels"] = []any{}
	})
	corruptLedger(t, path, raw, "接收缺少运输单号", func(m map[string]any) {
		ledgerReceive(m, "RS2")["shipment"] = ""
	})
	// 部分接收却标记全部接收。
	corruptLedger(t, path, raw, "部分接收却标记已接收", func(m map[string]any) {
		ledgerShipment(m, "S1")["receivedBy"] = "RS2"
	})
	restoreLedger(t, path, raw)

	// 全部接收后缺少接收标记：先补齐，再篡改。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("还原后应能正常打开: %v", err)
	}
	mustReceive(t, s2, "RS3", "S1", "站点B", tClock(2026, 10, 5, 14, 0))
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corruptLedger(t, path, raw2, "全部接收缺少接收标记", func(m map[string]any) {
		delete(ledgerShipment(m, "S1"), "receivedBy")
	})
	// 已收件不再该单在途不构成损坏；未接收件离开在途才是矛盾。
	corruptLedger(t, path, raw2, "未接收件状态矛盾", func(m map[string]any) {
		delete(m["receives"].(map[string]any), "RS3")
		delete(ledgerShipment(m, "S1"), "receivedBy")
		trail := ledgerParcel(m, "P003")["trail"].([]any)
		ledgerParcel(m, "P003")["trail"] = trail[:len(trail)-1]
		ledgerParcel(m, "P003")["status"] = "在站"
	})
	restoreLedger(t, path, raw2)
}

// 并发分批接收：重叠成员至多一次生效，不重叠的合法接收全部保留。
func TestConcurrentPartialReceive(t *testing.T) {
	// 重叠成员：两个显式集合都含 P001，至多一个成功。
	dbPath := t.TempDir() + "/ledger.json"
	for _, id := range []string{"P001", "P002", "P003"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败", id)
		}
	}
	if _, _, code := runCLI(t, dbPath, "ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002", "--parcel", "P003"); code != 0 {
		t.Fatalf("发运失败")
	}
	_, _, codes := runParallel(t, 2, func(i int) []string {
		if i == 0 {
			return []string{"--data", dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B",
				"--parcel", "P001", "--parcel", "P002"}
		}
		return []string{"--data", dbPath, "receive", "--request", "RS2", "--shipment", "S1", "--station", "站点B",
			"--parcel", "P001", "--parcel", "P003"}
	})
	if got := countCodes(codes, exitOK); got != 1 {
		t.Fatalf("重叠成员的并发接收应至多一次生效，成功 %d 次（codes=%v）", got, codes)
	}
	if got := countCodes(codes, exitBusiness); got != 1 {
		t.Fatalf("重叠成员的另一接收应以状态码 1 退出（codes=%v）", codes)
	}

	// 不重叠成员：两个显式集合互不相交，全部保留。
	dbPath2 := t.TempDir() + "/ledger.json"
	for _, id := range []string{"P001", "P002"} {
		if _, _, code := runCLI(t, dbPath2, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败", id)
		}
	}
	if _, _, code := runCLI(t, dbPath2, "ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002"); code != 0 {
		t.Fatalf("发运失败")
	}
	_, _, codes = runParallel(t, 2, func(i int) []string {
		parcel := "P001"
		req := "RS1"
		if i == 1 {
			parcel = "P002"
			req = "RS2"
		}
		return []string{"--data", dbPath2, "receive", "--request", req, "--shipment", "S1", "--station", "站点B",
			"--parcel", parcel}
	})
	if got := countCodes(codes, exitOK); got != 2 {
		t.Fatalf("不重叠的合法接收应全部保留，成功 %d 次（codes=%v）", got, codes)
	}
	s, err := Open(dbPath2)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	sh, _ := s.ShipmentQuery("S1")
	if sh.ReceivedBy == "" {
		t.Fatalf("两批不重叠接收后运输单应全部接收: %+v", sh)
	}
	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		if p.Status != statusInStation || p.Station != "站点B" {
			t.Fatalf("包裹 %q 应归目的站在站: %+v", id, p)
		}
	}
}
