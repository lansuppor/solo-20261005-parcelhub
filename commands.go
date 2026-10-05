package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// timeFmt 是命令行展示时间的格式（JSON 文件内使用 RFC3339）。
const timeFmt = "2006-01-02 15:04:05 -0700"

// newFlagSet 为子命令准备统一的解析器：错误不自动退出，也不自行打印，
// 全部交由 parseFlags 统一输出，避免重复提示。
func newFlagSet(name string, _ io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// parseFlags 解析子命令参数。第二个返回值是退出码；ok=false 时应直接退出。
func parseFlags(fs *flag.FlagSet, argv []string, stdout, stderr io.Writer, printCmdHelp func(io.Writer)) (ok bool, code int) {
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCmdHelp(stdout)
			return false, exitOK
		}
		fmt.Fprintf(stderr, "%s %s: %v；运行 %s %s --help 查看用法\n", appName, fs.Name(), err, appName, fs.Name())
		return false, exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s %s: 未知参数 %q；运行 %s %s --help 查看用法\n", appName, fs.Name(), fs.Arg(0), appName, fs.Name())
		return false, exitUsage
	}
	return true, 0
}

func cmdRegister(dataFile string, argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("register", stderr)
	fs.StringVar(&dataFile, "data", dataFile, "本地台账数据文件路径")
	id := fs.String("id", "", "包裹编号（不可为空或仅含空白）")
	station := fs.String("station", "", "收件站点（不可为空或仅含空白）")
	if ok, code := parseFlags(fs, argv, stdout, stderr, printRegisterHelp); !ok {
		return code
	}

	cleanIDVal, err := cleanID("包裹编号", *id)
	if err != nil {
		fmt.Fprintf(stderr, "%s register: %v\n", appName, err)
		return exitBusiness
	}
	cleanStation, err := cleanID("收件站点", *station)
	if err != nil {
		fmt.Fprintf(stderr, "%s register: %v\n", appName, err)
		return exitBusiness
	}

	store, err := Open(dataFile)
	if err != nil {
		fmt.Fprintf(stderr, "%s register: %v\n", appName, err)
		return exitBusiness
	}
	p, err := store.Register(cleanIDVal, cleanStation, now())
	if err != nil {
		fmt.Fprintf(stderr, "%s register: %v\n", appName, err)
		return exitBusiness
	}

	fmt.Fprintf(stdout, `收件登记成功
包裹编号: %s
收件站点: %s
当前状态: %s
发生时间: %s
`, p.ID, p.Station, p.Status, p.Registered.Format(timeFmt))
	return exitOK
}

