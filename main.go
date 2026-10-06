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
	case "receipt-import":
		return cmdReceiptImport(dataFile, rest[1:], stdout, stderr)
	case "receipt-revoke":
		return cmdReceiptRevoke(dataFile, rest[1:], stdout, stderr)
	case "batch":
		return cmdBatch(dataFile, rest[1:], stdout, stderr)
	case "freeze":
		return cmdFreeze(dataFile, rest[1:], stdout, stderr)
	case "unfreeze":
		return cmdUnfreeze(dataFile, rest[1:], stdout, stderr)
	case "abort":
		return cmdAbort(dataFile, rest[1:], stdout, stderr)
	case "relay":
		return cmdRelay(dataFile, rest[1:], stdout, stderr)
	case "ship":
		return cmdShip(dataFile, rest[1:], stdout, stderr)
	case "receive":
		return cmdReceive(dataFile, rest[1:], stdout, stderr)
	case "shipment":
		return cmdShipment(dataFile, rest[1:], stdout, stderr)
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
  receipt-import         从本地文件整批导入回执：全部可接受才整体生效，任一不符整份拒绝
  receipt-revoke         撤销误录回执：包裹实际仍由原配送员配送，该件恢复原批次配送中
  batch                  按批次号查询成员、配送员、出站时间与逐件回执进度
  freeze                 在站包裹异常冻结：禁止交接、配送出站与整批退回
  unfreeze               解除异常冻结：原站恢复在站；异常单永久标记为已解除
  abort                  配送批次中止：未回执包裹全部收回出发站，批次永久关闭
  relay                  配送途中整批续接：原批次未回执包裹交给另一配送员，新建批次继续配送
  ship                   站间发运：整单包裹离开源站、转为“站间在途”，归属站点暂记源站
  receive                整单到站接收：运输单全部成员改归目的站、恢复在站，运输单永久标记已接收
  shipment               按运输单号查询两站、原成员、待接收或已接收状态、发运与接收信息

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
  %s receipt-import --file receipts.json
  %s receipt-revoke --request RV1 --receipt RC2 --reason 误录失败，实际仍配送中
  %s batch    --id B1
  %s freeze   --incident E1 --parcel P001 --station 站点A --reason 外包装破损
  %s unfreeze --request U1 --incident E1 --note 已核实放行
  %s abort    --request A1 --batch B1 --reason 车辆故障全部收回
  %s relay    --request T1 --from B1 --to B2 --courier 李四 --reason 原配送员车辆故障
  %s ship     --shipment S1 --from 站点A --to 站点B \
              --parcel P001 --parcel P002
  %s receive  --request RS1 --shipment S1 --station 站点B
  %s shipment --id S1

无参数、-h 或 --help 显示本帮助。业务校验失败以状态码 1 退出；
未知命令或参数提示于标准错误并以状态码 2 退出。

