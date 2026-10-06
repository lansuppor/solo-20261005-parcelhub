package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// buildReceivedLedger 建立 P001、P002 经运输单 S1 从站点A 发往站点B 并整单接收的
// 有效台账，返回数据文件路径与有效文件字节（供篡改后还原）。
func buildReceivedLedger(t *testing.T) (string, []byte) {
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

// corruptLedger 把 fn 应用到有效台账字节的 JSON 表示上并写回，
// 断言重新载入被明确拒绝（ErrCorrupt，不崩溃、不报告成功）。
func corruptLedger(t *testing.T, path string, raw []byte, name string, fn func(m map[string]any)) {
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
		t.Fatalf("%s：应明确拒绝载入，得到 err=%v", name, err)
	}
}

// restoreLedger 还原有效文件字节并断言可以正常打开。
func restoreLedger(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("还原有效文件后应能正常打开: %v", err)
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

// 空关联：运输单、接收结果及其关联成员为 null 或缺失时必须明确拒绝，不崩溃。
func TestShipReceiveNullAssociationsRejected(t *testing.T) {
	path, raw := buildReceivedLedger(t)

	corruptLedger(t, path, raw, "运输单结果为 null", func(m map[string]any) {
		m["shipments"].(map[string]any)["S1"] = nil
	})
	corruptLedger(t, path, raw, "接收结果为 null", func(m map[string]any) {
		m["receives"].(map[string]any)["RS1"] = nil
	})
	corruptLedger(t, path, raw, "包裹记录为 null", func(m map[string]any) {
		m["parcels"].(map[string]any)["P001"] = nil
	})
	corruptLedger(t, path, raw, "运输单成员为 null", func(m map[string]any) {
		ledgerShipment(m, "S1")["parcels"] = []any{"P001", nil}
	})
	corruptLedger(t, path, raw, "接收成员为 null", func(m map[string]any) {
		ledgerReceive(m, "RS1")["parcels"] = []any{nil, "P002"}
	})
	corruptLedger(t, path, raw, "运输单成员缺失", func(m map[string]any) {
		delete(ledgerShipment(m, "S1"), "parcels")
	})
	corruptLedger(t, path, raw, "接收缺少请求号", func(m map[string]any) {
		ledgerReceive(m, "RS1")["request"] = ""
	})

	restoreLedger(t, path, raw)
}

// 发运与接收轨迹的运输单、两站、发生时间必须与保存结果一致；接收源站必须等于运输单源站。
func TestShipReceiveTrailMismatchRejected(t *testing.T) {
	path, raw := buildReceivedLedger(t)

	// P001 轨迹：0=收件 1=发运 2=接收。
	corruptLedger(t, path, raw, "接收轨迹源站错误", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		trail[2].(map[string]any)["from"] = "站点C"
	})
	corruptLedger(t, path, raw, "接收轨迹目的站错误", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		trail[2].(map[string]any)["to"] = "站点C"
	})
	corruptLedger(t, path, raw, "接收轨迹时间不符", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		trail[2].(map[string]any)["time"] = "2026-10-05T13:00:00Z"
	})
	corruptLedger(t, path, raw, "接收轨迹请求号不符", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		trail[2].(map[string]any)["request"] = "RS404"
	})
	corruptLedger(t, path, raw, "发运轨迹源站错误", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		trail[1].(map[string]any)["from"] = "站点C"
	})
	corruptLedger(t, path, raw, "发运轨迹目的站错误", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		trail[1].(map[string]any)["to"] = "站点C"
	})
	corruptLedger(t, path, raw, "发运轨迹时间不符", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		trail[1].(map[string]any)["time"] = "2026-10-05T11:00:00Z"
	})

	restoreLedger(t, path, raw)
}