func cmdQuery(dataFile string, argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("query", stderr)
	fs.StringVar(&dataFile, "data", dataFile, "本地台账数据文件路径")
	id := fs.String("id", "", "要查询的包裹编号")
	if ok, code := parseFlags(fs, argv, stdout, stderr, printQueryHelp); !ok {
		return code
	}

	cleanIDVal, err := cleanID("包裹编号", *id)
	if err != nil {
		fmt.Fprintf(stderr, "%s query: %v\n", appName, err)
		return exitBusiness
	}

	store, err := Open(dataFile)
	if err != nil {
		fmt.Fprintf(stderr, "%s query: %v\n", appName, err)
		return exitBusiness
	}
	p, err := store.Query(cleanIDVal)
	if err != nil {
		fmt.Fprintf(stderr, "%s query: %v\n", appName, err)
		return exitBusiness
	}

	fmt.Fprintf(stdout, "包裹编号: %s\n当前站点: %s\n当前状态: %s\n", p.ID, p.Station, p.Status)
	if f := store.ActiveFreeze(cleanIDVal); f != nil {
		fmt.Fprintf(stdout, "当前未解除异常: 异常单号: %s    原因: %s    冻结时间: %s\n",
			f.Incident, f.Reason, f.Time.Format(timeFmt))
	}
	fmt.Fprintf(stdout, "轨迹（按提交顺序，共 %d 条）:\n", len(p.Trail))
	for i, e := range p.Trail {
		switch e.Op {
		case "退回":
			fmt.Fprintf(stdout, "  %d. 操作: 退回    源站: %s    目的站: %s    退回请求号: %s    原交接请求号: %s    原因: %s    时间: %s\n",
				i+1, e.From, e.Station, e.Request, e.RefRequest, e.Reason, e.Time.Format(timeFmt))
		case "出站":
			fmt.Fprintf(stdout, "  %d. 操作: 出站    站点: %s    批次号: %s    配送员: %s    时间: %s\n",
				i+1, e.Station, e.Batch, e.Courier, e.Time.Format(timeFmt))
		case "回执":
			if e.Result == resultFailed {
				fmt.Fprintf(stdout, "  %d. 操作: 回执    站点: %s    批次号: %s    结果: %s    原因: %s    请求号: %s    时间: %s\n",
					i+1, e.Station, e.Batch, e.Result, e.Reason, e.Request, e.Time.Format(timeFmt))
			} else {
				fmt.Fprintf(stdout, "  %d. 操作: 回执    站点: %s    批次号: %s    结果: %s    请求号: %s    时间: %s\n",
					i+1, e.Station, e.Batch, e.Result, e.Request, e.Time.Format(timeFmt))
			}
		case "冻结":
			fmt.Fprintf(stdout, "  %d. 操作: 冻结    站点: %s    异常单号: %s    原因: %s    时间: %s\n",
				i+1, e.Station, e.Incident, e.Reason, e.Time.Format(timeFmt))
		case "解除冻结":
			fmt.Fprintf(stdout, "  %d. 操作: 解除冻结    站点: %s    异常单号: %s    解除请求号: %s    处理说明: %s    时间: %s\n",
				i+1, e.Station, e.Incident, e.Request, e.Note, e.Time.Format(timeFmt))
		default:
			req := e.Request
			if req == "" {
				req = "-"
			}
			fmt.Fprintf(stdout, "  %d. 操作: %s    站点: %s    请求号: %s    时间: %s\n",
				i+1, e.Op, e.Station, req, e.Time.Format(timeFmt))
		}
	}
	return exitOK
}

func cmdHandoff(dataFile string, argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("handoff", stderr)
	fs.StringVar(&dataFile, "data", dataFile, "本地台账数据文件路径")
	request := fs.String("request", "", "交接请求号（用于去重）")
	from := fs.String("from", "", "源站点")
	to := fs.String("to", "", "目的站点")
	var parcels stringList
	fs.Var(&parcels, "parcel", "包裹编号，可重复指定；集合不可为空或含重复编号")
	if ok, code := parseFlags(fs, argv, stdout, stderr, printHandoffHelp); !ok {
		return code
	}

	cleanReq, err := cleanID("请求号", *request)
	if err != nil {
		fmt.Fprintf(stderr, "%s handoff: %v\n", appName, err)
		return exitBusiness
	}
	cleanFrom, err := cleanID("源站点", *from)
	if err != nil {
		fmt.Fprintf(stderr, "%s handoff: %v\n", appName, err)
		return exitBusiness
	}
	cleanTo, err := cleanID("目的站点", *to)
	if err != nil {
		fmt.Fprintf(stderr, "%s handoff: %v\n", appName, err)
		return exitBusiness
	}
	if len(parcels) == 0 {
		fmt.Fprintf(stderr, "%s handoff: 包裹集合不可为空，至少需要一个 --parcel\n", appName)
		return exitBusiness
	}
	cleanParcels := make([]string, 0, len(parcels))
	seen := make(map[string]bool, len(parcels))
	for _, raw := range parcels {
		id, err := cleanID("包裹编号", raw)
		if err != nil {
			fmt.Fprintf(stderr, "%s handoff: %v\n", appName, err)
			return exitBusiness
		}
		if seen[id] {
			fmt.Fprintf(stderr, "%s handoff: 包裹集合中编号 %q 重复\n", appName, id)
			return exitBusiness
		}
		seen[id] = true
		cleanParcels = append(cleanParcels, id)
	}
	if cleanFrom == cleanTo {
		fmt.Fprintf(stderr, "%s handoff: 源站点与目的站点不能相同（均为 %q）\n", appName, cleanFrom)
		return exitBusiness
	}

	store, err := Open(dataFile)
	if err != nil {
		fmt.Fprintf(stderr, "%s handoff: %v\n", appName, err)
		return exitBusiness
	}
	result, replayed, err := store.Handoff(cleanReq, cleanFrom, cleanTo, cleanParcels, now())
	if err != nil {
		fmt.Fprintf(stderr, "%s handoff: %v\n", appName, err)
		return exitBusiness
	}

	headline := "交接成功"
	if replayed {
		headline = "交接成功（请求号重复提交，返回首次保存的结果，未再追加轨迹）"
	}
	fmt.Fprintf(stdout, "%s\n请求号: %s\n源站点: %s\n目的站点: %s\n包裹（%d 件）:\n",
		headline, result.Request, result.From, result.To, len(result.Parcels))
	for _, id := range result.Parcels {
		fmt.Fprintf(stdout, "  - %s\n", id)
	}
	fmt.Fprintf(stdout, "发生时间: %s\n", result.Time.Format(timeFmt))
	return exitOK
}

