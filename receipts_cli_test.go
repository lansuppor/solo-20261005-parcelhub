package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeReceiptsFile 把内容写入临时目录下的回执导入文件，返回路径。
func writeReceiptsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "receipts.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写导入文件失败: %v", err)
	}
	return path
}

// setupTwoBatches 登记三件包裹并派出两个批次：B1(P001,P002)、B2(P003)。
func setupTwoBatches(t *testing.T, dbPath string) {
	t.Helper()
	for _, id := range []string{"P001", "P002", "P003"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败", id)
		}
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A", "--courier", "张三",
		"--parcel", "P001", "--parcel", "P002"); code != 0 {
		t.Fatalf("批次 B1 出站失败")
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B2", "--station", "站点A", "--courier", "李四",
		"--parcel", "P003"); code != 0 {
		t.Fatalf("批次 B2 出站失败")
	}
}

func TestCLIReceiptsImportAcrossBatches(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	setupTwoBatches(t, dbPath)

	file := writeReceiptsFile(t, `[
	  {"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"},
	  {"request": "RC2", "batch": "B1", "parcel": "P002", "result": "失败", "reason": "收件人不在"},
	  {"request": "RC3", "batch": "B2", "parcel": "P003", "result": "签收"}
	]`)
	out, errText, code := runCLI(t, dbPath, "receipts", "--file", file)
	if code != 0 {
		t.Fatalf("整批导入应成功: code=%d err=%s", code, errText)
	}
	for _, want := range []string{
		"回执导入成功（共 3 条：新增 3 条，重放 0 条）",
		"1. 请求号: RC1    批次号: B1    包裹编号: P001    结果: 签收",
		"2. 请求号: RC2    批次号: B1    包裹编号: P002    结果: 失败    原因: 收件人不在",
		"3. 请求号: RC3    批次号: B2    包裹编号: P003    结果: 签收",
		"发生时间:", "新增",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("导入输出缺少 %q:\n%s", want, out)
		}
	}

	// 状态与轨迹：签收 -> 已签收；失败 -> 回到出发站在站，轨迹含回执记录。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前状态: 已签收") || !strings.Contains(out, "操作: 回执") {
		t.Fatalf("P001 应为已签收且含回执轨迹: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, dbPath, "query", "--id", "P002")
	if code != 0 || !strings.Contains(out, "当前站点: 站点A") || !strings.Contains(out, "当前状态: 在站") ||
		!strings.Contains(out, "结果: 失败") || !strings.Contains(out, "原因: 收件人不在") {
		t.Fatalf("P002 应恢复在站且含失败回执轨迹: code=%d out=%s", code, out)
	}

	// 批次进度：B1 全部回执自动完成，B2 完成。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 已完成") || !strings.Contains(out, "2/2 已回执") {
		t.Fatalf("B1 应已完成: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B2")
	if code != 0 || !strings.Contains(out, "批次状态: 已完成") {
		t.Fatalf("B2 应已完成: code=%d out=%s", code, out)
	}
}

func TestCLIReceiptsReplayAcrossEntries(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	setupTwoBatches(t, dbPath)

	file := writeReceiptsFile(t, `[
	  {"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"},
	  {"request": "RC2", "batch": "B1", "parcel": "P002", "result": "失败", "reason": "收件人不在"}
	]`)
	if _, _, code := runCLI(t, dbPath, "receipts", "--file", file); code != 0 {
		t.Fatalf("首次导入失败: %d", code)
	}

	// 文件提交后的结果能用逐件 receipt 重放。
	out, _, code := runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001", "--result", "签收")
	if code != 0 || !strings.Contains(out, "请求号重复提交，返回首次保存的结果") {
		t.Fatalf("receipt 应重放文件导入的结果: code=%d out=%s", code, out)
	}

	// 相同内容换文件、重排记录：全部重放，不改写台账。
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("读取台账失败: %v", err)
	}
	file2 := writeReceiptsFile(t, `[
	  {"request": "RC2", "batch": "B1", "parcel": "P002", "result": "失败", "reason": "收件人不在"},
	  {"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"}
	]`)
	out, _, code = runCLI(t, dbPath, "receipts", "--file", file2)
	if code != 0 || !strings.Contains(out, "回执导入成功（共 2 条：新增 0 条，重放 2 条）") {
		t.Fatalf("重排重放应成功且全部标记重放: code=%d out=%s", code, out)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("读取台账失败: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("全部为历史重放时不应改写台账")
	}

	// 逐件提交后的结果也能从文件重放；新记录在同一份文件中继续生效。
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC3", "--batch", "B2", "--parcel", "P003", "--result", "签收"); code != 0 {
		t.Fatalf("逐件回执失败: %d", code)
	}
	file3 := writeReceiptsFile(t, `[
	  {"request": "RC3", "batch": "B2", "parcel": "P003", "result": "签收"}
	]`)
	out, _, code = runCLI(t, dbPath, "receipts", "--file", file3)
	if code != 0 || !strings.Contains(out, "重放 1 条") {
		t.Fatalf("文件应重放逐件提交的结果: code=%d out=%s", code, out)
	}
}