// 轨迹缺失或重复：每张运输单的成员恰有一次对应发运，已接收时恰有一次对应接收。
func TestShipReceiveTrailMissingOrDuplicateRejected(t *testing.T) {
	path, raw := buildReceivedLedger(t)

	corruptLedger(t, path, raw, "成员缺少发运轨迹", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		ledgerParcel(m, "P001")["trail"] = append(trail[:1:1], trail[2:]...)
	})
	corruptLedger(t, path, raw, "成员缺少接收轨迹", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		ledgerParcel(m, "P001")["trail"] = trail[:2]
	})
	corruptLedger(t, path, raw, "重复发运轨迹", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		ledgerParcel(m, "P001")["trail"] = append(trail, trail[1])
	})
	corruptLedger(t, path, raw, "重复接收轨迹", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		ledgerParcel(m, "P001")["trail"] = append(trail, trail[2])
	})

	restoreLedger(t, path, raw)
}

// 生命周期按轨迹保存顺序判定：接收轨迹位于发运之前必须拒绝。
func TestReceiveTrailBeforeShipRejected(t *testing.T) {
	path, raw := buildReceivedLedger(t)

	corruptLedger(t, path, raw, "接收轨迹位于发运之前", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		trail[1], trail[2] = trail[2], trail[1]
	})

	restoreLedger(t, path, raw)
}

// 发运后到接收前不得出现其他作业轨迹；未接收时发运必须是最后一条轨迹。
func TestTransitInterleavedOpRejected(t *testing.T) {
	path, raw := buildReceivedLedger(t)

	corruptLedger(t, path, raw, "在途夹入交接轨迹", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		handoff := map[string]any{
			"op": "交接", "station": "站点A", "request": "R9",
			"time": "2026-10-05T11:00:00Z",
		}
		trail = append(trail, nil)
		copy(trail[3:], trail[2:])
		trail[2] = handoff
		ledgerParcel(m, "P001")["trail"] = trail
	})
	corruptLedger(t, path, raw, "在途夹入冻结轨迹", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		freeze := map[string]any{
			"op": "冻结", "station": "站点A", "incident": "E9", "reason": "异常",
			"time": "2026-10-05T11:00:00Z",
		}
		m["freezes"].(map[string]any)["E9"] = map[string]any{
			"incident": "E9", "parcel": "P001", "station": "站点A", "reason": "异常",
			"time": "2026-10-05T11:00:00Z",
		}
		trail = append(trail, nil)
		copy(trail[3:], trail[2:])
		trail[2] = freeze
		ledgerParcel(m, "P001")["trail"] = trail
	})
	restoreLedger(t, path, raw)

	// 未接收的运输单：发运必须是最后一条轨迹，成员仍在源站站间在途。
	s, path2 := openTempStore(t)
	setupShipBase(t, s)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	raw2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	corruptLedger(t, path2, raw2, "未接收运输单发运后出现其他轨迹", func(m map[string]any) {
		trail := ledgerParcel(m, "P001")["trail"].([]any)
		ledgerParcel(m, "P001")["trail"] = append(trail, map[string]any{
			"op": "交接", "station": "站点C", "request": "R9",
			"time": "2026-10-05T11:00:00Z",
		})
	})
	corruptLedger(t, path2, raw2, "未接收运输单成员状态矛盾", func(m map[string]any) {
		ledgerParcel(m, "P001")["status"] = "在站"
	})
	corruptLedger(t, path2, raw2, "未接收运输单成员归属矛盾", func(m map[string]any) {
		ledgerParcel(m, "P001")["station"] = "站点B"
	})
	restoreLedger(t, path2, raw2)
}