func cmdReturn(dataFile string, argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("return", stderr)
	fs.StringVar(&dataFile, "data", dataFile, "本地台账数据文件路径")
	request := fs.String("request", "", "退回请求号（用于去重）")
	handoff := fs.String("handoff", "", "被退回的原交接请求号")
	reason := fs.String("reason", "", "退回原因")
	if ok, code := parseFlags(fs, argv, stdout, stderr, printReturnHelp); !ok {
		return code
	}

	cleanReq, err := cleanID("退回请求号", *request)
	if err != nil {
		fmt.Fprintf(stderr, "%s return: %v\n", appName, err)
		return exitBusiness
	}
	cleanHandoff, err := cleanID("原交接请求号", *handoff)
	if err != nil {
		fmt.Fprintf(stderr, "%s return: %v\n", appName, err)
		return exitBusiness
	}
	cleanReason, err := cleanID("退回原因", *reason)
	if err != nil {
		fmt.Fprintf(stderr, "%s return: %v\n", appName, err)
		return exitBusiness
	}

	store, err := Open(dataFile)
	if err != nil {
		fmt.Fprintf(stderr, "%s return: %v\n", appName, err)
		return exitBusiness
	}
	result, replayed, err := store.Return(cleanReq, cleanHandoff, cleanReason, now())
	if err != nil {
		fmt.Fprintf(stderr, "%s return: %v\n", appName, err)
		return exitBusiness
	}

	headline := "退回成功"
	if replayed {
		headline = "退回成功（请求号重复提交，返回首次保存的结果，未再追加轨迹）"
	}
	fmt.Fprintf(stdout, "%s\n退回请求号: %s\n原交接请求号: %s\n源站点: %s\n目的站点: %s\n退回原因: %s\n包裹（%d 件）:\n",
		headline, result.Request, result.Handoff, result.From, result.To, result.Reason, len(result.Parcels))
	for _, id := range result.Parcels {
		fmt.Fprintf(stdout, "  - %s\n", id)
	}
	fmt.Fprintf(stdout, "发生时间: %s\n", result.Time.Format(timeFmt))
	return exitOK
}

// cleanParcelList 清洗并校验可重复的 --parcel 参数：逐个去除两端空白、
// 不可为空、集合不可为空且编号不可重复。
func cleanParcelList(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("包裹集合不可为空，至少需要一个 --parcel")
	}
	clean := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, v := range raw {
		id, err := cleanID("包裹编号", v)
		if err != nil {
			return nil, err
		}
		if seen[id] {
			return nil, fmt.Errorf("包裹集合中编号 %q 重复", id)
		}
		seen[id] = true
		clean = append(clean, id)
	}
	return clean, nil
}