func TestCLIReceiptsMixedNewAndReplay(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	setupTwoBatches(t, dbPath)

	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001", "--result", "签收"); code != 0 {
		t.Fatalf("逐件回执失败: %d", code)
	}
	file := writeReceiptsFile(t, `[
	  {"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"},
	  {"request": "RC2", "batch": "B1", "parcel": "P002", "result": "失败", "reason": "  收件人不在  "}
	]`)
	out, _, code := runCLI(t, dbPath, "receipts", "--file", file)
	if code != 0 || !strings.Contains(out, "回执导入成功（共 2 条：新增 1 条，重放 1 条）") {
		t.Fatalf("混合导入应成功: code=%d out=%s", code, out)
	}
	if !strings.Contains(out, "原因: 收件人不在") {
		t.Fatalf("原因应清理两端空白: %s", out)
	}
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 已完成") {
		t.Fatalf("B1 应已完成: code=%d out=%s", code, out)
	}
}

func TestCLIReceiptsRejectKeepsLedgerUntouched(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	setupTwoBatches(t, dbPath)

	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{"空字段", `[{"request": "  ", "batch": "B1", "parcel": "P001", "result": "签收"}]`, "不可为空"},
		{"结果非法", `[{"request": "RC1", "batch": "B1", "parcel": "P001", "result": "丢失"}]`, "回执结果只能是"},
		{"失败无原因", `[{"request": "RC1", "batch": "B1", "parcel": "P001", "result": "失败", "reason": "  "}]`, "原因不可为空"},
		{"签收带原因", `[{"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收", "reason": "多写"}]`, "不可带原因"},
		{"文件内请求号重复", `[
		  {"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"},
		  {"request": "RC1", "batch": "B1", "parcel": "P002", "result": "签收"}
		]`, "请求号重复"},
		{"批次不存在", `[{"request": "RC1", "batch": "B9", "parcel": "P001", "result": "签收"}]`, `批次号 "B9" 不存在`},
		{"包裹不属于批次", `[{"request": "RC1", "batch": "B1", "parcel": "P003", "result": "签收"}]`, "不属于批次"},
		{"同批同包裹换请求号", `[
		  {"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"},
		  {"request": "RC2", "batch": "B1", "parcel": "P001", "result": "失败", "reason": "拒收"}
		]`, "只能成功回执一次"},
		{"部分有效也不提交", `[
		  {"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"},
		  {"request": "RC2", "batch": "B9", "parcel": "P002", "result": "签收"}
		]`, "第 2 条记录"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, _ := os.ReadFile(dbPath)
			file := writeReceiptsFile(t, c.content)
			out, errText, code := runCLI(t, dbPath, "receipts", "--file", file)
			if code != exitBusiness {
				t.Fatalf("应以状态码 1 退出: code=%d out=%s", code, out)
			}
			if !strings.Contains(errText, c.wantErr) {
				t.Fatalf("错误提示应包含 %q: %s", c.wantErr, errText)
			}
			if out != "" {
				t.Fatalf("失败时不应输出任何已提交成功结果: %s", out)
			}
			after, _ := os.ReadFile(dbPath)
			if string(before) != string(after) {
				t.Fatalf("整份拒绝后台账不应变化")
			}
			// 新请求号均不占用：包裹仍在配送中，可纠正后重试。
			qout, _, qcode := runCLI(t, dbPath, "query", "--id", "P001")
			if qcode != 0 || !strings.Contains(qout, "当前状态: 配送中") {
				t.Fatalf("P001 应仍在配送中: %s", qout)
			}
		})
	}

	// 纠正后重试成功。
	file := writeReceiptsFile(t, `[{"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"}]`)
	if out, _, code := runCLI(t, dbPath, "receipts", "--file", file); code != 0 || !strings.Contains(out, "新增 1 条") {
		t.Fatalf("纠正后重试应成功: code=%d out=%s", code, out)
	}
}

func TestCLIReceiptsConflictWithExistingRequest(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	setupTwoBatches(t, dbPath)

	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001", "--result", "签收"); code != 0 {
		t.Fatalf("逐件回执失败: %d", code)
	}
	// 同请求号换内容：冲突，整份拒绝。
	file := writeReceiptsFile(t, `[
	  {"request": "RC1", "batch": "B1", "parcel": "P001", "result": "失败", "reason": "拒收"},
	  {"request": "RC2", "batch": "B1", "parcel": "P002", "result": "签收"}
	]`)
	out, errText, code := runCLI(t, dbPath, "receipts", "--file", file)
	if code != exitBusiness || !strings.Contains(errText, "内容冲突") || !strings.Contains(errText, "第 1 条记录") {
		t.Fatalf("应报冲突并指出位置: code=%d err=%s", code, errText)
	}
	if out != "" {
		t.Fatalf("冲突时不应输出成功结果: %s", out)
	}
	// RC2 未被占用，P002 仍可正常回执。
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC2", "--batch", "B1", "--parcel", "P002", "--result", "签收"); code != 0 {
		t.Fatalf("RC2 应未被占用: %d", code)
	}
}

