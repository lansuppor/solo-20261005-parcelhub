package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCLIEndToEndFreezeUnfreeze(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"

	if _, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A"); code != 0 {
		t.Fatalf("登记失败: %d", code)
	}

	// 冻结成功输出。
	out, _, code := runCLI(t, dbPath, "freeze",
		"--incident", "E1", "--parcel", "P001", "--station", "站点A", "--reason", "外包装破损")
	if code != 0 || !strings.Contains(out, "冻结成功") ||
		!strings.Contains(out, "异常单号: E1") || !strings.Contains(out, "包裹编号: P001") ||
		!strings.Contains(out, "所在站点: 站点A") || !strings.Contains(out, "当前状态: 异常冻结") ||
		!strings.Contains(out, "异常原因: 外包装破损") {
		t.Fatalf("冻结输出不符: code=%d out=%s", code, out)
	}

	// query：当前状态、当前未解除异常、冻结轨迹（异常单号/原因/站点/时间可辨认）。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前状态: 异常冻结") ||
		!strings.Contains(out, "当前未解除异常: 异常单号: E1") ||
		!strings.Contains(out, "原因: 外包装破损") ||
		!strings.Contains(out, "2. 操作: 冻结") ||
		!strings.Contains(out, "异常单号: E1") {
		t.Fatalf("冻结后查询不符: code=%d out=%s", code, out)
	}

	// 冻结期间首次交接/出站整批拒绝。
	_, errText, code := runCLI(t, dbPath, "handoff",
		"--request", "R1", "--from", "站点A", "--to", "站点B", "--parcel", "P001")
	if code != exitBusiness || !strings.Contains(errText, "冻结期间整批交接拒绝") {
		t.Fatalf("冻结期间交接应整批失败: code=%d err=%s", code, errText)
	}
	_, errText, code = runCLI(t, dbPath, "dispatch",
		"--batch", "B1", "--station", "站点A", "--courier", "张三", "--parcel", "P001")
	if code != exitBusiness || !strings.Contains(errText, "冻结期间整批出站拒绝") {
		t.Fatalf("冻结期间出站应整批失败: code=%d err=%s", code, errText)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if strings.Contains(out, "交接") || strings.Contains(out, "出站") {
		t.Fatalf("被拒绝的操作不得留下轨迹:\n%s", out)
	}

	// 同异常单号同内容重放：返回首次结果，不追加冻结记录。
	out, _, code = runCLI(t, dbPath, "freeze",
		"--incident", " E1 ", "--parcel", "P001", "--station", "站点A", "--reason", " 外包装破损 ")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("重复冻结应重放: code=%d out=%s", code, out)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if strings.Count(out, "操作: 冻结") != 1 {
		t.Fatalf("重放不得追加冻结轨迹:\n%s", out)
	}

	// 同号换内容：冲突。
	_, errText, code = runCLI(t, dbPath, "freeze",
		"--incident", "E1", "--parcel", "P001", "--station", "站点A", "--reason", "别的原因")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("异常单号内容冲突应失败: code=%d err=%s", code, errText)
	}

	// 解除成功输出。
	out, _, code = runCLI(t, dbPath, "unfreeze",
		"--request", "U1", "--incident", "E1", "--note", "已核实放行")
	if code != 0 || !strings.Contains(out, "解除成功") ||
		!strings.Contains(out, "解除请求号: U1") || !strings.Contains(out, "异常单号: E1") ||
		!strings.Contains(out, "包裹编号: P001") || !strings.Contains(out, "当前状态: 在站") ||
		!strings.Contains(out, "处理说明: 已核实放行") {
		t.Fatalf("解除输出不符: code=%d out=%s", code, out)
	}

	// query：恢复在站、无未解除异常行、解除轨迹完整，冻结轨迹保留。
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if !strings.Contains(out, "当前状态: 在站") || strings.Contains(out, "当前未解除异常") ||
		!strings.Contains(out, "3. 操作: 解除冻结") ||
		!strings.Contains(out, "解除请求号: U1") || !strings.Contains(out, "处理说明: 已核实放行") ||
		strings.Count(out, "操作: 冻结") != 1 {
		t.Fatalf("解除后查询不符:\n%s", out)
	}

	// 已解除异常单不能重复解除；旧异常单号同内容只是重放冻结结果。
	_, errText, code = runCLI(t, dbPath, "unfreeze", "--request", "U2", "--incident", "E1", "--note", "再处理")
	if code != exitBusiness || !strings.Contains(errText, "已解除") {
		t.Fatalf("重复解除应失败: code=%d err=%s", code, errText)
	}
	out, _, code = runCLI(t, dbPath, "freeze",
		"--incident", "E1", "--parcel", "P001", "--station", "站点A", "--reason", "外包装破损")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("解除后旧异常单号同内容提交应重放首次结果: code=%d out=%s", code, out)
	}

	// 解除请求同内容重放：不检查当前状态（此时包裹在站、未冻结，仍返回首次解除结果）。
	out, _, code = runCLI(t, dbPath, "unfreeze", "--request", "U1", "--incident", "E1", "--note", "已核实放行")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("重复解除应重放: code=%d out=%s", code, out)
	}
	// 解除请求换内容：冲突。
	_, errText, code = runCLI(t, dbPath, "unfreeze", "--request", "U1", "--incident", "E1", "--note", "别的说明")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("解除请求号内容冲突应失败: code=%d err=%s", code, errText)
	}

	// 之后可用新异常单再次冻结。
	out, _, code = runCLI(t, dbPath, "freeze",
		"--incident", "E2", "--parcel", "P001", "--station", "站点A", "--reason", "再次异常")
	if code != 0 || !strings.Contains(out, "冻结成功") || strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("新异常单应可再次冻结: code=%d out=%s", code, out)
	}
}