func cmdDispatch(dataFile string, argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("dispatch", stderr)
	fs.StringVar(&dataFile, "data", dataFile, "本地台账数据文件路径")
	batch := fs.String("batch", "", "配送批次号（用于去重，完成后也不能复用）")
	station := fs.String("station", "", "出发站")
	courier := fs.String("courier", "", "配送员")
	var parcels stringList
	fs.Var(&parcels, "parcel", "包裹编号，可重复指定；集合不可为空或含重复编号")
	if ok, code := parseFlags(fs, argv, stdout, stderr, printDispatchHelp); !ok {
		return code
	}

	cleanBatch, err := cleanID("批次号", *batch)
	if err != nil {
		fmt.Fprintf(stderr, "%s dispatch: %v\n", appName, err)
		return exitBusiness
	}
	cleanStation, err := cleanID("出发站", *station)
	if err != nil {
		fmt.Fprintf(stderr, "%s dispatch: %v\n", appName, err)
		return exitBusiness
	}
	cleanCourier, err := cleanID("配送员", *courier)
	if err != nil {
		fmt.Fprintf(stderr, "%s dispatch: %v\n", appName, err)
		return exitBusiness
	}
	cleanParcels, err := cleanParcelList(parcels)
	if err != nil {
		fmt.Fprintf(stderr, "%s dispatch: %v\n", appName, err)
		return exitBusiness
	}

	store, err := Open(dataFile)
	if err != nil {
		fmt.Fprintf(stderr, "%s dispatch: %v\n", appName, err)
		return exitBusiness
	}
	result, replayed, err := store.Dispatch(cleanBatch, cleanStation, cleanCourier, cleanParcels, now())
	if err != nil {
		fmt.Fprintf(stderr, "%s dispatch: %v\n", appName, err)
		return exitBusiness
	}

	headline := "出站成功"
	if replayed {
		headline = "出站成功（批次号重复提交，返回首次保存的结果，未再追加轨迹）"
	}
	fmt.Fprintf(stdout, "%s\n批次号: %s\n出发站: %s\n配送员: %s\n包裹（%d 件）:\n",
		headline, result.Batch, result.Station, result.Courier, len(result.Parcels))
	for _, id := range result.Parcels {
		fmt.Fprintf(stdout, "  - %s\n", id)
	}
	fmt.Fprintf(stdout, "发生时间: %s\n", result.Time.Format(timeFmt))
	return exitOK
}

func cmdReceipt(dataFile string, argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("receipt", stderr)
	fs.StringVar(&dataFile, "data", dataFile, "本地台账数据文件路径")
	request := fs.String("request", "", "回执请求号（用于去重）")
	batch := fs.String("batch", "", "配送批次号")
	parcel := fs.String("parcel", "", "包裹编号")
	result := fs.String("result", "", "回执结果：签收 或 失败")
	reason := fs.String("reason", "", "失败原因（结果为失败时必填，签收时不可带）")
	if ok, code := parseFlags(fs, argv, stdout, stderr, printReceiptHelp); !ok {
		return code
	}

	cleanReq, err := cleanID("回执请求号", *request)
	if err != nil {
		fmt.Fprintf(stderr, "%s receipt: %v\n", appName, err)
		return exitBusiness
	}
	cleanBatch, err := cleanID("批次号", *batch)
	if err != nil {
		fmt.Fprintf(stderr, "%s receipt: %v\n", appName, err)
		return exitBusiness
	}
	cleanParcel, err := cleanID("包裹编号", *parcel)
	if err != nil {
		fmt.Fprintf(stderr, "%s receipt: %v\n", appName, err)
		return exitBusiness
	}
	cleanResult, err := cleanID("回执结果", *result)
	if err != nil {
		fmt.Fprintf(stderr, "%s receipt: %v\n", appName, err)
		return exitBusiness
	}
	if cleanResult != resultSigned && cleanResult != resultFailed {
		fmt.Fprintf(stderr, "%s receipt: 回执结果只能是 %q 或 %q，得到 %q\n", appName, resultSigned, resultFailed, cleanResult)
		return exitBusiness
	}
	cleanReason := strings.TrimSpace(*reason)
	if cleanResult == resultFailed && cleanReason == "" {
		fmt.Fprintf(stderr, "%s receipt: 结果为失败时原因不可为空或仅含空白\n", appName)
		return exitBusiness
	}
	if cleanResult == resultSigned && cleanReason != "" {
		fmt.Fprintf(stderr, "%s receipt: 结果为签收时不可带原因\n", appName)
		return exitBusiness
	}

	store, err := Open(dataFile)
	if err != nil {
		fmt.Fprintf(stderr, "%s receipt: %v\n", appName, err)
		return exitBusiness
	}
	res, replayed, err := store.Receipt(cleanReq, cleanBatch, cleanParcel, cleanResult, cleanReason, now())
	if err != nil {
		fmt.Fprintf(stderr, "%s receipt: %v\n", appName, err)
		return exitBusiness
	}

	headline := "回执成功"
	if replayed {
		headline = "回执成功（请求号重复提交，返回首次保存的结果，未再追加轨迹）"
	}
	fmt.Fprintf(stdout, "%s\n请求号: %s\n批次号: %s\n包裹编号: %s\n结果: %s\n",
		headline, res.Request, res.Batch, res.Parcel, res.Result)
	if res.Result == resultFailed {
		fmt.Fprintf(stdout, "原因: %s\n", res.Reason)
	}
	fmt.Fprintf(stdout, "发生时间: %s\n", res.Time.Format(timeFmt))
	return exitOK
}

