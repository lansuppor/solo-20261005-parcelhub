package main

import (
	"strings"
	"testing"
)

func TestCLIEndToEndRelay(t *testing.T) {
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

	// 配送途中整批续接：原批次未回执包裹交给另一配送员，新建批次继续配送。
	out, _, code := runCLI(t, dbPath, "relay", "--request", "T1", "--from", "B1", "--to", "B2",
		"--courier", "李四", "--reason", "原配送员车辆故障")
	if code != 0 || !strings.Contains(out, "续接成功") ||
		!strings.Contains(out, "原批次号: B1") || !strings.Contains(out, "新批次号: B2") ||
		!strings.Contains(out, "原配送员: 张三") || !strings.Contains(out, "新配送员: 李四") ||
		!strings.Contains(out, "出发站: 站点A") || !strings.Contains(out, "接手时间:") ||
		!strings.Contains(out, "P001") || !strings.Contains(out, "P002") {
		t.Fatalf("续接输出不符: code=%d out=%s", code, out)
	}

	// 同号同内容重放：返回首次结果，不追加轨迹。
	out, _, code = runCLI(t, dbPath, "relay", "--request", "T1", "--from", "B1", "--to", "B2",
		"--courier", "李四", "--reason", "原配送员车辆故障")
	if code != 0 || !strings.Contains(out, "重复提交") {
		t.Fatalf("续接重放应返回首次结果: code=%d out=%s", code, out)
	}
	// 换内容冲突，退出码 1。
	_, errText, code := runCLI(t, dbPath, "relay", "--request", "T1", "--from", "B1", "--to", "B3",
		"--courier", "李四", "--reason", "原配送员车辆故障")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("换内容应报冲突(1): code=%d err=%s", code, errText)
	}
	// 空白输入校验失败，退出码 1。
	if _, _, code := runCLI(t, dbPath, "relay", "--request", "  ", "--from", "B1", "--to", "B3",
		"--courier", "李四", "--reason", "原因"); code != exitBusiness {
		t.Fatalf("空白请求号应失败(1): code=%d", code)
	}

	// query 展示完整轨迹及配送中包裹的当前批次、配送员。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前状态: 配送中") ||
		!strings.Contains(out, "当前批次: B2") || !strings.Contains(out, "当前配送员: 李四") ||
		!strings.Contains(out, "操作: 续接") || !strings.Contains(out, "原批次号: B1") ||
		!strings.Contains(out, "新批次号: B2") || !strings.Contains(out, "续接请求号: T1") {
		t.Fatalf("query 应展示当前批次/配送员与续接轨迹: code=%d out=%s", code, out)
	}

	// batch 展示旧批次转交去向及时间，转交件不列为待处理。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 已转交") ||
		!strings.Contains(out, "转交去向: 批次 B2") || !strings.Contains(out, "续接请求号: T1") ||
		!strings.Contains(out, "转交时间:") || !strings.Contains(out, "已转交") ||
		strings.Contains(out, "未回执") {
		t.Fatalf("旧批次应展示转交去向且转交件不列为待处理: code=%d out=%s", code, out)
	}
	// batch 展示新批次来源和接手时间。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B2")
	if code != 0 || !strings.Contains(out, "批次状态: 配送中") ||
		!strings.Contains(out, "批次来源: 续接自批次 B1") || !strings.Contains(out, "接手时间:") ||
		!strings.Contains(out, "配送员: 李四") {
		t.Fatalf("新批次应展示来源与接手时间: code=%d out=%s", code, out)
	}

	// 新批次可正常回执。
	out, _, code = runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B2", "--parcel", "P001", "--result", "签收")
	if code != 0 || !strings.Contains(out, "回执成功") {
		t.Fatalf("新批次应可回执: code=%d out=%s", code, out)
	}
	// 转交件不能首次回执旧批次。
	_, errText, code = runCLI(t, dbPath, "receipt", "--request", "RC2", "--batch", "B1", "--parcel", "P002", "--result", "签收")
	if code != exitBusiness || !strings.Contains(errText, "不在批次") {
		t.Fatalf("转交件回执旧批次应失败(1): code=%d err=%s", code, errText)
	}
	// 旧批次不能首次中止或再次续接。
	if _, _, code := runCLI(t, dbPath, "abort", "--request", "A1", "--batch", "B1", "--reason", "原因"); code != exitBusiness {
		t.Fatalf("已转交批次中止应失败(1): code=%d", code)
	}
	if _, _, code := runCLI(t, dbPath, "relay", "--request", "T2", "--from", "B1", "--to", "B3",
		"--courier", "王五", "--reason", "原因"); code != exitBusiness {
		t.Fatalf("已转交批次再次续接应失败(1): code=%d", code)
	}
	// dispatch 使用续接创建的批次号报冲突。
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B2", "--station", "站点A", "--courier", "李四",
		"--parcel", "P002"); code != exitBusiness {
		t.Fatalf("dispatch 使用续接批次号应冲突(1): code=%d", code)
	}
}
