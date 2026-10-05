package main

import (
	"bytes"
	"strings"
	"testing"
)

func runCLI(t *testing.T, dbPath string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	full := append([]string{"--data", dbPath}, args...)
	code = run(full, &out, &errb)
	return out.String(), errb.String(), code
}

func TestHelpVariants(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"-h"},
		{"--help"},
		{"help"},
		{"register", "--help"},
		{"handoff", "-h"},
		{"return", "--help"},
	} {
		var out, errb bytes.Buffer
		code := run(args, &out, &errb)
		if code != 0 {
			t.Fatalf("%v 应以 0 退出，得到 %d", args, code)
		}
		if !strings.Contains(out.String(), appName) {
			t.Fatalf("%v 的帮助应包含应用名 %q:\n%s", args, appName, out.String())
		}
		if errb.Len() != 0 {
			t.Fatalf("%v 帮助不应写入 stderr: %s", args, errb.String())
		}
	}
}

func TestUnknownCommandAndArgsExit2(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	cases := [][]string{
		{"frobnicate"},
		{"--bogus"},
		{"register", "--id", "P1", "--station", "A", "extra"},
		{"register", "--unknown-flag", "x"},
		{"query", "--id"}, // 缺参数值
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		full := append([]string{"--data", dbPath}, c...)
		code := run(full, &out, &errb)
		if code != exitUsage {
			t.Fatalf("%v 应以状态码 2 退出，得到 %d (out=%s err=%s)", c, code, out.String(), errb.String())
		}
		if errb.Len() == 0 {
			t.Fatalf("%v 应在标准错误提示", c)
		}
	}
}

func TestCLIEndToEndRegisterQueryHandoff(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"

	// 登记两件包裹。
	out, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A")
	if code != 0 || !strings.Contains(out, "收件登记成功") || !strings.Contains(out, "在站") {
		t.Fatalf("登记 P001 失败: code=%d out=%s", code, out)
	}
	if _, _, code := runCLI(t, dbPath, "register", "--id", "P002", "--station", "站点A"); code != 0 {
		t.Fatalf("登记 P002 失败: code=%d", code)
	}

	// 重复登记：退出码 1，原记录不变。
	_, errText, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点B")
	if code != exitBusiness || !strings.Contains(errText, "已登记") {
		t.Fatalf("重复登记应失败(1): code=%d err=%s", code, errText)
	}
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前站点: 站点A") || strings.Count(out, "收件") != 1 {
		t.Fatalf("重复登记不得改变原记录: code=%d out=%s", code, out)
	}

	// 查询不存在的包裹：报错、退出 1。
	_, errText, code = runCLI(t, dbPath, "query", "--id", "GHOST")
	if code != exitBusiness || !strings.Contains(errText, "包裹不存在") {
		t.Fatalf("查询不存在包裹应报错(1): code=%d err=%s", code, errText)
	}

	// 交接：一件包裹不在源站点，整次失败，所有包裹不变。
	_, errText, code = runCLI(t, dbPath, "handoff",
		"--request", "RBAD", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "GHOST")
	if code != exitBusiness || !strings.Contains(errText, "整次交接未执行") {
		t.Fatalf("含未登记包裹的交接应整体失败: code=%d err=%s", code, errText)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if !strings.Contains(out, "当前站点: 站点A") || strings.Contains(out, "交接") {
		t.Fatalf("失败交接不得改动 P001: %s", out)
	}
	// 失败不占用请求号：纠正后同号可成功。
	out, _, code = runCLI(t, dbPath, "handoff",
		"--request", "RBAD", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002")
	if code != 0 || !strings.Contains(out, "交接成功") || strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("纠正后交接应成功: code=%d out=%s", code, out)
	}

	// 查询轨迹：收件 + 交接，交接记录带请求号。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 {
		t.Fatalf("查询失败: %d", code)
	}
	if !strings.Contains(out, "当前站点: 站点B") ||
		!strings.Contains(out, "1. 操作: 收件") ||
		!strings.Contains(out, "2. 操作: 交接") ||
		!strings.Contains(out, "请求号: RBAD") {
		t.Fatalf("轨迹展示不符:\n%s", out)
	}

	// 同请求号、同内容、集合换序：重放首次结果，不追加轨迹。
	out, _, code = runCLI(t, dbPath, "handoff",
		"--request", "RBAD", "--from", "站点A", "--to", "站点B",
		"--parcel", "P002", "--parcel", "P001")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("重复请求应重放: code=%d out=%s", code, out)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if strings.Count(out, "操作: 交接") != 1 {
		t.Fatalf("重放不得追加交接轨迹:\n%s", out)
	}

	// 同请求号换业务内容：冲突。
	_, errText, code = runCLI(t, dbPath, "handoff",
		"--request", "RBAD", "--from", "站点A", "--to", "站点X",
		"--parcel", "P001")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("请求号内容冲突应失败: code=%d err=%s", code, errText)
	}
}