并行使用: 多个终端可同时对同一台账作业。修改类命令自动取得该台账的
进程间排他锁（争用时等待），在锁内读取最新已提交数据后完成受理、去重
与整次原子保存，并行效果等同于某个逐次执行顺序；query、batch 每次读取
一份完整已提交台账，不加锁也不改写数据文件。锁随进程结束（含被强制
终止）自动释放，无需人工删除协调文件（<数据文件>.lock）。
`, appName, appVersion, appName, defaultDB, appName, appName, appName, appName, appName, appName, appName, appName, appName, appName, appName, appName, appName, appName, appName, appName, appName)
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

展示当前站点、当前状态、当前未解除异常（若有）及按提交顺序排列的完整轨迹；
配送中包裹同时展示当前批次与当前配送员；站间在途包裹同时展示当前运输单号
与目的站。
交接记录同时显示请求号；退回记录同时显示源站、目的站、
退回请求号、被退回的原交接请求号与原因；出站记录同时显示
批次号与配送员；回执记录同时显示批次号、结果、原因（失败时）
与请求号；冻结记录显示异常单号、原因、站点和时间；解除冻结记录
显示异常单号、解除请求号、处理说明和时间；收回记录显示批次号、
中止请求号、原因、站点和时间；续接记录显示原批次号、新批次号、
新配送员、续接请求号、原因、站点和时间；撤销回执记录显示批次号、
撤销请求号、原回执请求号、原因、站点和时间，已被撤销的回执记录
同时标注撤销状态；发运记录显示运输单号、源站、目的站和时间；
接收记录显示运输单号、接收请求号、源站、目的站和时间。
包裹不存在时报错，不会创建记录。

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
  - 每件包裹在一个批次最多一条未撤销回执，换请求号再次回执也拒绝；
    已撤销的回执不再占用名额，可用新请求号重新回执（见 receipt-revoke）；
    全部成员均有未撤销回执后批次自动完成
  - 回执请求号单独去重，可与批次号、包裹号、交接或退回请求号同名：
    相同请求号且批次、包裹、结果、清理后的原因相同，直接返回首次结果及时间，
    不改变状态、轨迹或批次进度，也不检查当前状态（即使失败包裹已进入新批次）；
    对已撤销回执的同内容重放仍返回原结果和首次时间并标明已撤销，
    不恢复回执或改变当前状态；请求号相同但内容不同报冲突；失败的提交不占用请求号

示例:
  %s receipt --request RC1 --batch B1 --parcel P001 --result 签收
  %s receipt --request RC2 --batch B1 --parcel P002 --result 失败 --reason 收件人不在
`, appName, appName, appName, appName)
}

func printReceiptImportHelp(w io.Writer) {
	fmt.Fprintf(w, `%s receipt-import — 从本地文件整批导入配送回执

用法:
  %s receipt-import [--data FILE] --file 回执导入文件

文件格式（JSON，有序且非空的数组，按文件顺序处理；文件只读，不会被修改）:
  [
    {"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"},
    {"request": "RC2", "batch": "B1", "parcel": "P002", "result": "失败", "reason": "收件人不在"}
  ]

规则:
  - 每条记录的请求号、批次号、包裹编号、结果去除两端空白后不可为空；
    结果为“失败”时必须给出清理后非空的原因，结果为“签收”时不可带原因
  - 文件内请求号清理后不可重复；文件路径或文件名不作为去重身份，
    相同内容换文件或重排记录不会再次生效
  - 与逐件 receipt 共用同一回执请求号去重范围（与其他业务编号独立）：
    同请求号且批次、包裹、结果、清理后的原因相同的记录返回首次结果与时间，
    不检查当前状态、不追加轨迹、不改变批次进度；对已撤销回执的同内容重放
    仍返回原结果和首次时间并标明已撤销，不恢复回执；换内容报冲突
  - 首次回执要求批次存在、包裹属于该批次、在该批次没有未撤销的回执，
    且当前仍在该批次配送中；每件在一个批次最多一条未撤销回执，
    同文件换请求号再次回执同一批次同一包裹也会整份拒绝
  - 整份导入只有全部记录可接受并整体保存后才报告成功，按文件顺序列出
    各条结果、发生时间及新增或重放标记；任一记录无效、冲突或受理条件
    不符，整份拒绝并提示记录位置与原因，新请求号均不占用，可纠正后重试；
    全部为历史重放时不改写台账

示例:
  %s receipt-import --file receipts.json
`, appName, appName, appName)
}