func TestCLIReceiptsFileProblems(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	setupTwoBatches(t, dbPath)

	// 文件不存在。
	if _, errText, code := runCLI(t, dbPath, "receipts", "--file", filepath.Join(t.TempDir(), "nope.json")); code != exitBusiness ||
		!strings.Contains(errText, "读取导入文件失败") {
		t.Fatalf("文件不存在应退出 1: code=%d err=%s", code, errText)
	}
	// 非法 JSON。
	if _, errText, code := runCLI(t, dbPath, "receipts", "--file", writeReceiptsFile(t, `{not json`)); code != exitBusiness ||
		!strings.Contains(errText, "解析导入文件失败") {
		t.Fatalf("非法 JSON 应退出 1: code=%d err=%s", code, errText)
	}
	// 空数组与 null 都视为空列表。
	for _, content := range []string{`[]`, `null`, `  `} {
		if _, _, code := runCLI(t, dbPath, "receipts", "--file", writeReceiptsFile(t, content)); code != exitBusiness {
			t.Fatalf("空列表 %q 应退出 1: code=%d", content, code)
		}
	}
	// 缺 --file。
	if _, _, code := runCLI(t, dbPath, "receipts"); code != exitBusiness {
		t.Fatalf("缺 --file 应退出 1: code=%d", code)
	}
	// 输入文件不被改写。
	file := writeReceiptsFile(t, `[{"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"}]`)
	before, _ := os.ReadFile(file)
	if _, _, code := runCLI(t, dbPath, "receipts", "--file", file); code != 0 {
		t.Fatalf("导入应成功: %d", code)
	}
	after, _ := os.ReadFile(file)
	if string(before) != string(after) {
		t.Fatalf("输入文件不应被改写")
	}
}

func TestCLIReceiptsCorruptLedger(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(dbPath, []byte(`{"version":1,"parcels":{},"handoffs":{},"receipts":{"RC1":null}}`), 0o644); err != nil {
		t.Fatalf("写损坏台账失败: %v", err)
	}
	file := writeReceiptsFile(t, `[{"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"}]`)
	before, _ := os.ReadFile(dbPath)
	_, errText, code := runCLI(t, dbPath, "receipts", "--file", file)
	if code != exitBusiness || !strings.Contains(errText, "数据文件已损坏") {
		t.Fatalf("损坏台账应明确拒绝: code=%d err=%s", code, errText)
	}
	after, _ := os.ReadFile(dbPath)
	if string(before) != string(after) {
		t.Fatalf("损坏台账不应被覆盖")
	}
}

func TestCLIReceiptsReplayIgnoresCurrentState(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	setupTwoBatches(t, dbPath)

	// P002 失败回执回到出发站后，随新批次 B3 再次出站；旧请求重放不检查当前状态。
	file := writeReceiptsFile(t, `[{"request": "RC2", "batch": "B1", "parcel": "P002", "result": "失败", "reason": "收件人不在"}]`)
	if _, _, code := runCLI(t, dbPath, "receipts", "--file", file); code != 0 {
		t.Fatalf("导入失败: %d", code)
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B3", "--station", "站点A", "--courier", "王五", "--parcel", "P002"); code != 0 {
		t.Fatalf("B3 出站失败: %d", code)
	}
	// 冻结另一件包裹也不影响历史重放。
	if _, _, code := runCLI(t, dbPath, "freeze", "--incident", "E1", "--parcel", "P003", "--station", "站点A", "--reason", "破损"); code != 0 {
		// P003 在配送中，不能冻结；改用未登记包裹之外的在站包裹。
		if _, _, code2 := runCLI(t, dbPath, "register", "--id", "P009", "--station", "站点A"); code2 != 0 {
			t.Fatalf("登记 P009 失败: %d", code2)
		}
		if _, _, code2 := runCLI(t, dbPath, "freeze", "--incident", "E1", "--parcel", "P009", "--station", "站点A", "--reason", "破损"); code2 != 0 {
			t.Fatalf("冻结失败: %d", code2)
		}
	}
	out, _, code := runCLI(t, dbPath, "receipts", "--file", file)
	if code != 0 || !strings.Contains(out, "重放 1 条") {
		t.Fatalf("进入新批次后旧请求仍应重放: code=%d out=%s", code, out)
	}
	// 重放不改变新批次进度：P002 仍在 B3 配送中。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P002")
	if code != 0 || !strings.Contains(out, "当前状态: 配送中") {
		t.Fatalf("重放不应改变当前状态: %s", out)
	}
}
