package main

import (
	"strings"
	"testing"
)

// 端到端：分批接收的两种调用方式、运输单进度展示、query 的运输归属展示、
// 重放与冲突的退出码。
func TestCLIEndToEndPartialReceive(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"

	for _, id := range []string{"P001", "P002", "P003"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败: code=%d", id, code)
		}
	}
	if _, _, code := runCLI(t, dbPath, "ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002", "--parcel", "P003"); code != 0 {
		t.Fatalf("发运失败: code=%d", code)
	}

	// 显式集合分批接收：只接 P001。
	out, _, code := runCLI(t, dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B",
		"--parcel", "P001")
	if code != 0 || !strings.Contains(out, "接收成功") ||
		!strings.Contains(out, "接收请求号: RS1") || !strings.Contains(out, "本次接收包裹（1 件）") ||
		!strings.Contains(out, "P001") || strings.Contains(out, "P002") {
		t.Fatalf("显式集合接收输出不符: code=%d out=%s", code, out)
	}

	// 运输单查询：部分接收、进度与逐件接收信息。
	out, _, code = runCLI(t, dbPath, "shipment", "--id", "S1")
	if code != 0 || !strings.Contains(out, "运输单状态: 部分接收") ||
		!strings.Contains(out, "接收进度: 1/3 已接收") ||
		!strings.Contains(out, "P001    已接收    接收请求号: RS1") ||
		!strings.Contains(out, "P002    未接收（站间在途）") ||
		!strings.Contains(out, "P003    未接收（站间在途）") ||
		!strings.Contains(out, "发运时间:") {
		t.Fatalf("部分接收的运输单查询输出不符: code=%d out=%s", code, out)
	}

	// query：已收件展示在站与接收轨迹、不显示当前运输单；余件仍显示运输单与目的站。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前状态: 在站") ||
		!strings.Contains(out, "当前站点: 站点B") || strings.Contains(out, "当前运输单") ||
		!strings.Contains(out, "操作: 接收") || !strings.Contains(out, "接收请求号: RS1") {
		t.Fatalf("已收件 query 输出不符: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, dbPath, "query", "--id", "P002")
	if code != 0 || !strings.Contains(out, "当前状态: 站间在途") ||
		!strings.Contains(out, "当前运输单: S1") || !strings.Contains(out, "目的站: 站点B") {
		t.Fatalf("余件 query 输出不符: code=%d out=%s", code, out)
	}

	// 已收件后续作业（交接）不妨碍余件接收。
	if _, _, code := runCLI(t, dbPath, "handoff", "--request", "R1", "--from", "站点B", "--to", "站点C",
		"--parcel", "P001"); code != 0 {
		t.Fatalf("已收件应能继续交接: code=%d", code)
	}

	// 不选成员方式：接收全部尚未接收件（P002、P003），运输单全部接收。
	out, _, code = runCLI(t, dbPath, "receive", "--request", "RS2", "--shipment", "S1", "--station", "站点B")
	if code != 0 || !strings.Contains(out, "本次接收包裹（2 件）") ||
		!strings.Contains(out, "P002") || !strings.Contains(out, "P003") {
		t.Fatalf("不选成员接收输出不符: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, dbPath, "shipment", "--id", "S1")
	if code != 0 || !strings.Contains(out, "运输单状态: 已接收") ||
		!strings.Contains(out, "接收进度: 3/3 已接收") ||
		!strings.Contains(out, "接收请求号: RS2") || !strings.Contains(out, "接收站点: 站点B") ||
		!strings.Contains(out, "接收时间:") {
		t.Fatalf("全部接收的运输单查询输出不符: code=%d out=%s", code, out)
	}

	// 重放：显式集合换序与不选成员均返回首次结果；同号切换方式冲突；全部接收后新请求拒绝。
	out, _, code = runCLI(t, dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B",
		"--parcel", "P001")
	if code != 0 || !strings.Contains(out, "重复提交") {
		t.Fatalf("显式集合重放应返回首次结果: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, dbPath, "receive", "--request", "RS2", "--shipment", "S1", "--station", "站点B")
	if code != 0 || !strings.Contains(out, "重复提交") || !strings.Contains(out, "本次接收包裹（2 件）") {
		t.Fatalf("不选成员重放应返回首次集合: code=%d out=%s", code, out)
	}
	_, errText, code := runCLI(t, dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("同号切换方式应报冲突(1): code=%d err=%s", code, errText)
	}
	if _, _, code := runCLI(t, dbPath, "receive", "--request", "RS9", "--shipment", "S1", "--station", "站点B"); code != exitBusiness {
		t.Fatalf("全部接收后新请求应拒绝(1): code=%d", code)
	}

	// 显式集合参数校验：空集合元素、集合内重复、选中件不属于原单，均退出码 1。
	if _, _, code := runCLI(t, dbPath, "receive", "--request", "RQ1", "--shipment", "S1", "--station", "站点B",
		"--parcel", "  "); code != exitBusiness {
		t.Fatalf("空白包裹编号应失败(1): code=%d", code)
	}
	if _, _, code := runCLI(t, dbPath, "receive", "--request", "RQ1", "--shipment", "S1", "--station", "站点B",
		"--parcel", "P001", "--parcel", "P001"); code != exitBusiness {
		t.Fatalf("集合内重复应失败(1): code=%d", code)
	}
}
