package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readFileText 读取文件内容，失败时终止测试。
func readFileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取文件失败: %v", err)
	}
	return string(b)
}

// writeImportFile 把内容写入临时目录下的导入文件并返回路径。
func writeImportFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入导入文件失败: %v", err)
	}
	return path
}

func TestImportReceiptsSuccessAcrossBatches(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	mustDispatch(t, s, "B2", "站点A", "李四", []string{"P003"}, tClock(2026, 10, 5, 10, 30))

	t2 := tClock(2026, 10, 5, 11, 0)
	items, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultSigned},
		{Request: "RC2", Batch: "B1", Parcel: "P002", Result: resultFailed, Reason: "收件人不在"},
		{Request: "RC3", Batch: "B2", Parcel: "P003", Result: resultSigned},
	}, t2)
	if err != nil {
		t.Fatalf("整批导入应成功: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("应返回 3 条结果: %d", len(items))
	}
	for i, it := range items {
		if it.Replayed {
			t.Fatalf("第 %d 条应为新增: %+v", i+1, it)
		}
		if !it.Time.Equal(t2) {
			t.Fatalf("第 %d 条发生时间应为导入时间: %+v", i+1, it)
		}
	}
	// 结果按文件顺序排列。
	if items[0].Record.Request != "RC1" || items[1].Record.Request != "RC2" || items[2].Record.Request != "RC3" {
		t.Fatalf("结果应按文件顺序排列: %+v", items)
	}

	p1, _ := s.Query("P001")
	if p1.Status != statusSigned || p1.Station != "站点A" {
		t.Fatalf("签收后应为已签收且站点保留: %+v", p1)
	}
	e1 := p1.Trail[len(p1.Trail)-1]
	if e1.Op != "回执" || e1.Batch != "B1" || e1.Result != resultSigned || e1.Request != "RC1" || !e1.Time.Equal(t2) {
		t.Fatalf("签收回执轨迹不符: %+v", e1)
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusInStation || p2.Station != "站点A" {
		t.Fatalf("失败回执后应恢复在站并回到出发站: %+v", p2)
	}
	e2 := p2.Trail[len(p2.Trail)-1]
	if e2.Op != "回执" || e2.Result != resultFailed || e2.Reason != "收件人不在" || e2.Request != "RC2" {
		t.Fatalf("失败回执轨迹不符: %+v", e2)
	}

	// 批次进度更新：两个批次的成员全部回执后都完成。
	if !s.data.Batches["B1"].Done() || !s.data.Batches["B2"].Done() {
		t.Fatal("全部成员回执后批次应自动完成")
	}
	if s.data.Batches["B1"].Receipts["P002"].Reason != "收件人不在" {
		t.Fatalf("批次回执进度应保留失败原因: %+v", s.data.Batches["B1"].Receipts["P002"])
	}
}

func TestImportReceiptsAtomicRejection(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	before := readFileText(t, path)

	cases := []struct {
		name    string
		records []ReceiptImportRecord
		wantSub string
	}{
		{"批次不存在", []ReceiptImportRecord{
			{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultSigned},
			{Request: "RC2", Batch: "NOPE", Parcel: "P002", Result: resultSigned},
		}, "第 2 条记录"},
		{"包裹不属于批次", []ReceiptImportRecord{
			{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultSigned},
			{Request: "RC2", Batch: "B1", Parcel: "P003", Result: resultSigned},
		}, "不属于批次"},
		{"同文件换请求号重复回执同批次同包裹", []ReceiptImportRecord{
			{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultSigned},
			{Request: "RC2", Batch: "B1", Parcel: "P001", Result: resultFailed, Reason: "破损"},
		}, "只能成功回执一次"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.ImportReceipts(c.records, tClock(2026, 10, 5, 11, 0))
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("应整份拒绝且提示 %q: %v", c.wantSub, err)
			}
			if after := readFileText(t, path); after != before {
				t.Fatal("整份拒绝时不得写入数据文件")
			}
			// 新请求号均不占用，包裹状态、轨迹与批次进度不变。
			for _, rec := range c.records {
				if _, ok := s.data.Receipts[rec.Request]; ok {
					t.Fatalf("被拒绝的导入不得占用请求号 %q", rec.Request)
				}
			}
			p1, _ := s.Query("P001")
			if p1.Status != statusDelivering || len(p1.Trail) != 2 {
				t.Fatalf("被拒绝的导入不得改动包裹: %+v", p1)
			}
			if len(s.data.Batches["B1"].Receipts) != 0 {
				t.Fatal("被拒绝的导入不得改变批次进度")
			}
		})
	}

	// 纠正后同请求号可成功导入（失败不占号）。
	items, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultSigned},
		{Request: "RC2", Batch: "B1", Parcel: "P002", Result: resultFailed, Reason: "收件人不在"},
	}, tClock(2026, 10, 5, 12, 0))
	if err != nil || len(items) != 2 {
		t.Fatalf("纠正后导入应成功: %v", err)
	}
	if !s.data.Batches["B1"].Done() {
		t.Fatal("纠正导入后批次应完成")
	}
}

