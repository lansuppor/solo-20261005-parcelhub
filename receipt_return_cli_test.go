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
		t.Fatalf("P001 签收失败: code=%d", code)
	}
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC2", "--batch", "B1", "--parcel", "P002",
		"--result", "签收"); code != 0 {
		t.Fatalf("P002 签收失败: code=%d", code)
	}

	// 退件：输出退件请求、原签收、包裹、批次、接收站、原因与时间。
	out, _, code := runCLI(t, dbPath, "receipt-return", "--request", "PR1", "--receipt", "RC1",
		"--reason", " 收件人拒收 ")
	if code != 0 || !strings.Contains(out, "退件成功") ||
		!strings.Contains(out, "退件请求号: PR1") || !strings.Contains(out, "原签收回执请求号: RC1") ||
		!strings.Contains(out, "批次号: B1") || !strings.Contains(out, "包裹编号: P001") ||
		!strings.Contains(out, "接收站: 站点A") || !strings.Contains(out, "退件原因: 收件人拒收") ||
		!strings.Contains(out, "发生时间:") {
		t.Fatalf("退件输出不符: code=%d out=%s", code, out)
	}

	// 退件后在站，query 展示原签收（标注已退件）与退件轨迹。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前站点: 站点A") || !strings.Contains(out, "当前状态: 在站") ||
		!strings.Contains(out, "操作: 回执") || strings.Contains(out, "撤销状态") ||
		!strings.Contains(out, "退件状态: 已退件入站（退件请求号: PR1") ||
		!strings.Contains(out, "操作: 退件") ||
		!strings.Contains(out, "退件请求号: PR1") || !strings.Contains(out, "原签收回执请求号: RC1") ||
		!strings.Contains(out, "接收站: 站点A") || !strings.Contains(out, "原因: 收件人拒收") {
		t.Fatalf("退件后 query 不符: code=%d out=%s", code, out)
	}

	// batch 保留原签收并计有效回执，不列为待配送或已撤销，另列退件记录。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 已完成") ||
		!strings.Contains(out, "逐件回执（2/2 已回执）") ||
		!strings.Contains(out, "- P001    已回执    结果: 签收    请求号: RC1") ||
		!strings.Contains(out, "已退件入站    退件请求号: PR1") ||
		!strings.Contains(out, "退件记录（1 条") ||
		!strings.Contains(out, "退件请求号: PR1    原签收回执请求号: RC1") {
		t.Fatalf("batch 退件展示不符: code=%d out=%s", code, out)
	}
	if strings.Contains(out, "未回执") || strings.Contains(out, "撤销记录") || strings.Contains(out, "待配送") {
		t.Fatalf("退件不得把该件列为未回执/待配送或产生撤销记录: %s", out)
	}

	// 同号、同原签收、清理后同原因重放：返回首次结果，不追加轨迹。
	out, _, code = runCLI(t, dbPath, "receipt-return", "--request", "PR1", "--receipt", "RC1",
		"--reason", "收件人拒收")
	if code != 0 || !strings.Contains(out, "重复提交") {
		t.Fatalf("退件重放应返回首次结果: code=%d out=%s", code, out)
	}
	// 换内容冲突，退出码 1。
	if _, errText, code := runCLI(t, dbPath, "receipt-return", "--request", "PR1", "--receipt", "RC1",
		"--reason", "别的原因"); code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("换内容应报冲突(1): code=%d", code)
	}
	// 空白输入校验失败，退出码 1。
	if _, _, code := runCLI(t, dbPath, "receipt-return", "--request", " ", "--receipt", "RC1",
		"--reason", "原因"); code != exitBusiness {
		t.Fatalf("空白请求号应失败(1): code=%d", code)
	}
	// 每条签收只能退一次：换退件号再退拒绝。
	if _, errText, code := runCLI(t, dbPath, "receipt-return", "--request", "PR2", "--receipt", "RC1",
		"--reason", "再退"); code != exitBusiness || !strings.Contains(errText, "只能成功退件一次") {
		t.Fatalf("重复退件同一签收应失败(1): code=%d err=%s", code, errText)
	}
	// 退件后不能撤销该签收。
	if _, errText, code := runCLI(t, dbPath, "receipt-revoke", "--request", "RV1", "--receipt", "RC1",
		"--reason", "误录签收"); code != exitBusiness || !strings.Contains(errText, "已办理实物退件") {
		t.Fatalf("退件后撤销应失败(1): code=%d err=%s", code, errText)
	}
	// 失败回执不能退件。
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B2", "--station", "站点A",
		"--courier", "李四", "--parcel", "P001"); code != 0 {
		t.Fatalf("退件后重新出站应成功: code=%d", code)
	}
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC3", "--batch", "B2", "--parcel", "P001",
		"--result", "失败", "--reason", "再次不在"); code != 0 {
		t.Fatalf("失败回执应成功: code=%d", code)
	}
	if _, errText, code := runCLI(t, dbPath, "receipt-return", "--request", "PR3", "--receipt", "RC3",
		"--reason", "退失败件"); code != exitBusiness || !strings.Contains(errText, "只有真实签收") {
		t.Fatalf("失败回执退件应失败(1): code=%d err=%s", code, errText)
	}

	// receipt 重放旧签收仍返回原结果和时间，标明已退件，不改变包裹。
	out, _, code = runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001",
		"--result", "签收")
	if code != 0 || !strings.Contains(out, "重复提交") || !strings.Contains(out, "退件状态: 已退件入站（退件请求号: PR1）") {
		t.Fatalf("旧签收重放应标明已退件: code=%d out=%s", code, out)
	}
}

func TestCLIReceiptReturnImportMixed(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A")
	runCLI(t, dbPath, "register", "--id", "P002", "--station", "站点A")
	runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A", "--courier", "张三",
		"--parcel", "P001", "--parcel", "P002")
	runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001", "--result", "签收")
	runCLI(t, dbPath, "receipt-return", "--request", "PR1", "--receipt", "RC1", "--reason", "拒收")

	// 混合重放（已退件旧签收）与新回执的导入：整份原子处理，重放条目标明已退件。
	importFile := writeImportFile(t, "receipts.json", `[
	  {"request": "RC1", "batch": "B1", "parcel": "P001", "result": "签收"},
	  {"request": "RC2", "batch": "B1", "parcel": "P002", "result": "签收"}
	]`)
	out, _, code := runCLI(t, dbPath, "receipt-import", "--file", importFile)
	if code != 0 || !strings.Contains(out, "重放（返回首次保存的结果），该签收已退件") ||
		!strings.Contains(out, "标记: 新增") {
		t.Fatalf("混合导入应整份成功并标明重放退件: code=%d out=%s", code, out)
	}
}