func TestCLIValidationFailures(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	cases := []struct {
		args   []string
		errSub string
	}{
		{[]string{"register", "--id", "   ", "--station", "A"}, "不可为空"},
		{[]string{"register", "--id", "P1", "--station", ""}, "不可为空"},
		{[]string{"handoff", "--request", "R1", "--from", "A", "--to", "A", "--parcel", "P1"}, "不能相同"},
		{[]string{"handoff", "--request", "R1", "--from", "A", "--to", "B"}, "不可为空"},
		{[]string{"handoff", "--request", "R1", "--from", "A", "--to", "B",
			"--parcel", "P1", "--parcel", "P1"}, "重复"},
		{[]string{"handoff", "--request", "  ", "--from", "A", "--to", "B", "--parcel", "P1"}, "不可为空"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		full := append([]string{"--data", dbPath}, c.args...)
		code := run(full, &out, &errb)
		if code != exitBusiness || !strings.Contains(errb.String(), c.errSub) {
			t.Fatalf("%v 应失败(1)且提示 %q，得到 code=%d err=%q", c.args, c.errSub, code, errb.String())
		}
	}
}

func TestCLIEndToEndReturn(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"

	runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A")
	runCLI(t, dbPath, "register", "--id", "P002", "--station", "站点A")
	out, _, code := runCLI(t, dbPath, "handoff",
		"--request", "R1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002")
	if code != 0 {
		t.Fatalf("交接失败: code=%d", code)
	}

	// 整批退回：输出可辨认源站、目的站、原因、两个请求号与全体包裹。
	out, _, code = runCLI(t, dbPath, "return", "--request", "RT1", "--handoff", "R1", "--reason", "错发站点")
	if code != 0 || !strings.Contains(out, "退回成功") ||
		!strings.Contains(out, "退回请求号: RT1") || !strings.Contains(out, "原交接请求号: R1") ||
		!strings.Contains(out, "源站点: 站点B") || !strings.Contains(out, "目的站点: 站点A") ||
		!strings.Contains(out, "退回原因: 错发站点") ||
		!strings.Contains(out, "- P001") || !strings.Contains(out, "- P002") {
		t.Fatalf("退回输出不符: code=%d out=%s", code, out)
	}

	// 查询展示退回的源站、目的站、时间、原因、两个请求号，且原轨迹保留。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前站点: 站点A") ||
		!strings.Contains(out, "1. 操作: 收件") || !strings.Contains(out, "2. 操作: 交接") ||
		!strings.Contains(out, "3. 操作: 退回") ||
		!strings.Contains(out, "源站: 站点B") || !strings.Contains(out, "目的站: 站点A") ||
		!strings.Contains(out, "退回请求号: RT1") || !strings.Contains(out, "原交接请求号: R1") ||
		!strings.Contains(out, "原因: 错发站点") {
		t.Fatalf("退回轨迹展示不符: code=%d out=%s", code, out)
	}

	// 同退回请求号、同原交接、同原因：重放首次结果，不追加轨迹。
	out, _, code = runCLI(t, dbPath, "return", "--request", "RT1", "--handoff", "R1", "--reason", "错发站点")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("重复退回应重放: code=%d out=%s", code, out)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if strings.Count(out, "操作: 退回") != 1 {
		t.Fatalf("重放不得追加退回轨迹:\n%s", out)
	}

	// 同退回请求号换原因：冲突。
	_, errText, code := runCLI(t, dbPath, "return", "--request", "RT1", "--handoff", "R1", "--reason", "别的原因")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("退回请求号内容冲突应失败: code=%d err=%s", code, errText)
	}

	// 换退回请求号再次退回同一原交接：失败。
	_, errText, code = runCLI(t, dbPath, "return", "--request", "RT2", "--handoff", "R1", "--reason", "再退")
	if code != exitBusiness || !strings.Contains(errText, "不能再次退回") {
		t.Fatalf("已退回的原交接再次退回应失败: code=%d err=%s", code, errText)
	}

	// 原 handoff 请求同内容重放照常，不重新移动包裹。
	out, _, code = runCLI(t, dbPath, "handoff",
		"--request", "R1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("已退回的原交接重放应返回首次结果: code=%d out=%s", code, out)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if !strings.Contains(out, "当前站点: 站点A") || strings.Count(out, "操作: 交接") != 1 {
		t.Fatalf("交接重放不得重新移动包裹:\n%s", out)
	}
}

func TestCLIReturnValidation(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A")
	runCLI(t, dbPath, "handoff", "--request", "R1", "--from", "站点A", "--to", "站点B", "--parcel", "P001")

	cases := []struct {
		args   []string
		errSub string
	}{
		{[]string{"return", "--request", "  ", "--handoff", "R1", "--reason", "x"}, "不可为空"},
		{[]string{"return", "--request", "RT1", "--handoff", "", "--reason", "x"}, "不可为空"},
		{[]string{"return", "--request", "RT1", "--handoff", "R1", "--reason", " \t "}, "不可为空"},
		{[]string{"return", "--request", "RT1", "--handoff", "NOPE", "--reason", "x"}, "不存在"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		full := append([]string{"--data", dbPath}, c.args...)
		code := run(full, &out, &errb)
		if code != exitBusiness || !strings.Contains(errb.String(), c.errSub) {
			t.Fatalf("%v 应失败(1)且提示 %q，得到 code=%d err=%q", c.args, c.errSub, code, errb.String())
		}
	}
	// 空白清洗后等价的提交应视为同一请求（此处首次成功，再次提交为重放）。
	out, _, code := runCLI(t, dbPath, "return", "--request", " RT1 ", "--handoff", " R1 ", "--reason", " 错发 ")
	if code != 0 || !strings.Contains(out, "退回成功") {
		t.Fatalf("两端空白应被清洗后成功: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, dbPath, "return", "--request", "RT1", "--handoff", "R1", "--reason", "错发")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("清洗后同内容应重放: code=%d out=%s", code, out)
	}
}