func TestImportReceiptsReplayMixedWithNew(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002", "P003"}, tClock(2026, 10, 5, 10, 0))

	// 逐件 receipt 先提交一条失败回执：实物回到出发站，恢复在站。
	t2 := tClock(2026, 10, 5, 11, 0)
	first, _, err := s.Receipt("RC1", "B1", "P001", resultFailed, "收件人不在", t2)
	if err != nil {
		t.Fatal(err)
	}
	// 失败包裹随后被冻结：同内容重放不检查当前状态，仍返回首次结果。
	mustFreeze(t, s, "E1", "P001", "站点A", "外包装破损", tClock(2026, 10, 5, 11, 30))

	// 文件混合历史重放（RC1）与新回执（RC2、RC3）。
	t3 := tClock(2026, 10, 5, 12, 0)
	items, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultFailed, Reason: "收件人不在"},
		{Request: "RC2", Batch: "B1", Parcel: "P002", Result: resultSigned},
		{Request: "RC3", Batch: "B1", Parcel: "P003", Result: resultSigned},
	}, t3)
	if err != nil {
		t.Fatalf("混合重放与新增的导入应成功: %v", err)
	}
	if len(items) != 3 || !items[0].Replayed || items[1].Replayed || items[2].Replayed {
		t.Fatalf("第 1 条应为重放、其余为新增: %+v", items)
	}
	// 重放返回首次结果与时间。
	if !items[0].Time.Equal(first.Time) || !items[0].Time.Equal(t2) {
		t.Fatalf("重放应返回首次时间 %v，得到 %v", first.Time, items[0].Time)
	}
	if !items[1].Time.Equal(t3) || !items[2].Time.Equal(t3) {
		t.Fatalf("新增记录应使用导入时间: %+v", items)
	}

	// 重放不改变被冻结包裹的状态与轨迹；新回执正常生效。
	p1, _ := s.Query("P001")
	if p1.Status != statusFrozen || len(p1.Trail) != 4 { // 收件、出站、回执、冻结
		t.Fatalf("重放不得改动被冻结包裹: %+v", p1)
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusSigned {
		t.Fatalf("新回执应生效: %+v", p2)
	}
	if !s.data.Batches["B1"].Done() {
		t.Fatal("全部成员回执后批次应完成")
	}
	if len(s.data.Receipts) != 3 {
		t.Fatalf("重放不得新增请求结果，应共 3 条: %d", len(s.data.Receipts))
	}

	// 相同内容换文件、重排记录再次导入：全部重放，不改写台账。
	before := readFileText(t, path)
	items, err = s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC3", Batch: "B1", Parcel: "P003", Result: resultSigned},
		{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultFailed, Reason: "收件人不在"},
		{Request: "RC2", Batch: "B1", Parcel: "P002", Result: resultSigned},
	}, tClock(2026, 10, 5, 13, 0))
	if err != nil {
		t.Fatalf("全部重放的导入应成功: %v", err)
	}
	for i, it := range items {
		if !it.Replayed {
			t.Fatalf("第 %d 条应为重放: %+v", i+1, it)
		}
	}
	if after := readFileText(t, path); after != before {
		t.Fatal("全部为历史重放时不得改写台账")
	}

	// 同请求号换内容：整份冲突拒绝，已有结果不变。
	_, err = s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC2", Batch: "B1", Parcel: "P002", Result: resultFailed, Reason: "别的原因"},
	}, tClock(2026, 10, 5, 14, 0))
	if err == nil || !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("换内容应报冲突: %v", err)
	}
	if after := readFileText(t, path); after != before {
		t.Fatal("冲突拒绝时不得改写台账")
	}
	saved := s.data.Receipts["RC2"]
	if saved.Result != resultSigned || saved.Reason != "" {
		t.Fatalf("冲突提交不得改动已有回执结果: %+v", saved)
	}
}