// 接收后没有后续作业时成员必须在目的站在站；后续流转后的当前站点与状态
// 必须由轨迹记录形成，无对应记录的站点或状态改变必须拒绝。
func TestPostReceiveStateContradictionRejected(t *testing.T) {
	path, raw := buildReceivedLedger(t)

	corruptLedger(t, path, raw, "接收后状态矛盾", func(m map[string]any) {
		ledgerParcel(m, "P001")["status"] = "站间在途"
	})
	corruptLedger(t, path, raw, "接收后归属矛盾", func(m map[string]any) {
		ledgerParcel(m, "P001")["station"] = "站点C"
	})
	corruptLedger(t, path, raw, "接收后冻结状态无记录", func(m map[string]any) {
		ledgerParcel(m, "P001")["status"] = "异常冻结"
	})
	restoreLedger(t, path, raw)

	// 合法后续流转（交接）之后：篡改当前站点而无对应轨迹同样拒绝。
	s, path2 := openTempStore(t)
	setupShipBase(t, s)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	mustReceive(t, s, "RS1", "S1", "站点B", tClock(2026, 10, 5, 12, 0))
	mustHandoff(t, s, "R2", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 13, 0))
	raw2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	corruptLedger(t, path2, raw2, "交接后无记录的站点改变", func(m map[string]any) {
		ledgerParcel(m, "P001")["station"] = "站点D"
	})
	corruptLedger(t, path2, raw2, "交接后无记录的状态改变", func(m map[string]any) {
		ledgerParcel(m, "P001")["status"] = "配送中"
	})
	restoreLedger(t, path2, raw2)
}

