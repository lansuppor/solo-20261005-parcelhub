package main

import (
	"fmt"
	"os"
	"strings"
)

const appName = "parcelhub"

const helpText = `parcelhub — 包裹站点交接台账

用法:
  parcelhub <命令> [参数]

命令:
  register   登记单件包裹
             --parcel <编号> --site <收件站点>
  query      查询包裹当前站点、状态与完整轨迹
             --parcel <编号>
  handover   站点交接（一次提交多件包裹）
             --request <请求号> --from <源站点> --to <目的站点> --parcels <编号,编号,...>

通用参数:
  --data <文件>   本地数据文件路径（默认 parcelhub.json）
  --help, -h      显示本帮助

说明:
  登记成功后包裹状态为在站；交接表示实物已完成移交，成功后包裹归属目的站点。
  交接请求号用于去重：相同请求号与相同内容重放返回首次结果，内容不同则冲突。
  所有数据保存在本地文件，无需外部服务。
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h")) {
		fmt.Fprint(os.Stdout, helpText)
		return 0
	}
	cmd, rest := args[0], args[1:]
	for _, a := range rest {
		if a == "--help" || a == "-h" {
			fmt.Fprint(os.Stdout, helpText)
			return 0
		}
	}
	switch cmd {
	case "register":
		return runRegister(rest)
	case "query":
		return runQuery(rest)
	case "handover":
		return runHandover(rest)
	default:
		fmt.Fprintf(os.Stderr, "%s: 未知命令 %q；使用 --help 查看帮助\n", appName, cmd)
		return 2
	}
}

// parseFlags 解析 --name value / --name=value 形式的参数。
// listFlags 中的参数可重复出现并累积。未知参数或多余位置参数返回错误。
func parseFlags(args []string, listFlags map[string]bool) (map[string]string, map[string][]string, error) {
	single := map[string]string{}
	multi := map[string][]string{}
	known := map[string]bool{"data": true, "parcel": true, "site": true, "request": true, "from": true, "to": true, "parcels": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			return nil, nil, fmt.Errorf("未知参数 %q", a)
		}
		name := strings.TrimPrefix(a, "--")
		value, hasValue := "", false
		if idx := strings.Index(name, "="); idx >= 0 {
			name, value, hasValue = name[:idx], name[idx+1:], true
		}
		if !known[name] {
			return nil, nil, fmt.Errorf("未知参数 %q", "--"+name)
		}
		if !hasValue {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("参数 %q 缺少取值", "--"+name)
			}
			i++
			value = args[i]
		}
		if listFlags[name] {
			multi[name] = append(multi[name], value)
		} else {
			if _, dup := single[name]; dup {
				return nil, nil, fmt.Errorf("参数 %q 重复指定", "--"+name)
			}
			single[name] = value
		}
	}
	return single, multi, nil
}

func dataPath(single map[string]string) string {
	if p, ok := single["data"]; ok {
		return p
	}
	return "parcelhub.json"
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "%s: %v\n", appName, err)
	return 1
}

func usageErr(err error) int {
	fmt.Fprintf(os.Stderr, "%s: %v；使用 --help 查看帮助\n", appName, err)
	return 2
}

func runRegister(args []string) int {
	single, _, err := parseFlags(args, nil)
	if err != nil {
		return usageErr(err)
	}
	if err := cmdRegister(dataPath(single), single["parcel"], single["site"], os.Stdout); err != nil {
		return fail(err)
	}
	return 0
}

func runQuery(args []string) int {
	single, _, err := parseFlags(args, nil)
	if err != nil {
		return usageErr(err)
	}
	if err := cmdQuery(dataPath(single), single["parcel"], os.Stdout); err != nil {
		return fail(err)
	}
	return 0
}

func runHandover(args []string) int {
	single, multi, err := parseFlags(args, map[string]bool{"parcels": true})
	if err != nil {
		return usageErr(err)
	}
	var parcels []string
	for _, group := range multi["parcels"] {
		for _, id := range strings.Split(group, ",") {
			parcels = append(parcels, strings.TrimSpace(id))
		}
	}
	if err := cmdHandover(dataPath(single), single["request"], single["from"], single["to"], parcels, os.Stdout); err != nil {
		return fail(err)
	}
	return 0
}