func TestImportReceiptsCrossEntryReplay(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))

	// 文件提交后的结果能用逐件 receipt 重放。
	if _, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultSigned},
	}, tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatal(err)
	}
	res, replayed, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || !res.Time.Equal(tClock(2026, 10, 5, 11, 0)) {
		t.Fatalf("文件提交的结果应能用 receipt 重放: err=%v replayed=%v res=%+v", err, replayed, res)
	}

	// 逐件提交后的结果能从文件重放。
	if _, _, err := s.Receipt("RC2", "B1", "P002", resultFailed, "收件人不在", tClock(2026, 10, 5, 12, 30)); err != nil {
		t.Fatal(err)
	}
	items, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC2", Batch: "B1", Parcel: "P002", Result: resultFailed, Reason: "收件人不在"},
	}, tClock(2026, 10, 5, 13, 0))
	if err != nil || len(items) != 1 || !items[0].Replayed || !items[0].Time.Equal(tClock(2026, 10, 5, 12, 30)) {
		t.Fatalf("逐件提交的结果应能从文件重放: err=%v items=%+v", err, items)
	}
}

func TestImportReceiptsPersistenceAcrossRestart(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	t2 := tClock(2026, 10, 5, 11, 0)
	if _, err := s.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultSigned},
		{Request: "RC2", Batch: "B1", Parcel: "P002", Result: resultFailed, Reason: "收件人不在"},
	}, t2); err != nil {
		t.Fatal(err)
	}

	// 重启后进度、首次时间与去重规则保持一致。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	if !s2.data.Batches["B1"].Done() {
		t.Fatal("重启后批次应保持完成")
	}
	items, err := s2.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC1", Batch: "B1", Parcel: "P001", Result: resultSigned},
		{Request: "RC2", Batch: "B1", Parcel: "P002", Result: resultFailed, Reason: "收件人不在"},
	}, tClock(2026, 10, 5, 12, 0))
	if err != nil || len(items) != 2 || !items[0].Replayed || !items[1].Replayed {
		t.Fatalf("重启后同内容导入应全部重放: err=%v items=%+v", err, items)
	}
	if !items[0].Time.Equal(t2) || !items[1].Time.Equal(t2) {
		t.Fatalf("重启后重放应返回首次时间: %+v", items)
	}
	// 换内容仍报冲突。
	if _, err := s2.ImportReceipts([]ReceiptImportRecord{
		{Request: "RC1", Batch: "B1", Parcel: "P002", Result: resultSigned},
	}, tClock(2026, 10, 5, 13, 0)); err == nil || !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("重启后换内容应报冲突: %v", err)
	}
}

func TestCLIReceiptImportEndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A")
	runCLI(t, dbPath, "register", "--id", "P002", "--station", "站点A")
	runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A", "--courier", "张三",
		"--parcel", "P001", "--parcel", "P002")

	content := `[
  {"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"},
  {"request": "RC2", "batch": "B1", "parcel": "P002", "result": "失败", "reason": "收件人不在"}
]`
	file := writeImportFile(t, "receipts.json", content)

	out, _, code := runCLI(t, dbPath, "receipt-import", "--file", file)
	if code != 0 || !strings.Contains(out, "回执导入成功") ||
		!strings.Contains(out, "共 2 条") || !strings.Contains(out, "新增 2 条") ||
		!strings.Contains(out, "请求号: RC1") || !strings.Contains(out, "结果: 签收") ||
		!strings.Contains(out, "请求号: RC2") || !strings.Contains(out, "原因: 收件人不在") ||
		!strings.Contains(out, "标记: 新增") || !strings.Contains(out, "时间: ") {
		t.Fatalf("导入输出不符: code=%d out=%s", code, out)
	}
	// 输入文件始终不改写。
	if got := readFileText(t, file); got != content {
		t.Fatalf("输入文件不得被改写:\n%s", got)
	}

	// 状态与批次进度生效。
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if !strings.Contains(out, "当前状态: 已签收") || !strings.Contains(out, "操作: 回执") {
		t.Fatalf("导入后查询不符:\n%s", out)
	}
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 已完成") || !strings.Contains(out, "逐件回执（2/2 已回执）") {
		t.Fatalf("导入后批次查询不符: code=%d out=%s", code, out)
	}

	// 相同内容换文件再次导入：全部重放，不追加轨迹。
	file2 := writeImportFile(t, "again.json", content)
	out, _, code = runCLI(t, dbPath, "receipt-import", "--file", file2)
	if code != 0 || !strings.Contains(out, "重放 2 条") || !strings.Contains(out, "标记: 重放") {
		t.Fatalf("换文件重放不符: code=%d out=%s", code, out)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if strings.Count(out, "操作: 回执") != 1 {
		t.Fatalf("重放不得追加回执轨迹:\n%s", out)
	}

	// 文件提交的结果能用逐件 receipt 重放。
	out, _, code = runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001", "--result", "签收")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("receipt 重放文件结果不符: code=%d out=%s", code, out)
	}
}

func TestCLIReceiptImportFileErrors(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A")
	runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A", "--courier", "张三", "--parcel", "P001")
	before := readFileText(t, dbPath)

	cases := []struct {
		name    string
		content *string // nil 表示文件不存在
		errSub  string
	}{
		{"文件不存在", nil, "读取回执导入文件失败"},
		{"非法 JSON", strPtr(`[{`), "解析回执导入文件失败"},
		{"空文件", strPtr(`  `), "解析回执导入文件失败"},
		{"空列表", strPtr(`[]`), "非空"},
		{"null 列表", strPtr(`null`), "非空"},
		{"多余内容", strPtr(`[{"request":"R1","batch":"B1","parcel":"P001","result":"签收"}] {}`), "多余内容"},
		{"未知字段", strPtr(`[{"request":"R1","batch":"B1","parcel":"P001","result":"签收","extra":1}]`), "解析回执导入文件失败"},
		{"请求号为空白", strPtr(`[{"request":"  ","batch":"B1","parcel":"P001","result":"签收"}]`), "第 1 条记录"},
		{"结果无效", strPtr(`[{"request":"R1","batch":"B1","parcel":"P001","result":"丢了"}]`), "第 1 条记录"},
		{"失败缺原因", strPtr(`[{"request":"R1","batch":"B1","parcel":"P001","result":"失败"}]`), "第 1 条记录"},
		{"失败原因仅空白", strPtr(`[{"request":"R1","batch":"B1","parcel":"P001","result":"失败","reason":"  "}]`), "第 1 条记录"},
		{"签收带原因", strPtr(`[{"request":"R1","batch":"B1","parcel":"P001","result":"签收","reason":"多事"}]`), "第 1 条记录"},
		{"文件内请求号重复", strPtr(`[
  {"request":"R1","batch":"B1","parcel":"P001","result":"签收"},
  {"request":" R1 ","batch":"B1","parcel":"P001","result":"签收"}
]`), "第 2 条记录"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "missing.json")
			if c.content != nil {
				file = writeImportFile(t, "in.json", *c.content)
			}
			out, errText, code := runCLI(t, dbPath, "receipt-import", "--file", file)
			if code != exitBusiness || !strings.Contains(errText, c.errSub) {
				t.Fatalf("应失败(1)且提示 %q: code=%d err=%s", c.errSub, code, errText)
			}
			if out != "" {
				t.Fatalf("失败时不得输出已提交成功结果: %q", out)
			}
			if after := readFileText(t, dbPath); after != before {
				t.Fatal("失败时不得改写台账")
			}
		})
	}

	// 缺少 --file：退出 1。
	if _, _, code := runCLI(t, dbPath, "receipt-import"); code != exitBusiness {
		t.Fatalf("缺少 --file 应以 1 退出: %d", code)
	}
}