func TestCLIFreezeBlocksReturnButNotFlow(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"

	runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A")
	runCLI(t, dbPath, "handoff", "--request", "R1", "--from", "站点A", "--to", "站点B", "--parcel", "P001")

	// 冻结再解除：没有其他新流转，原本可退回的交接仍可整批退回。
	if _, _, code := runCLI(t, dbPath, "freeze",
		"--incident", "E1", "--parcel", "P001", "--station", "站点B", "--reason", "破损"); code != 0 {
		t.Fatal("冻结应成功")
	}
	// 冻结期间整批退回拒绝。
	_, errText, code := runCLI(t, dbPath, "return", "--request", "RT1", "--handoff", "R1", "--reason", "错发")
	if code != exitBusiness || !strings.Contains(errText, "冻结期间整批退回拒绝") {
		t.Fatalf("冻结期间退回应整批失败: code=%d err=%s", code, errText)
	}
	if _, _, code := runCLI(t, dbPath, "unfreeze", "--request", "U1", "--incident", "E1", "--note", "放行"); code != 0 {
		t.Fatal("解除应成功")
	}
	out, _, code := runCLI(t, dbPath, "return", "--request", "RT1", "--handoff", "R1", "--reason", "错发")
	if code != 0 || !strings.Contains(out, "退回成功") {
		t.Fatalf("解除后无新流转旧交接应仍可退回: code=%d out=%s", code, out)
	}

	// 发生新交接后，冻结/解除穿插其间，旧交接仍不能退回。
	runCLI(t, dbPath, "handoff", "--request", "R2", "--from", "站点A", "--to", "站点C", "--parcel", "P001")
	runCLI(t, dbPath, "freeze", "--incident", "E2", "--parcel", "P001", "--station", "站点C", "--reason", "破损")
	runCLI(t, dbPath, "unfreeze", "--request", "U2", "--incident", "E2", "--note", "放行")
	_, errText, code = runCLI(t, dbPath, "return", "--request", "RT2", "--handoff", "R1", "--reason", "错发")
	if code != exitBusiness {
		t.Fatalf("发生新交接后旧交接不能退回: code=%d err=%s", code, errText)
	}
}

func TestCLIFreezeBatchRejectsOthersUnchanged(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A")
	runCLI(t, dbPath, "register", "--id", "P002", "--station", "站点A")
	runCLI(t, dbPath, "freeze", "--incident", "E1", "--parcel", "P002", "--station", "站点A", "--reason", "破损")

	// 含冻结包裹的交接整批拒绝：P001 不动，失败不占号。
	_, _, code := runCLI(t, dbPath, "handoff",
		"--request", "R1", "--from", "站点A", "--to", "站点B", "--parcel", "P001", "--parcel", "P002")
	if code != exitBusiness {
		t.Fatalf("含冻结包裹的交接应失败(1)，得到 %d", code)
	}
	out, _, _ := runCLI(t, dbPath, "query", "--id", "P001")
	if !strings.Contains(out, "当前站点: 站点A") || strings.Contains(out, "交接") {
		t.Fatalf("整批拒绝不得改动其他成员:\n%s", out)
	}
	// 解除后同请求号可成功（证明失败不占号）。
	runCLI(t, dbPath, "unfreeze", "--request", "U1", "--incident", "E1", "--note", "放行")
	out, _, code = runCLI(t, dbPath, "handoff",
		"--request", "R1", "--from", "站点A", "--to", "站点B", "--parcel", "P001", "--parcel", "P002")
	if code != 0 || !strings.Contains(out, "交接成功") {
		t.Fatalf("解除后同请求号交接应成功: code=%d out=%s", code, out)
	}
}