// receiptFileRecord 是回执整批导入文件中的单条记录（JSON）。
type receiptFileRecord struct {
	Request string `json:"request"`
	Batch   string `json:"batch"`
	Parcel  string `json:"parcel"`
	Result  string `json:"result"`
	Reason  string `json:"reason"`
}

// loadReceiptImportFile 读取并解析回执整批导入文件。文件为有序且非空的
// JSON 数组，元素为 request/batch/parcel/result/reason 字段的对象；
// 文件只读，绝不改写。任一记录清洗后不合法或请求号在文件内重复，
// 都返回带记录位置的错误，整份不予导入。
func loadReceiptImportFile(path string) ([]ReceiptImportRecord, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取回执导入文件失败: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var records []receiptFileRecord
	if err := dec.Decode(&records); err != nil {
		return nil, fmt.Errorf("解析回执导入文件失败: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("解析回执导入文件失败: 回执列表之后存在多余内容")
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("回执导入文件必须是有序且非空的回执列表")
	}

	clean := make([]ReceiptImportRecord, 0, len(records))
	seen := make(map[string]bool, len(records))
	for i, rawRec := range records {
		rec, err := cleanReceiptImportRecord(rawRec)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条记录: %w", i+1, err)
		}
		if seen[rec.Request] {
			return nil, fmt.Errorf("第 %d 条记录: 回执请求号 %q 在文件内重复", i+1, rec.Request)
		}
		seen[rec.Request] = true
		clean = append(clean, rec)
	}
	return clean, nil
}

// cleanReceiptImportRecord 清洗并校验一条导入记录：各标识去除两端空白后不可为空；
// 结果为失败时必须给出清理后非空的原因，签收时不可带原因。
func cleanReceiptImportRecord(raw receiptFileRecord) (ReceiptImportRecord, error) {
	var rec ReceiptImportRecord
	var err error
	if rec.Request, err = cleanID("回执请求号", raw.Request); err != nil {
		return rec, err
	}
	if rec.Batch, err = cleanID("批次号", raw.Batch); err != nil {
		return rec, err
	}
	if rec.Parcel, err = cleanID("包裹编号", raw.Parcel); err != nil {
		return rec, err
	}
	if rec.Result, err = cleanID("回执结果", raw.Result); err != nil {
		return rec, err
	}
	if rec.Result != resultSigned && rec.Result != resultFailed {
		return rec, fmt.Errorf("回执结果只能是 %q 或 %q，得到 %q", resultSigned, resultFailed, rec.Result)
	}
	rec.Reason = strings.TrimSpace(raw.Reason)
	if rec.Result == resultFailed && rec.Reason == "" {
		return rec, fmt.Errorf("结果为失败时原因不可为空或仅含空白")
	}
	if rec.Result == resultSigned && rec.Reason != "" {
		return rec, fmt.Errorf("结果为签收时不可带原因")
	}
	return rec, nil
}

