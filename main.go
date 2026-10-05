package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	appName    = "parcelhub"
	appVersion = "1.0.0"
	defaultDB  = "parcelhub-ledger.json"
)

// exitCode 约定：
//
//	0 成功
//	1 业务校验失败或数据文件读写失败（原有有效数据保持不变）
//	2 未知命令或未知参数
const (
	exitOK       = 0
	exitBusiness = 1
	exitUsage    = 2
)

// stringList 收集可重复出现的字符串参数，如 handoff 的 --parcel。
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	// 允许全局参数出现在子命令之前：parcelhub --data FILE <command> ...
	dataFile := defaultDB
	rest := args
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		a := rest[0]
		switch {
		case a == "-h" || a == "--help":
			printHelp(stdout)
			return exitOK
		case a == "--data":
			if len(rest) < 2 {
				fmt.Fprintf(stderr, "%s: 选项 --data 需要一个参数\n", appName)
				return exitUsage
			}
			dataFile = rest[1]
			rest = rest[2:]
		case strings.HasPrefix(a, "--data="):
			dataFile = strings.TrimPrefix(a, "--data=")
			rest = rest[1:]
		default:
			fmt.Fprintf(stderr, "%s: 未知参数 %s；运行 %s --help 查看帮助\n", appName, a, appName)
			return exitUsage
		}
	}

	if len(rest) == 0 {
		printHelp(stdout)
		return exitOK
	}

	cmd := rest[0]
	switch cmd {
	case "-h", "--help", "help":
		printHelp(stdout)
		return exitOK
	case "register":
		return cmdRegister(dataFile, rest[1:], stdout, stderr)
	case "query":
		return cmdQuery(dataFile, rest[1:], stdout, stderr)
	case "handoff":
		return cmdHandoff(dataFile, rest[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "%s: 未知命令 %q；运行 %s --help 查看可用命令\n", appName, cmd, appName)
		return exitUsage
	}
}

// cleanID 去除标识两端空白；仅含空白时返回错误（由调用方给出字段名）。
func cleanID(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("%s不可为空或仅含空白", field)
	}
	return v, nil
}

func now() time.Time { return time.Now() }

func printHelp(w io.Writer) {
	fmt.Fprintf(w, `%s %s — 包裹站点交接台账（本地命令行，无外部服务）

用法:
  %s [--data FILE] <命令> [选项]

全局选项:
  --data FILE            本地台账数据文件（默认 %s；不存在时自动建立，已有内容损坏则拒绝读写）

命令:
  register               单件收件登记，登记后包裹状态为“在站”
  query                  按包裹编号查询当前站点、状态与完整轨迹
  handoff                一次提交多件包裹的站点交接；成功后包裹直接归属目的站点

常用示例:
  %s register --id P001 --station 站点A
  %s query    --id P001
  %s handoff  --request R1 --from 站点A --to 站点B \
              --parcel P001 --parcel P002

无参数、-h 或 --help 显示本帮助。业务校验失败以状态码 1 退出；
未知命令或参数提示于标准错误并以状态码 2 退出。
`, appName, appVersion, appName, defaultDB, appName, appName, appName)
}

func printRegisterHelp(w io.Writer) {
	fmt.Fprintf(w, `%s register — 单件收件登记

用法:
  %s register [--data FILE] --id 包裹编号 --station 收件站点

规则:
  - 包裹编号与收件站点均不可为空或仅含空白
  - 包裹编号在同一数据文件内唯一；重复登记报错且不覆盖原记录
  - 成功后状态为“在站”，并产生一条收件记录

示例:
  %s register --id P001 --station 站点A
`, appName, appName, appName)
}

func printQueryHelp(w io.Writer) {
	fmt.Fprintf(w, `%s query — 按包裹编号查询

用法:
  %s query [--data FILE] --id 包裹编号

展示当前站点、当前状态及按提交顺序排列的完整轨迹；
交接记录同时显示请求号。包裹不存在时报错，不会创建记录。

示例:
  %s query --id P001
`, appName, appName, appName)
}

func printHandoffHelp(w io.Writer) {
	fmt.Fprintf(w, `%s handoff — 多件包裹站点交接

用法:
  %s handoff [--data FILE] --request 请求号 --from 源站点 --to 目的站点 \
             --parcel 包裹编号 [--parcel 包裹编号 ...]

规则:
  - 请求号、源站点、目的站点及每个包裹编号均不可为空或仅含空白
  - 包裹集合不可为空且编号不可重复；源站点与目的站点不能相同
  - 首次提交：每件包裹必须已登记且当前归属源站点；
    任一不满足则整次交接失败，其他包裹的归属与轨迹均不变
  - 全部满足时一起改归目的站点（仍为“在站”），各追加一条交接记录
  - 请求号用于交接去重：相同请求号且源、目的、包裹集合相同
    （集合顺序无关）直接返回首次结果，不再追加轨迹；
    请求号相同但业务内容不同则报冲突；失败的提交不占用请求号

示例:
  %s handoff --request R1 --from 站点A --to 站点B --parcel P001 --parcel P002
`, appName, appName, appName)
}
