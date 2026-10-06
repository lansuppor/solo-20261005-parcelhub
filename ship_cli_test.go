package main

import (
	"strings"
	"testing"
)

func TestCLIEndToEndShipReceive(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"

	for _, id := range []string{"P001", "P002"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败: code=%d", id, code)
		}
	}

	// 站间发运：整单转为站间在途，归属站点暂记源站。
	out, _, code := runCLI(t, dbPath, "ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002")
	if code != 0 || !strings.Contains(out, "发运成功") ||
		!strings.Contains(out, "运输单号: S1") || !strings.Contains(out, "源站点: 站点A") ||
		!strings.Contains(out, "目的站点: 站点B") || !strings.Contains(out, "P001") ||
		!strings.Contains(out, "P002") || !strings.Contains(out, "发生时间:") {
		t.Fatalf("发运输出不符: code=%d out=%s", code, out)
	}

	// 同号同两站同集合（换序）重放：返回首次成员顺序与发运时间，不追加轨迹。
	out, _, code = runCLI(t, dbPath, "ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P002", "--parcel", "P001")
	if code != 0 || !strings.Contains(out, "重复提交") {
		t.Fatalf("发运重放应返回首次结果: code=%d out=%s", code, out)
	}
	// 换内容冲突，退出码 1。
	_, errText, code := runCLI(t, dbPath, "ship", "--shipment", "S1", "--from", "站点A", "--to", "站点C",
		"--parcel", "P001", "--parcel", "P002")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("换内容应报冲突(1): code=%d err=%s", code, errText)
	}
	// 空白输入与相同两站校验失败，退出码 1。
	if _, _, code := runCLI(t, dbPath, "ship", "--shipment", "  ", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001"); code != exitBusiness {
		t.Fatalf("空白运输单号应失败(1): code=%d", code)
	}
	if _, _, code := runCLI(t, dbPath, "ship", "--shipment", "S9", "--from", "站点A", "--to", "站点A",
		"--parcel", "P001"); code != exitBusiness {
		t.Fatalf("两站相同应失败(1): code=%d", code)
	}

	// query 展示在途包裹的运输单、目的站与发运轨迹。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前状态: 站间在途") ||
		!strings.Contains(out, "当前站点: 站点A") ||
		!strings.Contains(out, "当前运输单: S1") || !strings.Contains(out, "目的站: 站点B") ||
		!strings.Contains(out, "操作: 发运") || !strings.Contains(out, "运输单号: S1") {
		t.Fatalf("query 应展示在途信息与发运轨迹: code=%d out=%s", code, out)
	}

	// 在途件不能首次交接、出站或冻结，退出码 1。
	if _, _, code := runCLI(t, dbPath, "handoff", "--request", "R1", "--from", "站点A", "--to", "站点C",
		"--parcel", "P001"); code != exitBusiness {
		t.Fatalf("在途件交接应失败(1): code=%d", code)
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A", "--courier", "张三",
		"--parcel", "P001"); code != exitBusiness {
		t.Fatalf("在途件出站应失败(1): code=%d", code)
	}
	if _, _, code := runCLI(t, dbPath, "freeze", "--incident", "E1", "--parcel", "P001", "--station", "站点A",
		"--reason", "异常"); code != exitBusiness {
		t.Fatalf("在途件冻结应失败(1): code=%d", code)
	}

	// 按运输单查询：待接收状态、两站、原成员、发运时间。
	out, _, code = runCLI(t, dbPath, "shipment", "--id", "S1")
	if code != 0 || !strings.Contains(out, "运输单号: S1") ||
		!strings.Contains(out, "源站点: 站点A") || !strings.Contains(out, "目的站点: 站点B") ||
		!strings.Contains(out, "运输单状态: 待接收") || !strings.Contains(out, "发运时间:") ||
		!strings.Contains(out, "P001") || !strings.Contains(out, "P002") {
		t.Fatalf("运输单查询输出不符: code=%d out=%s", code, out)
	}
	// 不存在的运输单报错，退出码 1。
	if _, _, code := runCLI(t, dbPath, "shipment", "--id", "S404"); code != exitBusiness {
		t.Fatalf("查询不存在的运输单应失败(1): code=%d", code)
	}

	// 接收站点不是目的站：整单拒绝，退出码 1。
	if _, _, code := runCLI(t, dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点C"); code != exitBusiness {
		t.Fatalf("接收站点不符应失败(1): code=%d", code)
	}

	// 整单到站接收：全部成员改归目的站、恢复在站。
	out, _, code = runCLI(t, dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B")
	if code != 0 || !strings.Contains(out, "接收成功") ||
		!strings.Contains(out, "接收请求号: RS1") || !strings.Contains(out, "运输单号: S1") ||
		!strings.Contains(out, "接收站点: 站点B") || !strings.Contains(out, "P001") ||
		!strings.Contains(out, "P002") || !strings.Contains(out, "发生时间:") {
		t.Fatalf("接收输出不符: code=%d out=%s", code, out)
	}

	// 同号同运输单同接收站重放：返回首次接收结果与时间。
	out, _, code = runCLI(t, dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B")
	if code != 0 || !strings.Contains(out, "重复提交") {
		t.Fatalf("接收重放应返回首次结果: code=%d out=%s", code, out)
	}
	// 换内容冲突；换请求号再次接收同单拒绝。
	_, errText, code = runCLI(t, dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点C")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("接收换内容应报冲突(1): code=%d err=%s", code, errText)
	}
	if _, _, code := runCLI(t, dbPath, "receive", "--request", "RS2", "--shipment", "S1", "--station", "站点B"); code != exitBusiness {
		t.Fatalf("换请求号再次接收同单应拒绝(1): code=%d", code)
	}

	// 接收后 query 展示在站状态与接收轨迹。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前状态: 在站") ||
		!strings.Contains(out, "当前站点: 站点B") || strings.Contains(out, "当前运输单") ||
		!strings.Contains(out, "操作: 接收") || !strings.Contains(out, "接收请求号: RS1") {
		t.Fatalf("接收后 query 输出不符: code=%d out=%s", code, out)
	}

	// 按运输单查询：已接收状态、接收请求与时间。
	out, _, code = runCLI(t, dbPath, "shipment", "--id", "S1")
	if code != 0 || !strings.Contains(out, "运输单状态: 已接收") ||
		!strings.Contains(out, "接收请求号: RS1") || !strings.Contains(out, "接收站点: 站点B") ||
		!strings.Contains(out, "接收时间:") {
		t.Fatalf("运输单查询应展示已接收信息: code=%d out=%s", code, out)
	}

	// 接收后可继续在站作业：交接去站点C。
	if _, _, code := runCLI(t, dbPath, "handoff", "--request", "R2", "--from", "站点B", "--to", "站点C",
		"--parcel", "P001"); code != 0 {
		t.Fatalf("接收后应能继续在站作业: code=%d", code)
	}
}
