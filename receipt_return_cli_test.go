package main

import (
	"strings"
	"testing"
)

func TestCLIEndToEndReceiptReturn(t *testing.T) {
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
		"--result", "签收"); code != 0 {
		t.Fatalf("回执失败: code=%d", code)
	}
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC2", "--batch", "B1", "--parcel", "P002",
		"--result", "签收"); code != 0 {
		t.Fatalf("回执失败: code=%d", code)
	}

	// 签收后实物退件：输出退件请求、原签收、包裹、批次、接收站、原因及时间。
	out, _, code := runCLI(t, dbPath, "receipt-return", "--request", "RR1", "--receipt", "RC1",
		"--reason", "  客户退货，实物送回  ")
	if code != 0 || !strings.Contains(out, "退件成功") ||
		!strings.Contains(out, "退件请求号: RR1") || !strings.Contains(out, "原签收请求号: RC1") ||
		!strings.Contains(out, "批次号: B1") || !strings.Contains(out, "包裹编号: P001") ||
		!strings.Contains(out, "接收站: 站点A") ||
		!strings.Contains(out, "退件原因: 客户退货，实物送回") || !strings.Contains(out, "发生时间:") {
		t.Fatalf("退件输出不符: code=%d out=%s", code, out)
	}

	// 该件恢复出发站在站；轨迹保留原签收并追加退件记录。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前站点: 站点A") || !strings.Contains(out, "当前状态: 在站") ||
		!strings.Contains(out, "操作: 回执") || !strings.Contains(out, "请求号: RC1") ||
		!strings.Contains(out, "操作: 退件") || !strings.Contains(out, "退件请求号: RR1") ||
		!strings.Contains(out, "原签收请求号: RC1") || !strings.Contains(out, "接收站: 站点A") {
		t.Fatalf("query 应展示恢复在站与退件轨迹: code=%d out=%s", code, out)
	}

	// batch 保留原签收并计入进度，标注退件事实，不列为待配送或撤销，另列退件记录。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 已完成") ||
		!strings.Contains(out, "逐件回执（2/2 已回执）") ||
		!strings.Contains(out, "结果: 签收    请求号: RC1") ||
		!strings.Contains(out, "已退件（实物送回 站点A）") ||
		!strings.Contains(out, "退件请求号: RR1") ||
		!strings.Contains(out, "退件记录（1 条") ||
		strings.Contains(out, "未回执（原回执 RC1") {
		t.Fatalf("batch 应保留签收并显示退件事实: code=%d out=%s", code, out)
	}

	// 同号、同原签收、清理后同原因重放：返回首次结果，不追加轨迹。
	out, _, code = runCLI(t, dbPath, "receipt-return", "--request", " RR1 ", "--receipt", "RC1",
		"--reason", "客户退货，实物送回")
	if code != 0 || !strings.Contains(out, "重复提交") {
		t.Fatalf("退件重放应返回首次结果: code=%d out=%s", code, out)
	}
	// 换内容冲突，退出码 1。
	_, errText, code := runCLI(t, dbPath, "receipt-return", "--request", "RR1", "--receipt", "RC1",
		"--reason", "别的原因")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("换内容应报冲突(1): code=%d err=%s", code, errText)
	}
	// 空白输入校验失败，退出码 1。
	if _, _, code := runCLI(t, dbPath, "receipt-return", "--request", " ", "--receipt", "RC1",
		"--reason", "原因"); code != exitBusiness {
		t.Fatalf("空白请求号应失败(1): code=%d", code)
	}
	// 每条签收只能退件一次：换请求号再退拒绝。
	if _, errText, code := runCLI(t, dbPath, "receipt-return", "--request", "RR2", "--receipt", "RC1",
		"--reason", "再次退货"); code != exitBusiness || !strings.Contains(errText, "每条签收只能退件一次") {
		t.Fatalf("重复退件同一签收应失败(1): code=%d err=%s", code, errText)
	}
	// 退件后不能再撤销该签收。
	if _, errText, code := runCLI(t, dbPath, "receipt-revoke", "--request", "RV1", "--receipt", "RC1",
		"--reason", "误录签收"); code != exitBusiness || !strings.Contains(errText, "退件后不能再撤销") {
		t.Fatalf("退件后撤销应失败(1): code=%d err=%s", code, errText)
	}

	// 再次签收可针对新回执办理退件。
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B2", "--station", "站点A", "--courier", "李四",
		"--parcel", "P001"); code != 0 {
		t.Fatalf("退件回站后应可再次出站: code=%d", code)
	}
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC9", "--batch", "B2", "--parcel", "P001",
		"--result", "签收"); code != 0 {
		t.Fatalf("新批次签收失败: code=%d", code)
	}
	out, _, code = runCLI(t, dbPath, "receipt-return", "--request", "RR9", "--receipt", "RC9",
		"--reason", "二次退货")
	if code != 0 || !strings.Contains(out, "退件成功") {
		t.Fatalf("新签收应可再次退件: code=%d out=%s", code, out)
	}
	// 失败回执不能退件：另建一件的失败回执。
	if _, _, code := runCLI(t, dbPath, "register", "--id", "P003", "--station", "站点A"); code != 0 {
		t.Fatal("登记 P003 失败")
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B3", "--station", "站点A", "--courier", "王五",
		"--parcel", "P003"); code != 0 {
		t.Fatal("B3 出站失败")
	}
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC3", "--batch", "B3", "--parcel", "P003",
		"--result", "失败", "--reason", "收件人不在"); code != 0 {
		t.Fatal("失败回执失败")
	}
	if _, errText, code := runCLI(t, dbPath, "receipt-return", "--request", "RRX", "--receipt", "RC3",
		"--reason", "x"); code != exitBusiness || !strings.Contains(errText, "只有已真实签收") {
		t.Fatalf("失败回执退件应失败(1): code=%d err=%s", code, errText)
	}
}
