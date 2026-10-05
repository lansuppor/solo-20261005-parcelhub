package main

import (
	"fmt"
	"io"
	"strings"
	"time"
)

func now() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z07:00")
}

func blank(s string) bool {
	return strings.TrimSpace(s) == ""
}

// cmdRegister 登记单件包裹：包裹编号 + 收件站点，编号在台账内唯一。
func cmdRegister(dataPath, parcelID, site string, out io.Writer) error {
	if blank(parcelID) {
		return fmt.Errorf("包裹编号不能为空或仅含空白")
	}
	if blank(site) {
		return fmt.Errorf("收件站点不能为空或仅含空白")
	}
	l, err := loadLedger(dataPath)
	if err != nil {
		return err
	}
	if _, exists := l.Parcels[parcelID]; exists {
		return fmt.Errorf("包裹编号 %q 已登记，不能重复登记", parcelID)
	}
	l.Parcels[parcelID] = &Parcel{
		ID:     parcelID,
		Site:   site,
		Status: statusAtSite,
		Events: []Event{{Op: "收件", Site: site, Time: now()}},
	}
	if err := saveLedger(dataPath, l); err != nil {
		return err
	}
	fmt.Fprintf(out, "登记成功\n包裹: %s\n站点: %s\n状态: %s\n", parcelID, site, statusAtSite)
	return nil
}

// cmdQuery 查询包裹当前站点、状态及按提交顺序排列的完整轨迹。
func cmdQuery(dataPath, parcelID string, out io.Writer) error {
	if blank(parcelID) {
		return fmt.Errorf("包裹编号不能为空或仅含空白")
	}
	l, err := loadLedger(dataPath)
	if err != nil {
		return err
	}
	p, exists := l.Parcels[parcelID]
	if !exists {
		return fmt.Errorf("包裹 %q 不存在", parcelID)
	}
	fmt.Fprintf(out, "包裹: %s\n当前站点: %s\n状态: %s\n轨迹:\n", p.ID, p.Site, p.Status)
	for i, e := range p.Events {
		switch e.Op {
		case "收件":
			fmt.Fprintf(out, "  %d. 收件 站点=%s 时间=%s\n", i+1, e.Site, e.Time)
		case "交接":
			fmt.Fprintf(out, "  %d. 交接 请求号=%s 从=%s 到=%s 时间=%s\n", i+1, e.RequestID, e.From, e.To, e.Time)
		default:
			fmt.Fprintf(out, "  %d. %s 时间=%s\n", i+1, e.Op, e.Time)
		}
	}
	return nil
}

// cmdHandover 站点交接：整批包裹由源站点移交目的站点，支持请求号去重。
func cmdHandover(dataPath, requestID, from, to string, parcels []string, out io.Writer) error {
	if blank(requestID) {
		return fmt.Errorf("请求号不能为空或仅含空白")
	}
	if blank(from) {
		return fmt.Errorf("源站点不能为空或仅含空白")
	}
	if blank(to) {
		return fmt.Errorf("目的站点不能为空或仅含空白")
	}
	if from == to {
		return fmt.Errorf("源站点与目的站点不能相同")
	}
	if len(parcels) == 0 {
		return fmt.Errorf("包裹编号集合不能为空")
	}
	seen := map[string]bool{}
	for _, id := range parcels {
		if blank(id) {
			return fmt.Errorf("包裹编号不能为空或仅含空白")
		}
		if seen[id] {
			return fmt.Errorf("包裹编号集合中存在重复: %q", id)
		}
		seen[id] = true
	}

	l, err := loadLedger(dataPath)
	if err != nil {
		return err
	}

	// 请求号去重：内容相同直接返回首次结果；内容不同报冲突。
	if rec, exists := l.Requests[requestID]; exists {
		if rec.Key == handoverKey(from, to, parcels) {
			printHandoverResult(out, rec.Result, true)
			return nil
		}
		return fmt.Errorf("请求号 %q 已用于其他交接内容，发生冲突", requestID)
	}

	// 首次提交：每件包裹必须已登记且当前归属源站点，否则整次失败。
	for _, id := range parcels {
		p, exists := l.Parcels[id]
		if !exists {
			return fmt.Errorf("包裹 %q 不存在，交接失败", id)
		}
		if p.Site != from {
			return fmt.Errorf("包裹 %q 当前归属站点 %q，不在源站点 %q，交接失败", id, p.Site, from)
		}
	}

	ts := now()
	for _, id := range parcels {
		p := l.Parcels[id]
		p.Site = to
		p.Status = statusAtSite
		p.Events = append(p.Events, Event{Op: "交接", From: from, To: to, RequestID: requestID, Time: ts})
	}
	result := HandoverResult{
		RequestID: requestID,
		From:      from,
		To:        to,
		Parcels:   append([]string(nil), parcels...),
	}
	l.Requests[requestID] = &HandoverRecord{Key: handoverKey(from, to, parcels), Result: result}

	if err := saveLedger(dataPath, l); err != nil {
		return err
	}
	printHandoverResult(out, result, false)
	return nil
}

func printHandoverResult(out io.Writer, r HandoverResult, replay bool) {
	fmt.Fprintf(out, "交接成功")
	if replay {
		fmt.Fprintf(out, "（重复请求，返回首次结果）")
	}
	fmt.Fprintf(out, "\n请求号: %s\n源站点: %s\n目的站点: %s\n包裹:\n", r.RequestID, r.From, r.To)
	for _, id := range r.Parcels {
		fmt.Fprintf(out, "  - %s\n", id)
	}
}
