package main

import (
	"strings"
	"testing"
)

// 端到端：登记 -> 创建清单 -> 查询预留 -> 确认出站 -> 批次与清单查询。
func TestCLIManifestConfirmFlow(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	for _, id := range []string{"P001", "P002"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败", id)
		}
	}

	out, _, code := runCLI(t, dbPath, "manifest-create", "--manifest", "M1", "--station", "站点A",
		"--parcel", "P001", "--parcel", "P002")
	if code != 0 || !strings.Contains(out, "清单创建成功") || !strings.Contains(out, "清单号: M1") {
		t.Fatalf("创建清单失败: code=%d out=%s", code, out)
	}

	// query 显示当前预留与清单预留轨迹。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前预留: 清单号: M1") || !strings.Contains(out, "操作: 清单预留") {
		t.Fatalf("query 应显示当前预留与清单预留轨迹: code=%d out=%s", code, out)
	}

	// 清单查询显示待出站。
	out, _, code = runCLI(t, dbPath, "manifest", "--id", "M1")
	if code != 0 || !strings.Contains(out, "清单状态: 待出站") {
		t.Fatalf("清单查询应显示待出站: code=%d out=%s", code, out)
	}

	// 预留件不能直接 dispatch。
	if _, _, code = runCLI(t, dbPath, "dispatch", "--batch", "B0", "--station", "站点A",
		"--courier", "张三", "--parcel", "P001"); code != exitBusiness {
		t.Fatalf("预留件直接 dispatch 应以状态码 1 退出，得到 %d", code)
	}

	// 确认出站。
	out, _, code = runCLI(t, dbPath, "manifest-confirm", "--manifest", "M1", "--batch", "B1", "--courier", "张三")
	if code != 0 || !strings.Contains(out, "确认出站成功") || !strings.Contains(out, "批次号: B1") {
		t.Fatalf("确认出站失败: code=%d out=%s", code, out)
	}

	// 批次查询可用，成员配送中。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次号: B1") || !strings.Contains(out, "配送员: 张三") {
		t.Fatalf("批次查询失败: code=%d out=%s", code, out)
	}
	// 清单查询显示已出站与确认批次。
	out, _, code = runCLI(t, dbPath, "manifest", "--id", "M1")
	if code != 0 || !strings.Contains(out, "清单状态: 已出站") || !strings.Contains(out, "确认批次: B1") {
		t.Fatalf("清单查询应显示已出站与确认批次: code=%d out=%s", code, out)
	}
	// query 显示配送中，不再显示预留。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前状态: 配送中") || strings.Contains(out, "当前预留") {
		t.Fatalf("确认后 query 应显示配送中且无预留: code=%d out=%s", code, out)
	}
	// 确认重放：同批次同配送员返回首次结果。
	out, _, code = runCLI(t, dbPath, "manifest-confirm", "--manifest", "M1", "--batch", "B1", "--courier", "张三")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("确认重放应返回首次结果: code=%d out=%s", code, out)
	}
	// 换内容确认冲突。
	if _, _, code = runCLI(t, dbPath, "manifest-confirm", "--manifest", "M1", "--batch", "B2", "--courier", "张三"); code != exitBusiness {
		t.Fatalf("换内容确认应以状态码 1 退出，得到 %d", code)
	}
}

