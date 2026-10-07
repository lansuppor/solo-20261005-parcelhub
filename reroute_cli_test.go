package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// 改址 CLI 全流程：发运 -> 部分接收 -> 改址 -> 查询展示 -> 新目的站接收 -> 重放与冲突。
func TestCLIRerouteFlow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	for _, args := range [][]string{
		{"register", "--id", "P001", "--station", "站点A"},
		{"register", "--id", "P002", "--station", "站点A"},
		{"register", "--id", "P003", "--station", "站点A"},
		{"ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B",
			"--parcel", "P001", "--parcel", "P002", "--parcel", "P003"},
		{"receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B", "--parcel", "P001"},
	} {
		if _, errText, code := runCLI(t, dbPath, args...); code != 0 {
			t.Fatalf("准备台账失败: %v code=%d err=%s", args, code, errText)
		}
	}

	// 改址：输出请求号、运输单、前后目的站、原因、集合与时间。
	out, _, code := runCLI(t, dbPath, "reroute", "--request", "RR1", "--shipment", "S1",
		"--expect", "站点B", "--to", "站点C", "--reason", "站点B 暂停收货")
	if code != 0 || !strings.Contains(out, "改址成功") ||
		!strings.Contains(out, "改址请求号: RR1") || !strings.Contains(out, "运输单号: S1") ||
		!strings.Contains(out, "原目的站: 站点B") || !strings.Contains(out, "新目的站: 站点C") ||
		!strings.Contains(out, "改址原因: 站点B 暂停收货") ||
		!strings.Contains(out, "本次改址（2 件）") || !strings.Contains(out, "P002") ||
		!strings.Contains(out, "P003") || strings.Contains(out, "P001") ||
		!strings.Contains(out, "发生时间:") {
		t.Fatalf("改址输出不符: code=%d out=%s", code, out)
	}

	// query：在途件展示当前有效目的站与原目的站，轨迹含改址记录。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P002")
	if code != 0 || !strings.Contains(out, "当前状态: 站间在途") ||
		!strings.Contains(out, "当前站点: 站点A") ||
		!strings.Contains(out, "当前运输单: S1") ||
		!strings.Contains(out, "当前有效目的站: 站点C") ||
		!strings.Contains(out, "原目的站: 站点B") ||
		!strings.Contains(out, "操作: 改址") ||
		!strings.Contains(out, "改址请求号: RR1") ||
		!strings.Contains(out, "原因: 站点B 暂停收货") {
		t.Fatalf("query 应展示有效目的站与改址轨迹: code=%d out=%s", code, out)
	}
	// 已收件不展示改址轨迹。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || strings.Contains(out, "操作: 改址") || strings.Contains(out, "当前运输单") {
		t.Fatalf("已收件不应展示改址轨迹或在途信息: code=%d out=%s", code, out)
	}

	// shipment：区分原目的站与当前有效目的站，保留接收进度，逐件显示实际接收站，
	// 按提交顺序列出改址记录及对应集合。
	out, _, code = runCLI(t, dbPath, "shipment", "--id", "S1")
	if code != 0 || !strings.Contains(out, "目的站点: 站点B") ||
		!strings.Contains(out, "当前有效目的站: 站点C") ||
		!strings.Contains(out, "运输单状态: 部分接收") ||
		!strings.Contains(out, "接收进度: 1/3") ||
		!strings.Contains(out, "P001    已接收    接收请求号: RS1") ||
		!strings.Contains(out, "接收站点: 站点B") ||
		!strings.Contains(out, "P002    待接收（站间在途）") ||
		!strings.Contains(out, "改址记录（1 次，按提交顺序）") ||
		!strings.Contains(out, "改址请求号: RR1") ||
		!strings.Contains(out, "原目的站: 站点B") ||
		!strings.Contains(out, "新目的站: 站点C") {
		t.Fatalf("shipment 应区分原目的站与有效目的站并列出改址记录: code=%d out=%s", code, out)
	}

	// 旧目的站接收整次拒绝；新目的站接收成功。
	if _, _, code := runCLI(t, dbPath, "receive", "--request", "RS2", "--shipment", "S1", "--station", "站点B"); code != exitBusiness {
		t.Fatalf("改址后按旧目的站接收应拒绝(1): code=%d", code)
	}
	out, _, code = runCLI(t, dbPath, "receive", "--request", "RS2", "--shipment", "S1", "--station", "站点C")
	if code != 0 || !strings.Contains(out, "接收站点: 站点C") ||
		!strings.Contains(out, "P002") || !strings.Contains(out, "P003") {
		t.Fatalf("应按新目的站接收余件: code=%d out=%s", code, out)
	}

	// 全部接收后拒绝新改址。
	if _, _, code := runCLI(t, dbPath, "reroute", "--request", "RR2", "--shipment", "S1",
		"--expect", "站点C", "--to", "站点D", "--reason", "再次改址"); code != exitBusiness {
		t.Fatalf("全部接收后改址应拒绝(1): code=%d", code)
	}

	// 同号同内容重放：返回首次改址信息、集合与时间（全部接收后仍成立）。
	out, _, code = runCLI(t, dbPath, "reroute", "--request", "RR1", "--shipment", "S1",
		"--expect", "站点B", "--to", "站点C", "--reason", "站点B 暂停收货")
	if code != 0 || !strings.Contains(out, "重复提交") ||
		!strings.Contains(out, "本次改址（2 件）") || !strings.Contains(out, "新目的站: 站点C") {
		t.Fatalf("改址重放应返回首次结果: code=%d out=%s", code, out)
	}
	// 同号换内容冲突。
	_, errText, code := runCLI(t, dbPath, "reroute", "--request", "RR1", "--shipment", "S1",
		"--expect", "站点B", "--to", "站点D", "--reason", "站点B 暂停收货")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("改址换内容应报冲突(1): code=%d err=%s", code, errText)
	}
	// 改址前的接收重放仍返回原站点、集合和时间。
	out, _, code = runCLI(t, dbPath, "receive", "--request", "RS1", "--shipment", "S1", "--station", "站点B", "--parcel", "P001")
	if code != 0 || !strings.Contains(out, "重复提交") || !strings.Contains(out, "接收站点: 站点B") {
		t.Fatalf("改址前接收重放应返回原站点: code=%d out=%s", code, out)
	}
	// ship 重放仍返回最初两站。
	out, _, code = runCLI(t, dbPath, "ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002", "--parcel", "P003")
	if code != 0 || !strings.Contains(out, "重复提交") ||
		!strings.Contains(out, "源站点: 站点A") || !strings.Contains(out, "目的站点: 站点B") {
		t.Fatalf("ship 重放应返回最初两站: code=%d out=%s", code, out)
	}
}