func TestCLIFreezeHistoricalReplayDuringFreeze(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A")
	runCLI(t, dbPath, "handoff", "--request", "R1", "--from", "站点A", "--to", "站点B", "--parcel", "P001")
	runCLI(t, dbPath, "freeze", "--incident", "E1", "--parcel", "P001", "--station", "站点B", "--reason", "破损")

	// 冻结期间历史交接同内容重放：返回首次结果，不移动包裹、不解除冻结。
	out, _, code := runCLI(t, dbPath, "handoff",
		"--request", "R1", "--from", "站点A", "--to", "站点B", "--parcel", "P001")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("冻结期间历史交接应重放: code=%d out=%s", code, out)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if !strings.Contains(out, "当前站点: 站点B") || !strings.Contains(out, "当前状态: 异常冻结") ||
		strings.Count(out, "操作: 交接") != 1 {
		t.Fatalf("历史重放不得移动包裹或解除冻结:\n%s", out)
	}
}

func TestCLIFreezeSameNamesAcrossNamespaces(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	runCLI(t, dbPath, "register", "--id", "X", "--station", "站点A")
	runCLI(t, dbPath, "handoff", "--request", "X", "--from", "站点A", "--to", "站点B", "--parcel", "X")
	// 异常单号、解除请求号均与包裹号/交接请求号同名。
	if _, _, code := runCLI(t, dbPath, "freeze",
		"--incident", "X", "--parcel", "X", "--station", "站点B", "--reason", "异常"); code != 0 {
		t.Fatal("异常单号同名应可用")
	}
	if _, _, code := runCLI(t, dbPath, "unfreeze",
		"--request", "X", "--incident", "X", "--note", "处理"); code != 0 {
		t.Fatal("解除请求号同名应可用")
	}
	// 同名交接重放不受影响。
	out, _, code := runCLI(t, dbPath, "handoff", "--request", "X", "--from", "站点A", "--to", "站点B", "--parcel", "X")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("同名编号不得影响交接去重: code=%d out=%s", code, out)
	}
}

func TestCLIFreezeValidation(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A")

	cases := []struct {
		args   []string
		errSub string
	}{
		{[]string{"freeze", "--incident", "  ", "--parcel", "P001", "--station", "站点A", "--reason", "x"}, "不可为空"},
		{[]string{"freeze", "--incident", "E1", "--parcel", "", "--station", "站点A", "--reason", "x"}, "不可为空"},
		{[]string{"freeze", "--incident", "E1", "--parcel", "P001", "--station", " \t", "--reason", "x"}, "不可为空"},
		{[]string{"freeze", "--incident", "E1", "--parcel", "P001", "--station", "站点A", "--reason", "   "}, "不可为空"},
		{[]string{"freeze", "--incident", "E1", "--parcel", "GHOST", "--station", "站点A", "--reason", "x"}, "未登记"},
		{[]string{"freeze", "--incident", "E1", "--parcel", "P001", "--station", "站点B", "--reason", "x"}, "不在指定站点"},
		{[]string{"unfreeze", "--request", "  ", "--incident", "E1", "--note", "x"}, "不可为空"},
		{[]string{"unfreeze", "--request", "U1", "--incident", "", "--note", "x"}, "不可为空"},
		{[]string{"unfreeze", "--request", "U1", "--incident", "E1", "--note", " \t "}, "不可为空"},
		{[]string{"unfreeze", "--request", "U1", "--incident", "NOPE", "--note", "x"}, "不存在"},
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

func TestCLIFreezeHelpVariants(t *testing.T) {
	for _, args := range [][]string{
		{"freeze", "--help"},
		{"unfreeze", "-h"},
	} {
		var out, errb bytes.Buffer
		code := run(args, &out, &errb)
		if code != 0 || !strings.Contains(out.String(), appName) || errb.Len() != 0 {
			t.Fatalf("%v 帮助应以 0 退出且不含 stderr: code=%d", args, code)
		}
	}
	// 主帮助列出两个新命令。
	var out, errb bytes.Buffer
	if code := run([]string{"--help"}, &out, &errb); code != 0 ||
		!strings.Contains(out.String(), "freeze") || !strings.Contains(out.String(), "unfreeze") {
		t.Fatalf("主帮助应列出 freeze/unfreeze: code=%d", code)
	}
}