func cmdReceiptImport(dataFile string, argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("receipt-import", stderr)
	fs.StringVar(&dataFile, "data", dataFile, "本地台账数据文件路径")
	file := fs.String("file", "", "回执导入文件路径（JSON 数组，只读，不会被修改）")
	if ok, code := parseFlags(fs, argv, stdout, stderr, printReceiptImportHelp); !ok {
		return code
	}

	if strings.TrimSpace(*file) == "" {
		fmt.Fprintf(stderr, "%s receipt-import: 选项 --file 需要一个非空的回执导入文件路径\n", appName)
		return exitBusiness
	}

	// 输入文件只读；读取、解析或清洗失败时整份不予导入。
	records, err := loadReceiptImportFile(*file)
	if err != nil {
		fmt.Fprintf(stderr, "%s receipt-import: %v\n", appName, err)
		return exitBusiness
	}

	store, err := Open(dataFile)
	if err != nil {
		fmt.Fprintf(stderr, "%s receipt-import: %v\n", appName, err)
		return exitBusiness
	}
	items, err := store.ImportReceipts(records, now())
	if err != nil {
		fmt.Fprintf(stderr, "%s receipt-import: %v\n", appName, err)
		return exitBusiness
	}

	replayed := 0
	for _, it := range items {
		if it.Replayed {
			replayed++
		}
	}
	fmt.Fprintf(stdout, "回执导入成功（共 %d 条：新增 %d 条，重放 %d 条）\n", len(items), len(items)-replayed, replayed)
	for i, it := range items {
		mark := "新增"
		if it.Replayed {
			mark = "重放（返回首次保存的结果）"
		}
		line := fmt.Sprintf("  %d. 请求号: %s    批次号: %s    包裹编号: %s    结果: %s",
			i+1, it.Record.Request, it.Record.Batch, it.Record.Parcel, it.Record.Result)
		if it.Record.Result == resultFailed {
			line += fmt.Sprintf("    原因: %s", it.Record.Reason)
		}
		fmt.Fprintf(stdout, "%s    标记: %s    时间: %s\n", line, mark, it.Time.Format(timeFmt))
	}
	return exitOK
}

func cmdBatch(dataFile string, argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("batch", stderr)
	fs.StringVar(&dataFile, "data", dataFile, "本地台账数据文件路径")
	id := fs.String("id", "", "要查询的配送批次号")
	if ok, code := parseFlags(fs, argv, stdout, stderr, printBatchHelp); !ok {
		return code
	}

	cleanIDVal, err := cleanID("批次号", *id)
	if err != nil {
		fmt.Fprintf(stderr, "%s batch: %v\n", appName, err)
		return exitBusiness
	}

	store, err := Open(dataFile)
	if err != nil {
		fmt.Fprintf(stderr, "%s batch: %v\n", appName, err)
		return exitBusiness
	}
	b, err := store.BatchQuery(cleanIDVal)
	if err != nil {
		fmt.Fprintf(stderr, "%s batch: %v\n", appName, err)
		return exitBusiness
	}

	status := "配送中"
	if b.Done() {
		status = "已完成"
	}
	fmt.Fprintf(stdout, "批次号: %s\n出发站: %s\n配送员: %s\n出站时间: %s\n批次状态: %s\n逐件回执（%d/%d 已回执）:\n",
		b.Batch, b.Station, b.Courier, b.Time.Format(timeFmt), status, len(b.Receipts), len(b.Parcels))
	for _, pid := range b.Parcels {
		e, ok := b.Receipts[pid]
		if !ok {
			fmt.Fprintf(stdout, "  - %s    未回执\n", pid)
			continue
		}
		if e.Result == resultFailed {
			fmt.Fprintf(stdout, "  - %s    已回执    结果: %s    原因: %s    请求号: %s    时间: %s\n",
				pid, e.Result, e.Reason, e.Request, e.Time.Format(timeFmt))
		} else {
			fmt.Fprintf(stdout, "  - %s    已回执    结果: %s    请求号: %s    时间: %s\n",
				pid, e.Result, e.Request, e.Time.Format(timeFmt))
		}
	}
	return exitOK
}