// 改址 CLI 输入校验：空白输入、预期不符、新目的站等于当前或源站均退出 1；未知参数退出 2。
func TestCLIRerouteValidation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	for _, args := range [][]string{
		{"register", "--id", "P001", "--station", "站点A"},
		{"ship", "--shipment", "S1", "--from", "站点A", "--to", "站点B", "--parcel", "P001"},
	} {
		if _, errText, code := runCLI(t, dbPath, args...); code != 0 {
			t.Fatalf("准备台账失败: %v code=%d err=%s", args, code, errText)
		}
	}

	// 各字段清理两端空白后不可为空。
	for _, args := range [][]string{
		{"reroute", "--request", "  ", "--shipment", "S1", "--expect", "站点B", "--to", "站点C", "--reason", "原因"},
		{"reroute", "--request", "RR1", "--shipment", " ", "--expect", "站点B", "--to", "站点C", "--reason", "原因"},
		{"reroute", "--request", "RR1", "--shipment", "S1", "--expect", " ", "--to", "站点C", "--reason", "原因"},
		{"reroute", "--request", "RR1", "--shipment", "S1", "--expect", "站点B", "--to", " ", "--reason", "原因"},
		{"reroute", "--request", "RR1", "--shipment", "S1", "--expect", "站点B", "--to", "站点C", "--reason", "  "},
	} {
		if _, _, code := runCLI(t, dbPath, args...); code != exitBusiness {
			t.Fatalf("空白输入应以 1 退出: %v code=%d", args, code)
		}
	}
	// 两端空白清理后受理成功。
	if out, _, code := runCLI(t, dbPath, "reroute", "--request", " RR1 ", "--shipment", " S1 ",
		"--expect", " 站点B ", "--to", " 站点C ", "--reason", " 站点B 暂停收货 "); code != 0 ||
		!strings.Contains(out, "改址请求号: RR1") {
		t.Fatalf("清理两端空白后应受理成功: code=%d out=%s", code, out)
	}
	// 业务失败：预期不符、新目的站等于当前、新目的站等于源站、运输单不存在。
	for _, args := range [][]string{
		{"reroute", "--request", "RR2", "--shipment", "S1", "--expect", "站点B", "--to", "站点D", "--reason", "原因"},
		{"reroute", "--request", "RR3", "--shipment", "S1", "--expect", "站点C", "--to", "站点C", "--reason", "原因"},
		{"reroute", "--request", "RR4", "--shipment", "S1", "--expect", "站点C", "--to", "站点A", "--reason", "原因"},
		{"reroute", "--request", "RR5", "--shipment", "S9", "--expect", "站点C", "--to", "站点D", "--reason", "原因"},
	} {
		if _, _, code := runCLI(t, dbPath, args...); code != exitBusiness {
			t.Fatalf("业务校验失败应以 1 退出: %v code=%d", args, code)
		}
	}
	// 失败的首次改址不占号：修正后可成功。
	if _, _, code := runCLI(t, dbPath, "reroute", "--request", "RR2", "--shipment", "S1",
		"--expect", "站点C", "--to", "站点D", "--reason", "修正"); code != 0 {
		t.Fatalf("失败不占号，修正后应成功: code=%d", code)
	}
	// 未知参数退出 2；--help 退出 0。
	if _, _, code := runCLI(t, dbPath, "reroute", "--bogus", "x"); code != exitUsage {
		t.Fatalf("未知参数应以 2 退出: code=%d", code)
	}
	if _, _, code := runCLI(t, dbPath, "reroute", "--help"); code != 0 {
		t.Fatalf("--help 应以 0 退出: code=%d", code)
	}
}