func printReceiptRevokeHelp(w io.Writer) {
	fmt.Fprintf(w, `%s receipt-revoke — 撤销误录的配送回执（包裹实际仍由原配送员配送，不收回实物）

用法:
  %s receipt-revoke [--data FILE] --request 撤销请求号 --receipt 原回执请求号 --reason 撤销原因

规则:
  - 撤销请求号、原回执请求号、撤销原因均去除两端空白，不可为空或仅含空白
  - 包裹与批次取自原回执，不可另选
  - 首次撤销：原回执必须存在、未撤销且仍为该件在该批次的有效回执；
    所属批次未中止、未转交；原回执必须是该包裹最后一条轨迹
    （其后发生过流转、冻结或解除均拒绝，其他成员的后续操作不阻止撤销）；
    包裹仍在原出发站且状态与回执结果一致
  - 成功后该件恢复原批次配送中，站点、配送员不变，该回执不再计入有效回执
    （原本已完成的批次恢复配送中，原成员顺序与出站或接手时间保留）；
    原回执结果与轨迹永久保留并标记已撤销，追加一条撤销回执记录，
    回执请求号不删除也不释放
  - 每件同批次最多一条未撤销回执：撤销后可用新回执请求号再次提交签收或失败，
    也可随原批次中止或续接；撤销不恢复配送前旧交接的退回资格
  - 撤销请求号独立去重，可与批次号、包裹号、回执请求号及其他业务编号同名：
    相同请求号且原回执、清理后的原因相同，直接返回首次撤销信息和时间，
    不检查现状、不再次改变进度或改写台账（后续重新回执、中止或续接后仍成立）；
    请求号相同但内容不同报冲突；失败的首次撤销不占用请求号
  - 每条原回执只能撤销一次，换撤销号再次撤销它拒绝

示例:
  %s receipt-revoke --request RV1 --receipt RC2 --reason 误录失败，实际仍配送中
`, appName, appName, appName)
}

func printBatchHelp(w io.Writer) {
	fmt.Fprintf(w, `%s batch — 按批次号查询配送批次

用法:
  %s batch [--data FILE] --id 批次号

展示批次原成员（按首次提交顺序）、配送员、出发站、出站时间、
逐件有效回执（已撤销的不计入进度）与未回执项，以及批次状态
（配送中 / 已完成 / 已中止 / 已转交）。批次发生过回执撤销时，
被撤销件标注“未回执（原回执已撤销）”，并另列撤销记录
（撤销请求号、原回执请求号、包裹、批次、原因、时间）。
已中止批次同时展示中止请求号、原因与时间，收回成员标注为“已收回”
（不列为未回执待处理），已回执成员的真实回执照常展示。
续接创建的新批次展示来源批次、原配送员、续接请求号与接手时间；
已转交的旧批次展示转交去向（新批次、新配送员）、续接请求号、原因与
转交时间，转交件标注为“已转交”（不列为未回执待处理），真实回执照常展示。
批次不存在时报错。

示例:
  %s batch --id B1
`, appName, appName, appName)
}

func printFreezeHelp(w io.Writer) {
	fmt.Fprintf(w, `%s freeze — 在站包裹异常冻结

用法:
  %s freeze [--data FILE] --incident 异常单号 --parcel 包裹编号 \
             --station 所在站点 --reason 异常原因

规则:
  - 异常单号、包裹编号、所在站点、异常原因均去除两端空白，不可为空或仅含空白
  - 首次冻结：包裹必须已登记、在指定站点且状态为在站；
    配送中或已签收的包裹不能冻结
  - 成功后站点不变，状态转为“异常冻结”，追加一条含异常单号、
    原因、站点和时间的冻结记录；一件包裹同时只能有一张未解除异常单
  - 冻结期间，包含该包裹的首次交接、配送出站或整批退回整批拒绝，
    其余成员的状态、轨迹和相关结果均不变
  - 冻结只是管理记录，不移动实物、不算新流转
  - 异常单号标识一次异常，解除后也不能复用：相同异常单号且包裹、站点、
    清理后的原因相同，直接返回首次冻结结果与时间，不再次冻结；
    异常单号相同但内容不同报冲突；失败的冻结不占用异常单号
  - 异常单号与包裹编号及其他业务编号分属独立去重范围，允许同名

示例:
  %s freeze --incident E1 --parcel P001 --station 站点A --reason 外包装破损
`, appName, appName, appName)
}

