package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func mustUnfreeze(t *testing.T, s *Store, request, incident, note string, now time.Time) {
	t.Helper()
	if _, replayed, err := s.Unfreeze(request, incident, note, now); err != nil || replayed {
		t.Fatalf("Unfreeze(%q) 意外失败: %v replayed=%v", request, err, replayed)
	}
}

// shipLedgerBase 建立一件已发运并已接收的合法台账（P001、P002 随 S1 从站点A 到站点B），
// 返回原始文件字节，供各损坏用例在其上做局部篡改。
func shipLedgerBase(t *testing.T) (path string, raw []byte) {
	t.Helper()
	s, path := openTempStore(t)
	setupShipBase(t, s)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	mustReceive(t, s, "RS1", "S1", "站点B", tClock(2026, 10, 5, 12, 0))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取数据文件失败: %v", err)
	}
	return path, raw
}

// mutateLedger 把 raw 台账经 fn 篡改后写回 path，并断言重新载入明确拒绝
// （返回 ErrCorrupt，不崩溃、不当作空库）；篡改后文件字节保持为篡改内容。
func mutateLedger(t *testing.T, path string, raw []byte, fn func(m map[string]any)) {
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
		t.Fatalf("损坏台账应明确拒绝载入，得到 err=%v", err)
	}
}

func ledgerParcel(m map[string]any, id string) map[string]any {
	return m["parcels"].(map[string]any)[id].(map[string]any)
}

func ledgerShipment(m map[string]any, no string) map[string]any {
	return m["shipments"].(map[string]any)[no].(map[string]any)
}

func ledgerReceive(m map[string]any, req string) map[string]any {
	return m["receives"].(map[string]any)[req].(map[string]any)
}

// trailEvents 返回包裹轨迹中指定操作的全部事件（按保存顺序）。
func trailEvents(m map[string]any, id, op string) []map[string]any {
	var out []map[string]any
	for _, e := range ledgerParcel(m, id)["trail"].([]any) {
		ev := e.(map[string]any)
		if ev["op"] == op {
			out = append(out, ev)
		}
	}
	return out
}

// 运输单、接收结果及其关联成员为 null 时必须明确拒绝，不崩溃、不当作空库。
func TestShipReceiveNullAssociations(t *testing.T) {
	path, raw := shipLedgerBase(t)

	mutateLedger(t, path, raw, func(m map[string]any) {
		m["shipments"].(map[string]any)["S1"] = nil
	})
	mutateLedger(t, path, raw, func(m map[string]any) {
		m["receives"].(map[string]any)["RS1"] = nil
	})
	mutateLedger(t, path, raw, func(m map[string]any) {
		ledgerShipment(m, "S1")["parcels"] = []any{nil, "P002"}
	})
	mutateLedger(t, path, raw, func(m map[string]any) {
		ledgerReceive(m, "RS1")["parcels"] = []any{"P001", nil}
	})
	mutateLedger(t, path, raw, func(m map[string]any) {
		m["parcels"].(map[string]any)["P001"] = nil
	})

	// 还原有效文件后正常可用。
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("还原后应能正常打开: %v", err)
	}
}

// 接收轨迹的源站必须等于运输单源站，两站与保存结果不一致即损坏。
func TestReceiveTrailStationMismatch(t *testing.T) {
	path, raw := shipLedgerBase(t)

	// 接收轨迹源站被改成非运输单源站。
	mutateLedger(t, path, raw, func(m map[string]any) {
		trailEvents(m, "P001", "接收")[0]["from"] = "站点C"
	})
	// 接收轨迹目的站被改成非运输单目的站。
	mutateLedger(t, path, raw, func(m map[string]any) {
		ev := trailEvents(m, "P001", "接收")[0]
		ev["to"] = "站点C"
		ev["station"] = "站点C"
		ledgerReceive(m, "RS1")["station"] = "站点C"
	})
	// 接收轨迹时间与保存的接收结果不一致。
	mutateLedger(t, path, raw, func(m map[string]any) {
		trailEvents(m, "P001", "接收")[0]["time"] = "2026-10-05T13:00:00+08:00"
	})
	// 发运轨迹目的站与运输单不一致。
	mutateLedger(t, path, raw, func(m map[string]any) {
		trailEvents(m, "P001", "发运")[0]["to"] = "站点C"
	})
}