func TestCLIReceiptImportAtomicRejection(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A")
	runCLI(t, dbPath, "register", "--id", "P002", "--station", "站点A")
	runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A", "--courier", "张三",
		"--parcel", "P001", "--parcel", "P002")
	before := readFileText(t, dbPath)

	// 第二条批次不存在：整份拒绝，第一条也不生效。
	file := writeImportFile(t, "bad.json", `[
  {"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"},
  {"request": "RC2", "batch": "NOPE", "parcel": "P002", "result": "签收"}
]`)
	out, errText, code := runCLI(t, dbPath, "receipt-import", "--file", file)
	if code != exitBusiness || !strings.Contains(errText, "第 2 条记录") || !strings.Contains(errText, "不存在") {
		t.Fatalf("应整份拒绝并提示位置与原因: code=%d err=%s", code, errText)
	}
	if out != "" {
		t.Fatalf("整份拒绝时不得输出成功结果: %q", out)
	}
	if after := readFileText(t, dbPath); after != before {
		t.Fatal("整份拒绝时不得改写台账")
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if strings.Contains(out, "操作: 回执") || !strings.Contains(out, "当前状态: 配送中") {
		t.Fatalf("整份拒绝时包裹不得变更:\n%s", out)
	}

	// 新请求号未占用：纠正后重试成功。
	file = writeImportFile(t, "fixed.json", `[
  {"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"},
  {"request": "RC2", "batch": "B1", "parcel": "P002", "result": "签收"}
]`)
	out, _, code = runCLI(t, dbPath, "receipt-import", "--file", file)
	if code != 0 || !strings.Contains(out, "新增 2 条") {
		t.Fatalf("纠正后重试应成功: code=%d out=%s", code, out)
	}
}

func TestCLIReceiptImportCorruptLedger(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.json")
	if err := os.WriteFile(dbPath, []byte(`{"version":1,"parcels":{`), 0o644); err != nil {
		t.Fatal(err)
	}
	file := writeImportFile(t, "in.json", `[{"request":"R1","batch":"B1","parcel":"P001","result":"签收"}]`)
	out, errText, code := runCLI(t, dbPath, "receipt-import", "--file", file)
	if code != exitBusiness || !strings.Contains(errText, "损坏") || out != "" {
		t.Fatalf("损坏台账应明确拒绝(1): code=%d out=%q err=%s", code, out, errText)
	}
	// 损坏内容不被覆盖。
	if got := readFileText(t, dbPath); got != `{"version":1,"parcels":{` {
		t.Fatalf("损坏台账不得被覆盖:\n%s", got)
	}
}

func TestCLIReceiptImportHelp(t *testing.T) {
	for _, args := range [][]string{
		{"receipt-import", "--help"},
		{"receipt-import", "-h"},
	} {
		var out, errb strings.Builder
		code := run(args, &out, &errb)
		if code != 0 || !strings.Contains(out.String(), appName) || !strings.Contains(out.String(), "--file") || errb.Len() != 0 {
			t.Fatalf("%v 帮助应以 0 退出且不含 stderr: code=%d", args, code)
		}
	}
}

func strPtr(s string) *string { return &s }