func printRelayHelp(w io.Writer) {
	fmt.Fprintf(w, `%s relay — 配送途中整批续接（原批次未回执包裹交给另一配送员，新建批次继续配送，无需回站）

用法:
  %s relay [--data FILE] --request 续接请求号 --from 原批次号 --to 新批次号 \
           --courier 新配送员 --reason 续接原因

规则:
  - 续接请求号、原批次号、新批次号、新配送员、续接原因均去除两端空白，不可为空或仅含空白
  - 成员与站点取自原批次，不允许另选包裹或站点
  - 首次续接：原批次必须存在、仍开放（未中止、未转交）且有未回执成员；
    新批次号必须从未被出站或续接使用；新配送员必须与原配送员不同；
    未回执成员必须全部仍在原批次配送中且归属出发站，任一不符整批拒绝、不作任何改动
  - 已回执成员不检查也不变更，其后续流转或冻结不阻止续接
  - 成功时新批次按原成员顺序接纳全部未回执件，沿用出发站，记录新配送员及接手时间；
    包裹保持“配送中”及原站点，当前配送归属切换到新批次，各追加一条续接记录；
    原批次永久关闭为“已转交”，原成员、配送员、出站时间与真实回执保留不变；
    转交不算回执、收回或再次出站
  - 转交件不能首次回执旧批次（receipt 与 receipt-import 同样遵守，导入含迟到回执时
    整份拒绝）；旧批次不能首次中止或再次续接；新批次可回执、导入、中止或再次续接；
    续接算新的流转，续接后不能退回配送前的旧交接
  - 续接请求号独立去重，可与批次号、包裹号及其他业务编号同名：
    相同请求号且原批次、新批次、新配送员、清理后的原因相同，直接返回首次转交集合、
    续接信息及时间，不检查现状、不重算成员、不改写台账（后续回执、中止或再次续接后
    仍可重放）；请求号相同但内容不同报冲突；失败的首次续接不占用请求号

示例:
  %s relay --request T1 --from B1 --to B2 --courier 李四 --reason 原配送员车辆故障
`, appName, appName, appName)
}

func printAbortHelp(w io.Writer) {
	fmt.Fprintf(w, `%s abort — 配送批次中止（未回执包裹全部收回出发站）

用法:
  %s abort [--data FILE] --request 中止请求号 --batch 批次号 --reason 中止原因

规则:
  - 中止请求号、批次号、中止原因均去除两端空白，不可为空或仅含空白
  - 成员与站点取自原批次结果，不允许另选成员或站点
  - 首次中止：批次必须存在、尚未完成且未中止，且至少有一件未回执成员；
    这些成员必须仍在原批次配送中、归属出发站，任一不符整批拒绝
  - 已回执成员及其回执完全保留，即使它们已进入其他批次或被冻结也不阻止中止
  - 成功时按原成员顺序将全部未回执件恢复“在站”（站点为出发站），各追加一条
    含批次、中止请求号、原因、站点和时间的收回记录；收回不是失败回执
  - 中止永久关闭原批次，原成员顺序、配送员与出站时间保留不变；
    每批只能成功中止一次，换请求号再中止或中止已完成批次均拒绝
  - 收回算新的流转：收回后不能退回出站前的旧交接；收回件可交接、冻结或
    再次出站，但不能首次回执旧批次（receipt 与 receipt-import 同样遵守）
  - 中止请求号独立去重，可与批次号、包裹号及其他业务编号同名：
    相同请求号且批次、清理后的原因相同，直接返回首次收回集合与时间，
    不重新计算成员、不检查当前状态、不改写台账；换批次或原因报冲突；
    失败的首次中止不占用请求号

示例:
  %s abort --request A1 --batch B1 --reason 车辆故障全部收回
`, appName, appName, appName)
}

func printUnfreezeHelp(w io.Writer) {
	fmt.Fprintf(w, `%s unfreeze — 解除异常冻结

用法:
  %s unfreeze [--data FILE] --request 解除请求号 --incident 异常单号 --note 处理说明

规则:
  - 解除请求号、异常单号、处理说明均去除两端空白，不可为空或仅含空白
  - 首次解除：异常单必须存在且尚未解除，并且仍是该包裹当前冻结对应的
    异常单；异常单不存在、已解除或对应关系不符均拒绝
  - 成功后包裹在原站恢复“在站”，原异常单永久标记为已解除（不可复用），
    追加一条含异常单号、解除请求号、处理说明及时间的记录，原冻结记录不删改；
    之后可用新异常单再次冻结
  - 解除只是管理记录，不移动实物、不算新流转；解除后若没有其他新流转，
    原本可退回的交接仍可整批退回
  - 解除请求号独立去重，可与异常单号、包裹号及其他业务编号同名：
    相同解除请求号且异常单、处理说明相同，直接返回首次解除结果与时间，
    不检查当前状态，不影响后来新建的异常单或后续流转；
    请求号相同但内容不同报冲突；失败的解除不占用请求号

示例:
  %s unfreeze --request U1 --incident E1 --note 已核实放行
`, appName, appName, appName)
}