// 发运、接收轨迹缺失或重复时明确拒绝。
func TestShipReceiveTrailMissingOrDuplicate(t *testing.T) {
	path, raw := shipLedgerBase(t)

	// 成员缺少发运轨迹。
	mutateLedger(t, path, raw, func(m map[string]any) {
		p := ledgerParcel(m, "P001")
		trail := p["trail"].([]any)
		kept := trail[:0]
		for _, e := range trail {
			if e.(map[string]any)["op"] != "发运" {
				kept = append(kept, e)
			}
		}
		p["trail"] = kept
	})
	// 成员缺少接收轨迹（运输单已接收）。
	mutateLedger(t, path, raw, func(m map[string]any) {
		p := ledgerParcel(m, "P002")
		trail := p["trail"].([]any)
		p["trail"] = trail[:len(trail)-1]
	})
	// 同一运输单的发运轨迹重复。
	mutateLedger(t, path, raw, func(m map[string]any) {
		p := ledgerParcel(m, "P001")
		ship := trailEvents(m, "P001", "发运")[0]
		p["trail"] = append(p["trail"].([]any), ship)
	})
	// 同一接收请求的接收轨迹重复。
	mutateLedger(t, path, raw, func(m map[string]any) {
		p := ledgerParcel(m, "P001")
		recv := trailEvents(m, "P001", "接收")[0]
		p["trail"] = append(p["trail"].([]any), recv)
	})
}

// 生命周期按轨迹保存顺序判定：接收轨迹位于发运之前即损坏。
func TestReceiveTrailBeforeShip(t *testing.T) {
	path, raw := shipLedgerBase(t)

	mutateLedger(t, path, raw, func(m map[string]any) {
		p := ledgerParcel(m, "P001")
		trail := p["trail"].([]any)
		// 轨迹为 收件/发运/接收：交换发运与接收的保存顺序（时间不变）。
		trail[1], trail[2] = trail[2], trail[1]
	})
}

// 发运后到接收前不得出现其他作业轨迹。
func TestOtherOpsBetweenShipAndReceive(t *testing.T) {
	path, raw := shipLedgerBase(t)

	// 在发运与接收之间夹入一条交接轨迹。
	mutateLedger(t, path, raw, func(m map[string]any) {
		p := ledgerParcel(m, "P001")
		trail := p["trail"].([]any)
		mid := map[string]any{
			"op":      "交接",
			"station": "站点B",
			"request": "R9",
			"time":    "2026-10-05T11:00:00+08:00",
		}
		trail = append(trail, nil)
		copy(trail[3:], trail[2:])
		trail[2] = mid
		p["trail"] = trail
	})
	// 在发运与接收之间夹入一条冻结轨迹。
	mutateLedger(t, path, raw, func(m map[string]any) {
		p := ledgerParcel(m, "P001")
		trail := p["trail"].([]any)
		mid := map[string]any{
			"op":       "冻结",
			"station":  "站点A",
			"incident": "E9",
			"reason":   "异常",
			"time":     "2026-10-05T11:00:00+08:00",
		}
		trail = append(trail, nil)
		copy(trail[3:], trail[2:])
		trail[2] = mid
		p["trail"] = trail
	})
	// 未接收时在发运之后追加其他作业轨迹（发运必须是最后一条轨迹）。
	mutateLedger(t, path, raw, func(m map[string]any) {
		// 先撤掉接收：运输单回到未接收，包裹回到在途。
		delete(m, "receives")
		delete(ledgerShipment(m, "S1"), "receivedBy")
		for _, id := range []string{"P001", "P002"} {
			p := ledgerParcel(m, id)
			trail := p["trail"].([]any)
			p["trail"] = trail[:len(trail)-1]
			p["station"] = "站点A"
			p["status"] = "站间在途"
		}
		// 再在 P001 的发运后夹入一条交接轨迹。
		p := ledgerParcel(m, "P001")
		p["trail"] = append(p["trail"].([]any), map[string]any{
			"op":      "交接",
			"station": "站点C",
			"request": "R9",
			"time":    "2026-10-05T11:00:00+08:00",
		})
	})
}