// 合法后续流转（交接、再次发运与接收、配送回执及其撤销、中止、续接、冻结与解除）
// 重新载入后仍应受理，并按这些记录形成的当前站点、状态和运输归属校验；
// 历史接收不把成员固定在目的站；发运、接收同内容重放仍返回首次结果且不改写文件。
func TestPostReceiveFlowsAndReplaySurviveReload(t *testing.T) {
	s, path := openTempStore(t)
	setupShipBase(t, s)
	mustRegister(t, s, "P003", "站点B", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s, "P004", "站点B", tClock(2026, 10, 5, 9, 0))

	shipTime := tClock(2026, 10, 5, 10, 0)
	recvTime := tClock(2026, 10, 5, 12, 0)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002"}, shipTime)
	mustReceive(t, s, "RS1", "S1", "站点B", recvTime)

	// P001：交接 B->C，再次发运 C->D 并接收，最终归站点D 在站。
	mustHandoff(t, s, "R2", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 13, 0))
	mustShip(t, s, "S2", "站点C", "站点D", []string{"P001"}, tClock(2026, 10, 5, 14, 0))
	mustReceive(t, s, "RS2", "S2", "站点D", tClock(2026, 10, 5, 15, 0))

	// P002：出站 -> 失败回执 -> 撤销 -> 签收。
	mustDispatch(t, s, "B1", "站点B", "张三", []string{"P002"}, tClock(2026, 10, 5, 13, 0))
	mustReceiptOK(t, s, "RC1", "B1", "P002", "失败", "收件人不在", tClock(2026, 10, 5, 14, 0))
	mustRevoke(t, s, "RV1", "RC1", "误录失败，实际仍配送中", tClock(2026, 10, 5, 15, 0))
	mustReceiptOK(t, s, "RC2", "B1", "P002", "签收", "", tClock(2026, 10, 5, 16, 0))

	// P003：出站 -> 中止收回 -> 冻结 -> 解除。
	mustDispatch(t, s, "B2", "站点B", "张三", []string{"P003"}, tClock(2026, 10, 5, 13, 0))
	mustAbort(t, s, "A1", "B2", "车辆故障全部收回", tClock(2026, 10, 5, 14, 0))
	mustFreeze(t, s, "E1", "P003", "站点B", "外包装破损", tClock(2026, 10, 5, 15, 0))
	if _, _, err := s.Unfreeze("U1", "E1", "已核实放行", tClock(2026, 10, 5, 16, 0)); err != nil {
		t.Fatalf("Unfreeze 意外失败: %v", err)
	}

	// P004：出站 -> 续接给另一配送员。
	mustDispatch(t, s, "B3", "站点B", "张三", []string{"P004"}, tClock(2026, 10, 5, 13, 0))
	mustTransfer(t, s, "T1", "B3", "B4", "李四", "原配送员车辆故障", tClock(2026, 10, 5, 14, 0))

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 重新载入：合法后续流转形成的当前站点、状态与运输归属必须被接受。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新载入合法台账失败: %v", err)
	}
	p1, _ := s2.Query("P001")
	if p1.Station != "站点D" || p1.Status != statusInStation {
		t.Fatalf("P001 应归站点D 在站（历史接收不固定在目的站）: %+v", p1)
	}
	p2, _ := s2.Query("P002")
	if p2.Station != "站点B" || p2.Status != statusSigned {
		t.Fatalf("P002 应在站点B 已签收: %+v", p2)
	}
	p3, _ := s2.Query("P003")
	if p3.Station != "站点B" || p3.Status != statusInStation {
		t.Fatalf("P003 应在站点B 在站: %+v", p3)
	}
	p4, _ := s2.Query("P004")
	if p4.Station != "站点B" || p4.Status != statusDelivering || currentBatch(p4) != "B4" {
		t.Fatalf("P004 应在批次 B4 配送中: %+v", p4)
	}

	// 发运、接收同内容重放：返回首次成员顺序与时间，不重新检查首次受理条件。
	res1, replayed, err := s2.Ship("S1", "站点A", "站点B", []string{"P002", "P001"}, tClock(2026, 10, 6, 9, 0))
	if err != nil || !replayed || !sameOrder(res1.Parcels, []string{"P001", "P002"}) || !res1.Time.Equal(shipTime) {
		t.Fatalf("发运重放应返回首次成员顺序与时间: %+v replayed=%v err=%v", res1, replayed, err)
	}
	res2, replayed, err := s2.Receive("RS1", "S1", "站点B", nil, tClock(2026, 10, 6, 9, 0))
	if err != nil || !replayed || !sameOrder(res2.Parcels, []string{"P001", "P002"}) || !res2.Time.Equal(recvTime) {
		t.Fatalf("接收重放应返回首次成员与时间: %+v replayed=%v err=%v", res2, replayed, err)
	}
	if _, replayed, err := s2.Ship("S2", "站点C", "站点D", []string{"P001"}, tClock(2026, 10, 6, 9, 0)); err != nil || !replayed {
		t.Fatalf("再次发运的重放应返回首次结果: replayed=%v err=%v", replayed, err)
	}
	if _, replayed, err := s2.Receive("RS2", "S2", "站点D", nil, tClock(2026, 10, 6, 9, 0)); err != nil || !replayed {
		t.Fatalf("再次接收的重放应返回首次结果: replayed=%v err=%v", replayed, err)
	}
	// 接收后运输单号不释放：换内容仍冲突。
	if _, _, err := s2.Ship("S1", "站点A", "站点C", []string{"P001"}, tClock(2026, 10, 6, 9, 0)); err == nil {
		t.Fatalf("接收后同号换内容仍应报冲突")
	}

	// 重放不追加轨迹、不改写文件。
	p1After, _ := s2.Query("P001")
	if len(p1After.Trail) != len(p1.Trail) {
		t.Fatalf("重放不应追加轨迹: %d -> %d", len(p1.Trail), len(p1After.Trail))
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("重放不应改写数据文件")
	}

	// 重新载入后仍可继续受理新业务：P001 从站点D 再次发运。
	if _, _, err := s2.Ship("S3", "站点D", "站点E", []string{"P001"}, tClock(2026, 10, 6, 10, 0)); err != nil {
		t.Fatalf("重新载入后继续发运失败: %v", err)
	}
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("再次载入失败: %v", err)
	}
	p1Final, _ := s3.Query("P001")
	if p1Final.Status != statusInTransit || p1Final.Station != "站点D" {
		t.Fatalf("P001 应在 S3 在途、归属暂记站点D: %+v", p1Final)
	}
	if sh := s3.ActiveShipment("P001"); sh == nil || sh.Shipment != "S3" {
		t.Fatalf("P001 当前运输单应为 S3")
	}
}