func printShipHelp(w io.Writer) {
	fmt.Fprintf(w, `%s ship — 站间发运（整单包裹离开源站、尚未到达目的站）

用法:
  %s ship [--data FILE] --shipment 运输单号 --from 源站点 --to 目的站点 \
          --parcel 包裹编号 [--parcel 包裹编号 ...]

规则:
  - 运输单号、源站点、目的站点及每个包裹编号均去除两端空白，不可为空或仅含空白
  - 包裹集合不可为空且编号不可重复；源站点与目的站点不能相同
  - 首次发运：每件包裹必须已登记、在源站且状态为在站；
    任一不满足则整单拒绝，其他包裹的状态与轨迹均不变
  - 全部满足时整单转为“站间在途”（归属站点暂记源站），每件追加一条
    含运输单号、两站和时间的发运记录；成员按首次提交顺序保存，不可修改
  - 每件包裹同时最多属于一张未接收运输单；在途件不能首次交接、退回、
    配送出站或冻结，涉及它的批量操作整批不变
  - 发运算新的流转：发运后不能退回此前的旧交接；运输单不能作为退回的原交接
  - 运输单号独立于已有各类编号，接收后也不能复用：相同运输单号且源站、
    目的站、包裹集合相同（集合顺序无关）直接返回首次成员顺序与发运时间，
    不再追加轨迹；运输单号相同但内容不同报冲突；失败的发运不占用运输单号

示例:
  %s ship --shipment S1 --from 站点A --to 站点B --parcel P001 --parcel P002
`, appName, appName, appName)
}

func printReceiveHelp(w io.Writer) {
	fmt.Fprintf(w, `%s receive — 整单到站接收（运输单全部成员到达目的站）

用法:
  %s receive [--data FILE] --request 接收请求号 --shipment 运输单号 --station 接收站点

规则:
  - 接收请求号、运输单号、接收站点均去除两端空白，不可为空或仅含空白
  - 成员取自运输单，不允许另选包裹
  - 首次接收：运输单必须存在且尚未接收，接收站点必须等于运输单目的站，
    全部成员必须仍归该单在途且归属源站；任一不满足则整单拒绝，不作任何改动
  - 成功时按发运保存顺序将全部成员改归目的站、恢复“在站”，各追加一条
    含运输单号、两站、接收请求号和时间的接收记录；运输单永久标记为已接收，
    随后可继续在站作业；接收算新的流转，接收后不能退回此前的旧交接
  - 接收请求号与运输单号及已有各类编号分属独立去重范围，允许同名：
    相同接收请求号且运输单、接收站点相同，直接返回首次接收结果与时间，
    不检查现状、不再追加轨迹（接收后包裹再流转也照常重放）；
    请求号相同但内容不同报冲突；换请求号再次接收同一运输单拒绝；
    失败的首次接收不占用请求号

示例:
  %s receive --request RS1 --shipment S1 --station 站点B
`, appName, appName, appName)
}

func printShipmentHelp(w io.Writer) {
	fmt.Fprintf(w, `%s shipment — 按运输单号查询站间运输单

用法:
  %s shipment [--data FILE] --id 运输单号

展示源站、目的站、原成员（按首次提交顺序）、运输单状态（待接收 / 已接收）、
发运时间；已接收的运输单同时展示接收请求号、接收站点与接收时间。
运输单不存在时报错。

示例:
  %s shipment --id S1
`, appName, appName, appName)
}