func cmdFreeze(dataFile string, argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("freeze", stderr)
	fs.StringVar(&dataFile, "data", dataFile, "本地台账数据文件路径")
	incident := fs.String("incident", "", "异常单号（标识一次异常，解除后也不能复用）")
	parcel := fs.String("parcel", "", "要冻结的包裹编号")
	station := fs.String("station", "", "包裹所在站点")
	reason := fs.String("reason", "", "异常原因")
	if ok, code := parseFlags(fs, argv, stdout, stderr, printFreezeHelp); !ok {
		return code
	}

	cleanIncident, err := cleanID("异常单号", *incident)
	if err != nil {
		fmt.Fprintf(stderr, "%s freeze: %v\n", appName, err)
		return exitBusiness
	}
	cleanParcel, err := cleanID("包裹编号", *parcel)
	if err != nil {
		fmt.Fprintf(stderr, "%s freeze: %v\n", appName, err)
		return exitBusiness
	}
	cleanStation, err := cleanID("所在站点", *station)
	if err != nil {
		fmt.Fprintf(stderr, "%s freeze: %v\n", appName, err)
		return exitBusiness
	}
	cleanReason, err := cleanID("异常原因", *reason)
	if err != nil {
		fmt.Fprintf(stderr, "%s freeze: %v\n", appName, err)
		return exitBusiness
	}

	store, err := Open(dataFile)
	if err != nil {
		fmt.Fprintf(stderr, "%s freeze: %v\n", appName, err)
		return exitBusiness
	}
	res, replayed, err := store.Freeze(cleanIncident, cleanParcel, cleanStation, cleanReason, now())
	if err != nil {
		fmt.Fprintf(stderr, "%s freeze: %v\n", appName, err)
		return exitBusiness
	}

	headline := "冻结成功"
	if replayed {
		headline = "冻结成功（异常单号重复提交，返回首次保存的结果，未再追加冻结记录）"
	}
	cur, _ := store.Query(cleanParcel)
	fmt.Fprintf(stdout, "%s\n异常单号: %s\n包裹编号: %s\n所在站点: %s\n当前状态: %s\n异常原因: %s\n发生时间: %s\n",
		headline, res.Incident, res.Parcel, res.Station, cur.Status, res.Reason, res.Time.Format(timeFmt))
	return exitOK
}

func cmdUnfreeze(dataFile string, argv []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("unfreeze", stderr)
	fs.StringVar(&dataFile, "data", dataFile, "本地台账数据文件路径")
	request := fs.String("request", "", "解除请求号（独立去重）")
	incident := fs.String("incident", "", "要解除的异常单号")
	note := fs.String("note", "", "处理说明")
	if ok, code := parseFlags(fs, argv, stdout, stderr, printUnfreezeHelp); !ok {
		return code
	}

	cleanReq, err := cleanID("解除请求号", *request)
	if err != nil {
		fmt.Fprintf(stderr, "%s unfreeze: %v\n", appName, err)
		return exitBusiness
	}
	cleanIncident, err := cleanID("异常单号", *incident)
	if err != nil {
		fmt.Fprintf(stderr, "%s unfreeze: %v\n", appName, err)
		return exitBusiness
	}
	cleanNote, err := cleanID("处理说明", *note)
	if err != nil {
		fmt.Fprintf(stderr, "%s unfreeze: %v\n", appName, err)
		return exitBusiness
	}

	store, err := Open(dataFile)
	if err != nil {
		fmt.Fprintf(stderr, "%s unfreeze: %v\n", appName, err)
		return exitBusiness
	}
	res, replayed, err := store.Unfreeze(cleanReq, cleanIncident, cleanNote, now())
	if err != nil {
		fmt.Fprintf(stderr, "%s unfreeze: %v\n", appName, err)
		return exitBusiness
	}

	headline := "解除成功"
	if replayed {
		headline = "解除成功（解除请求号重复提交，返回首次保存的结果，未再追加解除记录）"
	}
	cur, _ := store.Query(res.Parcel)
	fmt.Fprintf(stdout, "%s\n解除请求号: %s\n异常单号: %s\n包裹编号: %s\n当前状态: %s\n处理说明: %s\n发生时间: %s\n",
		headline, res.Request, res.Incident, res.Parcel, cur.Status, res.Note, res.Time.Format(timeFmt))
	return exitOK
}
