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
	case "return":
		return cmdReturn(dataFile, rest[1:], stdout, stderr)
	case "dispatch":
		return cmdDispatch(dataFile, rest[1:], stdout, stderr)
	case "receipt":
		return cmdReceipt(dataFile, rest[1:], stdout, stderr)
	case "batch":
		return cmdBatch(dataFile, rest[1:], stdout, stderr)
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
  return                 按原交接整批退回，实物送回该交接的源站点
  dispatch               配送批次出站：整批包裹转为“配送中”，批次成员保存后不可修改
  receipt                逐件回执：签收或失败（失败须给原因）；全部回执后批次自动完成
  batch                  按批次号查询成员、配送员、出站时间与逐件回执进度

常用示例:
  %s register --id P001 --station 站点A
  %s query    --id P001
  %s handoff  --request R1 --from 站点A --to 站点B \
              --parcel P001 --parcel P002
  %s return   --request RT1 --handoff R1 --reason 错发站点
  %s dispatch --batch B1 --station 站点A --courier 张三 \
              --parcel P001 --parcel P002
  %s receipt  --request RC1 --batch B1 --parcel P001 --result 签收
  %s receipt  --request RC2 --batch B1 --parcel P002 --result 失败 --reason 收件人不在
  %s batch    --id B1

无参数、-h 或 --help 显示本帮助。业务校验失败以状态码 1 退出；
未知命令或参数提示于标准错误并以状态码 2 退出。
`, appName, appVersion, appName, defaultDB, appName, appName, appName, appName, appName, appName, appName, appName)
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
交接记录同时显示请求号；退回记录同时显示源站、目的站、
退回请求号、被退回的原交接请求号与原因；出站记录同时显示
批次号与配送员；回执记录同时显示批次号、结果、原因（失败时）
与请求号。包裹不存在时报错，不会创建记录。

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

func printReturnHelp(w io.Writer) {
	fmt.Fprintf(w, `%s return — 按原交接整批退回（实物送回该交接的源站点）

用法:
  %s return [--data FILE] --request 退回请求号 --handoff 原交接请求号 --reason 退回原因

规则:
  - 退回请求号、原交接请求号、退回原因均去除两端空白，不可为空或仅含空白
  - 批次与站点取自原交接结果，不允许另选包裹或目的站
  - 首次退回：原交接必须存在且尚未成功退回；批次中每件包裹必须仍在
    原交接目的站、状态为在站，且最后一条流转记录就是该原交接
    （包裹经其他交接又回到同一站点的，不能退回旧交接；请求重放不算新流转）
  - 任一条件不满足则整批拒绝，其他包裹与已有请求结果不变
  - 成功时整批改归原交接源站（仍为“在站”），每件按提交顺序追加一条
    退回记录，原收件、交接轨迹保留不变；每个原交接只能成功退回一次
  - 退回请求号与交接请求号分属独立去重范围，允许同名：
    相同退回请求号且原交接、原因相同，直接返回首次结果，不再追加轨迹，
    即使包裹之后又被交接也照常重放；请求号相同但原交接或原因不同报冲突；
    失败的首次退回不占用请求号

示例:
  %s return --request RT1 --handoff R1 --reason 错发站点
`, appName, appName, appName)
}

func printDispatchHelp(w io.Writer) {
	fmt.Fprintf(w, `%s dispatch — 配送批次出站

用法:
  %s dispatch [--data FILE] --batch 批次号 --station 出发站 --courier 配送员 \
              --parcel 包裹编号 [--parcel 包裹编号 ...]

规则:
  - 批次号、出发站、配送员及每个包裹编号均去除两端空白，不可为空或仅含空白
  - 包裹集合不可为空且编号不可重复
  - 首次出站：每件包裹必须已登记、在出发站且状态为在站；
    任一不满足则整批拒绝，其他包裹的状态与轨迹均不变
  - 全部满足时整批转为“配送中”（站点仍记出发站），每件追加一条
    含批次、配送员和时间的出站记录；批次成员按首次提交顺序保存，不可修改
  - 批次号标识一次配送，完成后也不能复用：相同批次号且站点、配送员、
    包裹集合相同（集合顺序无关）直接返回首次结果及时间，不再追加轨迹；
    批次号相同但内容不同报冲突；失败的出站不占用批次号

示例:
  %s dispatch --batch B1 --station 站点A --courier 张三 --parcel P001 --parcel P002
`, appName, appName, appName)
}

func printReceiptHelp(w io.Writer) {
	fmt.Fprintf(w, `%s receipt — 逐件回执（签收或失败）

用法:
  %s receipt [--data FILE] --request 请求号 --batch 批次号 --parcel 包裹编号 \
             --result 签收|失败 [--reason 失败原因]

规则:
  - 请求号、批次号、包裹编号均去除两端空白，不可为空或仅含空白
  - 结果为“失败”时必须给出清理后非空的原因；结果为“签收”时不可带原因
  - 首次回执：包裹必须属于该批次、尚未在该批次回执，且当前仍在该批次配送中
  - 签收后状态为“已签收”；失败表示实物已回到出发站，恢复“在站”，
    可加入新的配送批次；两种结果都保留站点并追加含批次、结果、原因
    （失败时）、请求号和时间的回执记录
  - 每件包裹在一个批次只能成功回执一次，换请求号再次回执也拒绝；
    全部成员回执后批次自动完成
  - 回执请求号单独去重，可与批次号、包裹号、交接或退回请求号同名：
    相同请求号且批次、包裹、结果、清理后的原因相同，直接返回首次结果及时间，
    不改变状态、轨迹或批次进度，也不检查当前状态（即使失败包裹已进入新批次）；
    请求号相同但内容不同报冲突；失败的提交不占用请求号

示例:
  %s receipt --request RC1 --batch B1 --parcel P001 --result 签收
  %s receipt --request RC2 --batch B1 --parcel P002 --result 失败 --reason 收件人不在
`, appName, appName, appName, appName)
}

func printBatchHelp(w io.Writer) {
	fmt.Fprintf(w, `%s batch — 按批次号查询配送批次

用法:
  %s batch [--data FILE] --id 批次号

展示批次原成员（按首次提交顺序）、配送员、出发站、出站时间、
逐件回执与未回执项，以及批次是否完成。批次不存在时报错。

示例:
  %s batch --id B1
`, appName, appName, appName)
}