// 端到端：创建清单 -> 取消 -> 释放后可正常作业。
func TestCLIManifestCancelFlow(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	if _, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A"); code != 0 {
		t.Fatal("登记失败")
	}
	if _, _, code := runCLI(t, dbPath, "manifest-create", "--manifest", "M1", "--station", "站点A", "--parcel", "P001"); code != 0 {
		t.Fatal("创建清单失败")
	}

	out, _, code := runCLI(t, dbPath, "manifest-cancel", "--request", "MC1", "--manifest", "M1", "--reason", "计划调整")
	if code != 0 || !strings.Contains(out, "清单取消成功") {
		t.Fatalf("取消清单失败: code=%d out=%s", code, out)
	}
	// 清单查询显示已取消与取消信息。
	out, _, code = runCLI(t, dbPath, "manifest", "--id", "M1")
	if code != 0 || !strings.Contains(out, "清单状态: 已取消") ||
		!strings.Contains(out, "取消请求号: MC1") || !strings.Contains(out, "取消原因: 计划调整") {
		t.Fatalf("清单查询应显示已取消与取消信息: code=%d out=%s", code, out)
	}
	// query 显示清单取消轨迹，不再显示预留。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "操作: 清单取消") || strings.Contains(out, "当前预留") {
		t.Fatalf("取消后 query 应显示清单取消轨迹且无预留: code=%d out=%s", code, out)
	}
	// 取消重放。
	out, _, code = runCLI(t, dbPath, "manifest-cancel", "--request", "MC1", "--manifest", "M1", "--reason", "计划调整")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("取消重放应返回首次结果: code=%d out=%s", code, out)
	}
	// 换号再次取消同一清单拒绝。
	if _, _, code = runCLI(t, dbPath, "manifest-cancel", "--request", "MC2", "--manifest", "M1", "--reason", "计划调整"); code != exitBusiness {
		t.Fatalf("换号再次取消应以状态码 1 退出，得到 %d", code)
	}
	// 释放后可正常 dispatch。
	if _, _, code = runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A",
		"--courier", "张三", "--parcel", "P001"); code != 0 {
		t.Fatalf("取消后应可正常 dispatch，得到 %d", code)
	}
}

// 标识、配送员、原因清理两端空白后非空校验。
func TestCLIManifestBlankArgs(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	if _, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A"); code != 0 {
		t.Fatal("登记失败")
	}
	cases := [][]string{
		{"manifest-create", "--manifest", "  ", "--station", "站点A", "--parcel", "P001"},
		{"manifest-create", "--manifest", "M1", "--station", "站点A"},
		{"manifest-confirm", "--manifest", "M1", "--batch", "B1", "--courier", "  "},
		{"manifest-cancel", "--request", "MC1", "--manifest", "M1", "--reason", "  "},
	}
	for _, c := range cases {
		if _, _, code := runCLI(t, dbPath, c...); code != exitBusiness {
			t.Fatalf("%v 应以状态码 1 退出，得到 %d", c, code)
		}
	}
}

// 确认与取消竞争至多一方成功。
func TestConcurrentManifestConfirmCancel(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	if _, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A"); code != 0 {
		t.Fatal("登记失败")
	}
	if _, _, code := runCLI(t, dbPath, "manifest-create", "--manifest", "M1", "--station", "站点A", "--parcel", "P001"); code != 0 {
		t.Fatal("创建清单失败")
	}

	const n = 8
	_, errs, codes := runParallel(t, n, func(i int) []string {
		if i%2 == 0 {
			return []string{"--data", dbPath, "manifest-confirm", "--manifest", "M1", "--batch", "B1", "--courier", "张三"}
		}
		return []string{"--data", dbPath, "manifest-cancel", "--request", "MC1", "--manifest", "M1", "--reason", "计划调整"}
	})
	// 同内容并发：确认与取消各自可重放，但清单只能终结一次——
	// 成功路径要么全部确认（先确认者终结，取消方拒绝），要么全部取消。
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("并发后台账应可打开: %v (codes=%v errs=%v)", err, codes, errs)
	}
	m, err := s.ManifestQuery("M1")
	if err != nil {
		t.Fatalf("清单应存在: %v", err)
	}
	confirmed := m.ConfirmedBatch != ""
	cancelled := m.CancelledBy != ""
	if confirmed == cancelled {
		t.Fatalf("清单应恰好终结一次（确认或取消）: %+v", m)
	}
	for i, c := range codes {
		wantConfirm := i%2 == 0
		ok := c == exitOK
		if confirmed && wantConfirm && !ok {
			t.Fatalf("清单已确认，确认方应成功（重放或首次）: codes=%v errs=%v", codes, errs)
		}
		if confirmed && !wantConfirm && ok {
			t.Fatalf("清单已确认，取消方不得成功: codes=%v", codes)
		}
		if cancelled && !wantConfirm && !ok {
			t.Fatalf("清单已取消，取消方应成功（重放或首次）: codes=%v errs=%v", codes, errs)
		}
		if cancelled && wantConfirm && ok {
			t.Fatalf("清单已取消，确认方不得成功: codes=%v", codes)
		}
	}
}
