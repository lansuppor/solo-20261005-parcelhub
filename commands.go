package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
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

	fmt.Fprintf(stdout, "包裹编号: %s\n当前站点: %s\n当前状态: %s\n轨迹（按提交顺序，共 %d 条）:\n",
		p.ID, p.Station, p.Status, len(p.Trail))
	for i, e := range p.Trail {
		req := e.Request
		if req == "" {
			req = "-"
		}
		fmt.Fprintf(stdout, "  %d. 操作: %s    站点: %s    请求号: %s    时间: %s\n",
			i+1, e.Op, e.Station, req, e.Time.Format(timeFmt))
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