// 运输矛盾必须拒绝所有入口（查询、修改、历史重放），即使当前操作针对另一包裹：
// 标准错误说明损坏原因、退出码 1、不崩溃、不报告成功、不改写文件、新编号不占用；
// 还原有效文件后可用同一新号重试。
func TestCorruptShipmentLedgerBlocksAllEntries(t *testing.T) {
	s, path := openTempStore(t)
	setupShipBase(t, s)
	mustRegister(t, s, "P005", "站点A", tClock(2026, 10, 5, 9, 0)) // 与运输无关的包裹
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	mustReceive(t, s, "RS1", "S1", "站点B", tClock(2026, 10, 5, 12, 0))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 写入损坏台账：接收轨迹位于发运之前。
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	trail := ledgerParcel(m, "P001")["trail"].([]any)
	trail[1], trail[2] = trail[2], trail[1]
	corrupt, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	entries := [][]string{
		{"query", "--id", "P005"},                        // 查询：针对另一包裹也必须拒绝
		{"query", "--id", "P001"},                        // 查询：当事包裹
		{"shipment", "--id", "S1"},                       // 运输单查询
		{"register", "--id", "P006", "--station", "站点A"}, // 修改
		{"ship", "--shipment", "S9", "--from", "站点A", "--to", "站点B", "--parcel", "P005"},                     // 修改：新运输单号
		{"receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B"},                              // 历史重放
		{"ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B", "--parcel", "P001", "--parcel", "P002"}, // 历史重放
	}
	for _, args := range entries {
		stdout, stderr, code := runCLI(t, path, args...)
		if code != exitBusiness {
			t.Fatalf("%v 应以状态码 1 拒绝，得到 code=%d out=%s", args, code, stdout)
		}
		if !strings.Contains(stderr, "数据文件已损坏") {
			t.Fatalf("%v 标准错误应说明损坏原因，得到: %s", args, stderr)
		}
		if strings.Contains(stdout, "成功") {
			t.Fatalf("%v 不得报告成功: %s", args, stdout)
		}
	}

	// 不自动修补或覆盖台账：原文件字节不变，新业务编号不占用。
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(corrupt) {
		t.Fatalf("损坏文件不得被改写")
	}

	// 还原有效文件后可用同一新号重试。
	restoreLedger(t, path, raw)
	out, _, code := runCLI(t, path, "ship", "--shipment", "S9", "--from", "站点A", "--to", "站点B", "--parcel", "P005")
	if code != 0 || !strings.Contains(out, "发运成功") {
		t.Fatalf("还原后同一新号重试应成功: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, path, "register", "--id", "P006", "--station", "站点A")
	if code != 0 || !strings.Contains(out, "收件登记成功") {
		t.Fatalf("还原后登记应成功: code=%d out=%s", code, out)
	}
}

// 有效台账上的发运、接收重放不重新检查首次受理条件：即使包裹已不满足
// 首次发运条件（在源站在站），同内容重放仍返回首次结果。
func TestShipReceiveReplayDoesNotRecheckAcceptance(t *testing.T) {
	s, _ := openTempStore(t)
	setupShipBase(t, s)
	shipTime := tClock(2026, 10, 5, 10, 0)
	mustShip(t, s, "S1", "站点A", "站点B", []string{"P001"}, shipTime)
	recvTime := tClock(2026, 10, 5, 12, 0)
	mustReceive(t, s, "RS1", "S1", "站点B", recvTime)
	// 接收后包裹离开目的站，不再满足“在源站在站”的首次发运条件。
	mustHandoff(t, s, "R2", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 13, 0))

	res, replayed, err := s.Ship("S1", "站点A", "站点B", []string{"P001"}, time.Now())
	if err != nil || !replayed || !res.Time.Equal(shipTime) {
		t.Fatalf("发运重放不应重新检查首次受理条件: %+v replayed=%v err=%v", res, replayed, err)
	}
	rres, replayed, err := s.Receive("RS1", "S1", "站点B", nil, time.Now())
	if err != nil || !replayed || !rres.Time.Equal(recvTime) {
		t.Fatalf("接收重放不应重新检查首次受理条件: %+v replayed=%v err=%v", rres, replayed, err)
	}
	p, _ := s.Query("P001")
	if p.Station != "站点C" || len(p.Trail) != 4 {
		t.Fatalf("重放不应移动包裹或追加轨迹: %+v", p)
	}
}
