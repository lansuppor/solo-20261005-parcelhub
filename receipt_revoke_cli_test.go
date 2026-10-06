package main

import (
	"strings"
	"testing"
)

func TestCLIEndToEndReceiptRevoke(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"

	for _, id := range []string{"P001", "P002"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败: code=%d", id, code)
		}
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A", "--courier", "张三",
		"--parcel", "P001", "--parcel", "P002"); code != 0 {
		t.Fatalf("出站失败: code=%d", code)
	}
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001",
		"--result", "失败", "--reason", "收件人不在"); code != 0 {
		t.Fatalf("回执失败: code=%d", code)
	}

	// 撤销误录回执：输出撤销请求号、原回执请求号、包裹、批次、原因及时间。
	out, _, code := runCLI(t, dbPath, "receipt-revoke", "--request", "RV1", "--receipt", "RC1",
		"--reason", "误录失败，实际仍配送中")
	if code != 0 || !strings.Contains(out, "撤销成功") ||
		!strings.Contains(out, "撤销请求号: RV1") || !strings.Contains(out, "原回执请求号: RC1") ||
		!strings.Contains(out, "批次号: B1") || !strings.Contains(out, "包裹编号: P001") ||
		!strings.Contains(out, "撤销原因: 误录失败，实际仍配送中") || !strings.Contains(out, "发生时间:") {
		t.Fatalf("撤销输出不符: code=%d out=%s", code, out)
	}

	// 撤销后该件恢复原批次配送中，站点、配送员不变。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前状态: 配送中") ||
		!strings.Contains(out, "当前批次: B1") || !strings.Contains(out, "当前配送员: 张三") ||
		!strings.Contains(out, "操作: 撤销回执") || !strings.Contains(out, "撤销请求号: RV1") ||
		!strings.Contains(out, "原回执请求号: RC1") || !strings.Contains(out, "撤销状态: 已撤销") {
		t.Fatalf("query 应展示恢复配送中与撤销轨迹: code=%d out=%s", code, out)
	}

	// batch 区分有效回执与撤销历史。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 配送中") ||
		!strings.Contains(out, "逐件回执（0/2 已回执）") ||
		!strings.Contains(out, "未回执（原回执 RC1 已撤销，撤销请求号: RV1）") ||
		!strings.Contains(out, "撤销记录（1 条）") ||
		!strings.Contains(out, "撤销请求号: RV1") || !strings.Contains(out, "原回执请求号: RC1") {
		t.Fatalf("batch 应区分有效回执与撤销历史: code=%d out=%s", code, out)
	}

	// 同号、同原回执、同原因重放：返回首次信息，不再次改变进度。
	out, _, code = runCLI(t, dbPath, "receipt-revoke", "--request", "RV1", "--receipt", "RC1",
		"--reason", "误录失败，实际仍配送中")
	if code != 0 || !strings.Contains(out, "重复提交") {
		t.Fatalf("撤销重放应返回首次结果: code=%d out=%s", code, out)
	}
	// 换内容冲突，退出码 1。
	_, errText, code := runCLI(t, dbPath, "receipt-revoke", "--request", "RV1", "--receipt", "RC1",
		"--reason", "别的原因")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("换内容应报冲突(1): code=%d err=%s", code, errText)
	}
	// 空白输入校验失败，退出码 1。
	if _, _, code := runCLI(t, dbPath, "receipt-revoke", "--request", " ", "--receipt", "RC1",
		"--reason", "原因"); code != exitBusiness {
		t.Fatalf("空白请求号应失败(1): code=%d", code)
	}
	// 每条原回执只能撤销一次：换撤销号再次撤销拒绝。
	if _, _, code := runCLI(t, dbPath, "receipt-revoke", "--request", "RV2", "--receipt", "RC1",
		"--reason", "再次撤销"); code != exitBusiness {
		t.Fatalf("重复撤销同一回执应失败(1): code=%d", code)
	}

	// 已撤销回执的同内容重放仍返回原结果并标明已撤销，不恢复回执。
	out, _, code = runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001",
		"--result", "失败", "--reason", "收件人不在")
	if code != 0 || !strings.Contains(out, "重复提交") || !strings.Contains(out, "已撤销") ||
		!strings.Contains(out, "撤销请求号: RV1") {
		t.Fatalf("已撤销回执的重放应标明已撤销: code=%d out=%s", code, out)
	}
	// 换内容仍冲突。
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001",
		"--result", "签收"); code != exitBusiness {
		t.Fatalf("已撤销回执换内容应冲突(1): code=%d", code)
	}

	// 撤销后可用新回执请求号再次提交回执。
	out, _, code = runCLI(t, dbPath, "receipt", "--request", "RC9", "--batch", "B1", "--parcel", "P001",
		"--result", "签收")
	if code != 0 || !strings.Contains(out, "回执成功") {
		t.Fatalf("撤销后应可重新回执: code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "逐件回执（1/2 已回执）") ||
		!strings.Contains(out, "结果: 签收    请求号: RC9") {
		t.Fatalf("重新回执后进度应只计未撤销结果: code=%d out=%s", code, out)
	}
}