// 接收后没有后续作业时，成员必须在目的站在站；站点或状态矛盾即损坏。
func TestPostReceiveStatusContradiction(t *testing.T) {
	path, raw := shipLedgerBase(t)

	// 最后一条轨迹是接收，但归属站点不在目的站。
	mutateLedger(t, path, raw, func(m map[string]any) {
		ledgerParcel(m, "P001")["station"] = "站点A"
	})
	// 最后一条轨迹是接收，但状态不是在站。
	mutateLedger(t, path, raw, func(m map[string]any) {
		ledgerParcel(m, "P001")["status"] = "站间在途"
	})
	mutateLedger(t, path, raw, func(m map[string]any) {
		ledgerParcel(m, "P001")["status"] = "配送中"
	})
}

// 接收后的合法后续流转（交接、再次发运、配送回执及其撤销、中止、续接、
// 冻结和解除）在重新载入后照常受理；发运、接收同内容重放仍返回首次结果。
func TestPostReceiveFollowUpsReloadAndReplay(t *testing.T) {
	s, path := openTempStore(t)
	setupShipBase(t, s)
	shipTime := tClock(2026, 10, 5, 10, 0)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002"}, shipTime)
	recvTime := tClock(2026, 10, 5, 12, 0)
	mustReceive(t, s, "RS1", "S1", "站点B", recvTime)

	// P001：交接 -> 再次发运并接收 -> 出站 -> 签收回执 -> 撤销 -> 中止 ->
	// 再次出站 -> 续接 -> 失败回执 -> 冻结 -> 解除。
	mustHandoff(t, s, "R1", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 13, 0))
	mustShip(t, s, "S2", "站点C", "站点D", []string{"P001"}, tClock(2026, 10, 5, 14, 0))
	mustReceive(t, s, "RS2", "S2", "站点D", tClock(2026, 10, 5, 15, 0))
	mustDispatch(t, s, "B1", "站点D", "张三", []string{"P001"}, tClock(2026, 10, 5, 16, 0))
	mustReceiptOK(t, s, "RC1", "B1", "P001", "签收", "", tClock(2026, 10, 5, 17, 0))
	mustRevoke(t, s, "RV1", "RC1", "误录签收，实际仍配送中", tClock(2026, 10, 5, 18, 0))
	mustAbort(t, s, "A1", "B1", "车辆故障全部收回", tClock(2026, 10, 5, 19, 0))
	mustDispatch(t, s, "B2", "站点D", "张三", []string{"P001"}, tClock(2026, 10, 5, 20, 0))
	mustTransfer(t, s, "T1", "B2", "B3", "李四", "原配送员车辆故障", tClock(2026, 10, 5, 21, 0))
	mustReceiptOK(t, s, "RC2", "B3", "P001", "失败", "收件人不在", tClock(2026, 10, 5, 22, 0))
	mustFreeze(t, s, "E1", "P001", "站点D", "外包装破损", tClock(2026, 10, 5, 23, 0))
	mustUnfreeze(t, s, "U1", "E1", "已核实放行", tClock(2026, 10, 6, 0, 0))

	// P002：接收后冻结、解除，再交接。
	mustFreeze(t, s, "E2", "P002", "站点B", "面单污损", tClock(2026, 10, 5, 13, 0))
	mustUnfreeze(t, s, "U2", "E2", "已换面单", tClock(2026, 10, 5, 14, 0))
	mustHandoff(t, s, "R2", "站点B", "站点C", []string{"P002"}, tClock(2026, 10, 5, 15, 0))

	// 重新载入：合法后续流转形成的当前站点、状态和运输归属必须照常通过校验。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("合法后续流转的台账重新载入应通过校验: %v", err)
	}
	p1, _ := s2.Query("P001")
	if p1.Station != "站点D" || p1.Status != statusInStation {
		t.Fatalf("P001 应在站点D 在站: station=%q status=%q", p1.Station, p1.Status)
	}
	p2, _ := s2.Query("P002")
	if p2.Station != "站点C" || p2.Status != statusInStation {
		t.Fatalf("P002 应在站点C 在站: station=%q status=%q", p2.Station, p2.Status)
	}

	// 发运同内容重放（换序无关）：返回首次成员顺序与发运时间，不追加轨迹、不改写文件。
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	res, replayed, err := s2.Ship("S1", "站点A", "站点B", []string{"P002", "P001"}, tClock(2026, 10, 6, 1, 0))
	if err != nil || !replayed {
		t.Fatalf("发运同内容重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	if !sameOrder(res.Parcels, []string{"P001", "P002"}) || !res.Time.Equal(shipTime) {
		t.Fatalf("重放应返回首次成员顺序与发运时间: %+v", res)
	}
	// 接收同内容重放：返回首次成员顺序与接收时间，不检查现状。
	rres, replayed, err := s2.Receive("RS1", "S1", "站点B", tClock(2026, 10, 6, 1, 0))
	if err != nil || !replayed {
		t.Fatalf("接收同内容重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	if !sameOrder(rres.Parcels, []string{"P001", "P002"}) || !rres.Time.Equal(recvTime) {
		t.Fatalf("重放应返回首次成员顺序与接收时间: %+v", rres)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("重放不应改写数据文件")
	}
	if len(p1.Trail) != 15 {
		t.Fatalf("重放不应追加轨迹，P001 应有 15 条轨迹，得到 %d", len(p1.Trail))
	}

	// 两类编号独立、换内容冲突、接收后运输单号不释放。
	if _, _, err := s2.Ship("S1", "站点A", "站点C", []string{"P001"}, tClock(2026, 10, 6, 2, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("接收后同运输单号换内容仍应报冲突: %v", err)
	}
	if _, _, err := s2.Receive("RS1", "S2", "站点D", tClock(2026, 10, 6, 2, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同接收请求号换运输单应报冲突: %v", err)
	}
}

// 损坏台账上所有入口（查询、修改、历史重放）都拒绝：退出码 1、标准错误说明
// 损坏原因、不崩溃、不报告成功，原文件字节不变且新业务编号不占用；
// 还原有效文件后可用同一新号重试。
func TestCorruptLedgerRejectedEverywhere(t *testing.T) {
	path, raw := shipLedgerBase(t)

	// 篡改：交换 P001 发运与接收轨迹的保存顺序。
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	trail := ledgerParcel(m, "P001")["trail"].([]any)
	trail[1], trail[2] = trail[2], trail[1]
	buf, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询、修改及历史重放入口全部拒绝：退出码 1，标准错误说明损坏原因。
	cases := [][]string{
		{"query", "--id", "P001"},
		{"query", "--id", "P002"}, // 即使当前操作针对另一包裹也拒绝
		{"shipment", "--id", "S1"},
		{"batch", "--id", "B1"},
		{"register", "--id", "P003", "--station", "站点A"},
		{"ship", "--shipment", "S9", "--from", "站点A", "--to", "站点B", "--parcel", "P001"},
		{"receive", "--request", "RS9", "--shipment", "S1", "--station", "站点B"},
		{"ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B", "--parcel", "P001", "--parcel", "P002"}, // 历史重放同样拒绝
		{"receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B"},                              // 历史重放同样拒绝
		{"handoff", "--request", "R3", "--from", "站点B", "--to", "站点C", "--parcel", "P002"},
	}
	for _, args := range cases {
		out, errText, code := runCLI(t, path, args...)
		if code != exitBusiness {
			t.Fatalf("%v：损坏台账应退出 1，得到 code=%d out=%s", args, code, out)
		}
		if !strings.Contains(errText, "数据文件已损坏") {
			t.Fatalf("%v：标准错误应说明损坏原因，得到 %q", args, errText)
		}
		if strings.Contains(out, "成功") {
			t.Fatalf("%v：损坏台账不应报告成功，得到 %q", args, out)
		}
	}

	// 原文件字节不变，新业务编号不占用。
	now, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(now) != string(buf) {
		t.Fatalf("损坏台账的文件字节不应被改写")
	}

	// 还原有效文件后可用同一新号重试。
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, code := runCLI(t, path, "ship", "--shipment", "S9", "--from", "站点B", "--to", "站点C", "--parcel", "P001")
	if code != 0 || !strings.Contains(out, "发运成功") {
		t.Fatalf("还原后同一新运输单号应可重试成功: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, path, "receive", "--request", "RS9", "--shipment", "S9", "--station", "站点C")
	if code != 0 || !strings.Contains(out, "接收成功") {
		t.Fatalf("还原后同一新接收请求号应可重试成功: code=%d out=%s", code, out)
	}
}

// 未接收运输单的成员必须仍在源站站间在途，且只能属于一张未接收运输单。
func TestUnreceivedShipmentMemberState(t *testing.T) {
	s, path := openTempStore(t)
	setupShipBase(t, s)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 未接收但成员状态不是在途。
	mutateLedger(t, path, raw, func(m map[string]any) {
		ledgerParcel(m, "P001")["status"] = "在站"
	})
	// 未接收但成员归属不在源站。
	mutateLedger(t, path, raw, func(m map[string]any) {
		ledgerParcel(m, "P001")["station"] = "站点B"
	})
	// 包裹标记在途却不属于任何未接收运输单。
	mutateLedger(t, path, raw, func(m map[string]any) {
		ledgerShipment(m, "S1")["parcels"] = []any{"P002"}
	})
}

// 正常操作继续整次原子提交：损坏前的有效台账上连续作业，重新载入后
// 状态、轨迹、进度与去重结果完整一致。
func TestValidLedgerAtomicCommitAfterReload(t *testing.T) {
	s, path := openTempStore(t)
	setupShipBase(t, s)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	mustReceive(t, s, "RS1", "S1", "站点B", tClock(2026, 10, 5, 12, 0))
	mustShip(t, s, "S2", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 13, 0))

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新载入失败: %v", err)
	}
	p1, _ := s2.Query("P001")
	if p1.Status != statusInTransit || p1.Station != "站点B" || len(p1.Trail) != 4 {
		t.Fatalf("P001 应在 S2 在途且轨迹完整: %+v", p1)
	}
	if sh := s2.ActiveShipment("P001"); sh == nil || sh.Shipment != "S2" {
		t.Fatalf("P001 当前运输单应为 S2")
	}
	p2, _ := s2.Query("P002")
	if p2.Status != statusInStation || p2.Station != "站点B" || len(p2.Trail) != 3 {
		t.Fatalf("P002 应在站点B 在站: %+v", p2)
	}
	// 在途中的 S2 可正常接收，整次原子提交后重新载入一致。
	mustReceive(t, s2, "RS2", "S2", "站点C", tClock(2026, 10, 5, 14, 0))
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("再次载入失败: %v", err)
	}
	p1, _ = s3.Query("P001")
	if p1.Status != statusInStation || p1.Station != "站点C" || len(p1.Trail) != 5 {
		t.Fatalf("接收后 P001 应在站点C 在站: %+v", p1)
	}
	if _, replayed, err := s3.Receive("RS2", "S2", "站点C", tClock(2026, 10, 5, 15, 0)); err != nil || !replayed {
		t.Fatalf("接收重放应返回首次结果: %v replayed=%v", err, replayed)
	}
}
