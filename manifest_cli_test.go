package main

import (
	"bytes"
	"strings"
	"testing"
)

func setupManifestLedger(t *testing.T, dbPath string) {
	t.Helper()
	for _, id := range []string{"P001", "P002", "P003"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败", id)
		}
	}
}

func TestCLIEndToEndManifestCreateConfirm(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	setupManifestLedger(t, dbPath)

	// 创建：成员按首次顺序保存，站点、状态不变。
	out, _, code := runCLI(t, dbPath, "manifest-create",
		"--manifest", " M1 ", "--station", "站点A",
		"--parcel", " P002 ", "--parcel", "P001")
	if code != 0 || !strings.Contains(out, "清单创建成功") ||
		!strings.Contains(out, "分拣清单号: M1") || !strings.Contains(out, "站点: 站点A") ||
		!strings.Contains(out, "当前状态: 待出站") ||
		!strings.Contains(out, "- P002\n  - P001") {
		t.Fatalf("清单创建输出不符: code=%d out=%s", code, out)
	}

	// query 显示当前预留与分拣预留轨迹。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P002")
	if code != 0 || !strings.Contains(out, "当前预留: 分拣清单号: M1") ||
		!strings.Contains(out, "当前状态: 在站") ||
		!strings.Contains(out, "操作: 分拣预留") || !strings.Contains(out, "分拣清单号: M1") {
		t.Fatalf("预留后 query 不符: code=%d out=%s", code, out)
	}

	// 预留件不能直接 dispatch，整批拒绝、不占批次号。
	_, errText, code := runCLI(t, dbPath, "dispatch",
		"--batch", "BX", "--station", "站点A", "--courier", "张三",
		"--parcel", "P003", "--parcel", "P001")
	if code != exitBusiness || !strings.Contains(errText, "分拣清单") {
		t.Fatalf("涉及预留件的 dispatch 应整批拒绝: code=%d err=%s", code, errText)
	}
	if _, _, code := runCLI(t, dbPath, "batch", "--id", "BX"); code != exitBusiness {
		t.Fatal("失败的 dispatch 不得占用批次号 BX")
	}

	// 预留期间允许冻结；冻结件导致确认整单拒绝。
	if _, _, code := runCLI(t, dbPath, "freeze",
		"--incident", "E1", "--parcel", "P001", "--station", "站点A", "--reason", "破损"); code != 0 {
		t.Fatal("预留件应允许冻结")
	}
	_, errText, code = runCLI(t, dbPath, "manifest-confirm",
		"--manifest", "M1", "--batch", "B1", "--courier", "张三")
	if code != exitBusiness || !strings.Contains(errText, "冻结件导致整单拒绝") {
		t.Fatalf("冻结件应导致确认整单拒绝: code=%d err=%s", code, errText)
	}
	if _, _, code := runCLI(t, dbPath, "batch", "--id", "B1"); code != exitBusiness {
		t.Fatal("确认失败不得占用批次号 B1")
	}
	if _, _, code := runCLI(t, dbPath, "unfreeze",
		"--request", "U1", "--incident", "E1", "--note", "放行"); code != 0 {
		t.Fatal("预留件应允许解除冻结")
	}

	// 确认出站成功：按原顺序创建普通批次，成员转配送中。
	out, _, code = runCLI(t, dbPath, "manifest-confirm",
		"--manifest", "M1", "--batch", "B1", "--courier", "张三")
	if code != 0 || !strings.Contains(out, "清单确认出站成功") ||
		!strings.Contains(out, "分拣清单号: M1") || !strings.Contains(out, "批次号: B1") ||
		!strings.Contains(out, "- P002\n  - P001") {
		t.Fatalf("确认输出不符: code=%d out=%s", code, out)
	}

	// 清单查询显示已出站与确认信息。
	out, _, code = runCLI(t, dbPath, "manifest", "--id", "M1")
	if code != 0 || !strings.Contains(out, "清单状态: 已出站") ||
		!strings.Contains(out, "确认批次号: B1") || !strings.Contains(out, "配送员: 张三") ||
		!strings.Contains(out, "- P002") || !strings.Contains(out, "创建时间:") {
		t.Fatalf("已出站清单查询不符: code=%d out=%s", code, out)
	}

	// 出站轨迹已追加，当前预留不再显示。
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P002")
	if !strings.Contains(out, "当前状态: 配送中") || !strings.Contains(out, "当前批次: B1") ||
		strings.Contains(out, "当前预留:") ||
		strings.Count(out, "操作: 出站") != 1 {
		t.Fatalf("确认后 query 不符:\n%s", out)
	}

	// 同清单同批次同配送员重放，不追加轨迹。
	out, _, code = runCLI(t, dbPath, "manifest-confirm",
		"--manifest", "M1", "--batch", "B1", "--courier", "张三")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("确认重放应返回首次结果: code=%d out=%s", code, out)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P002")
	if strings.Count(out, "操作: 出站") != 1 {
		t.Fatalf("确认重放不得追加出站轨迹:\n%s", out)
	}

	// 换批次或配送员确认冲突/拒绝。
	if _, errText, code := runCLI(t, dbPath, "manifest-confirm",
		"--manifest", "M1", "--batch", "B9", "--courier", "张三"); code != exitBusiness ||
		!strings.Contains(errText, "已出站") {
		t.Fatalf("已出站清单换新批次应拒绝: code=%d err=%s", code, errText)
	}
	if _, errText, code := runCLI(t, dbPath, "manifest-confirm",
		"--manifest", "M1", "--batch", "B1", "--courier", "李四"); code != exitBusiness ||
		!strings.Contains(errText, "冲突") {
		t.Fatalf("同批次换配送员应冲突: code=%d err=%s", code, errText)
	}

	// dispatch 可按该批次原内容重放。
	out, _, code = runCLI(t, dbPath, "dispatch",
		"--batch", "B1", "--station", "站点A", "--courier", "张三",
		"--parcel", "P001", "--parcel", "P002")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("dispatch 应能按确认批次原内容重放: code=%d out=%s", code, out)
	}

	// 后续配送作业照常：逐件回执。
	if _, _, code := runCLI(t, dbPath, "receipt",
		"--request", "RC1", "--batch", "B1", "--parcel", "P002", "--result", "签收"); code != 0 {
		t.Fatal("确认批次应可正常回执")
	}
}

func TestCLIEndToEndManifestCancel(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	setupManifestLedger(t, dbPath)

	if _, _, code := runCLI(t, dbPath, "manifest-create",
		"--manifest", "M1", "--station", "站点A", "--parcel", "P001"); code != 0 {
		t.Fatal("创建清单失败")
	}
	// 冻结成员后取消：冻结应保持，预留释放。
	if _, _, code := runCLI(t, dbPath, "freeze",
		"--incident", "E1", "--parcel", "P001", "--station", "站点A", "--reason", "破损"); code != 0 {
		t.Fatal("冻结失败")
	}

	out, _, code := runCLI(t, dbPath, "manifest-cancel",
		"--request", " MC1 ", "--manifest", "M1", "--reason", " 计划取消 ")
	if code != 0 || !strings.Contains(out, "清单取消成功") ||
		!strings.Contains(out, "取消请求号: MC1") || !strings.Contains(out, "分拣清单号: M1") ||
		!strings.Contains(out, "取消原因: 计划取消") || !strings.Contains(out, "- P001") {
		t.Fatalf("取消输出不符: code=%d out=%s", code, out)
	}

	// 取消不改变站点、状态或解除冻结。
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if !strings.Contains(out, "当前状态: 异常冻结") || strings.Contains(out, "当前预留:") ||
		!strings.Contains(out, "操作: 取消预留") ||
		!strings.Contains(out, "取消请求号: MC1") || !strings.Contains(out, "原因: 计划取消") {
		t.Fatalf("取消后 query 不符:\n%s", out)
	}

	// 清单查询显示已取消及取消信息。
	out, _, code = runCLI(t, dbPath, "manifest", "--id", "M1")
	if code != 0 || !strings.Contains(out, "清单状态: 已取消") ||
		!strings.Contains(out, "取消请求号: MC1") || !strings.Contains(out, "取消原因: 计划取消") {
		t.Fatalf("已取消清单查询不符: code=%d out=%s", code, out)
	}

	// 取消重放返回首次结果，不追加轨迹。
	out, _, code = runCLI(t, dbPath, "manifest-cancel",
		"--request", "MC1", "--manifest", "M1", "--reason", "计划取消")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("取消重放应返回首次结果: code=%d out=%s", code, out)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if strings.Count(out, "操作: 取消预留") != 1 {
		t.Fatalf("取消重放不得追加轨迹:\n%s", out)
	}

	// 换号再次取消同一清单拒绝；已取消清单不能确认。
	if _, errText, code := runCLI(t, dbPath, "manifest-cancel",
		"--request", "MC2", "--manifest", "M1", "--reason", "计划取消"); code != exitBusiness ||
		!strings.Contains(errText, "已取消") {
		t.Fatalf("换号再次取消应拒绝: code=%d err=%s", code, errText)
	}
	if _, errText, code := runCLI(t, dbPath, "manifest-confirm",
		"--manifest", "M1", "--batch", "B1", "--courier", "张三"); code != exitBusiness ||
		!strings.Contains(errText, "已取消") {
		t.Fatalf("已取消清单不能确认: code=%d err=%s", code, errText)
	}

	// 解冻后释放的成员可恢复正常在站作业（直接 dispatch）。
	if _, _, code := runCLI(t, dbPath, "unfreeze",
		"--request", "U1", "--incident", "E1", "--note", "放行"); code != 0 {
		t.Fatal("解除冻结失败")
	}
	if _, _, code := runCLI(t, dbPath, "dispatch",
		"--batch", "B1", "--station", "站点A", "--courier", "张三", "--parcel", "P001"); code != 0 {
		t.Fatal("取消释放后的成员应可直接 dispatch")
	}
}

func TestCLIManifestCreateReplayConflict(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	setupManifestLedger(t, dbPath)
	if _, _, code := runCLI(t, dbPath, "manifest-create",
		"--manifest", "M1", "--station", "站点A",
		"--parcel", "P002", "--parcel", "P001"); code != 0 {
		t.Fatal("创建失败")
	}

	// 同号同站点同集合换序：重放返回首次顺序。
	out, _, code := runCLI(t, dbPath, "manifest-create",
		"--manifest", "M1", "--station", "站点A",
		"--parcel", "P001", "--parcel", "P002")
	if code != 0 || !strings.Contains(out, "返回首次保存的成员顺序与时间") ||
		!strings.Contains(out, "- P002\n  - P001") {
		t.Fatalf("换序重放应返回首次顺序: code=%d out=%s", code, out)
	}
	// 重放不追加轨迹。
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if strings.Count(out, "操作: 分拣预留") != 1 {
		t.Fatalf("重放不得追加预留轨迹:\n%s", out)
	}

	// 同号换集合/站点冲突。
	if _, errText, code := runCLI(t, dbPath, "manifest-create",
		"--manifest", "M1", "--station", "站点A", "--parcel", "P001"); code != exitBusiness ||
		!strings.Contains(errText, "冲突") {
		t.Fatalf("同号换集合应冲突: code=%d err=%s", code, errText)
	}
	// 取消请求号同号换内容冲突。
	if _, _, code := runCLI(t, dbPath, "manifest-cancel",
		"--request", "MCX", "--manifest", "M1", "--reason", "原因一"); code != 0 {
		t.Fatal("首次取消应成功")
	}
	if _, errText, code := runCLI(t, dbPath, "manifest-cancel",
		"--request", "MCX", "--manifest", "M1", "--reason", "原因二"); code != exitBusiness ||
		!strings.Contains(errText, "冲突") {
		t.Fatalf("同取消号换原因应冲突: code=%d err=%s", code, errText)
	}
}

func TestCLIManifestValidation(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	setupManifestLedger(t, dbPath)

	cases := []struct {
		args   []string
		errSub string
	}{
		{[]string{"manifest-create", "--manifest", "  ", "--station", "站点A", "--parcel", "P001"}, "不可为空"},
		{[]string{"manifest-create", "--manifest", "M1", "--station", " ", "--parcel", "P001"}, "不可为空"},
		{[]string{"manifest-create", "--manifest", "M1", "--station", "站点A"}, "不可为空"},
		{[]string{"manifest-create", "--manifest", "M1", "--station", "站点A",
			"--parcel", "P001", "--parcel", "P001"}, "重复"},
		{[]string{"manifest-confirm", "--manifest", "  ", "--batch", "B1", "--courier", "张三"}, "不可为空"},
		{[]string{"manifest-confirm", "--manifest", "M1", "--batch", " ", "--courier", "张三"}, "不可为空"},
		{[]string{"manifest-confirm", "--manifest", "M1", "--batch", "B1", "--courier", "  "}, "不可为空"},
		{[]string{"manifest-cancel", "--request", "  ", "--manifest", "M1", "--reason", "x"}, "不可为空"},
		{[]string{"manifest-cancel", "--request", "C1", "--manifest", " ", "--reason", "x"}, "不可为空"},
		{[]string{"manifest-cancel", "--request", "C1", "--manifest", "M1", "--reason", " \t "}, "不可为空"},
		{[]string{"manifest", "--id", " "}, "不可为空"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		full := append([]string{"--data", dbPath}, c.args...)
		code := run(full, &out, &errb)
		if code != exitBusiness || !strings.Contains(errb.String(), c.errSub) {
			t.Fatalf("%v 应失败(1)且提示 %q，得到 code=%d err=%q", c.args, c.errSub, code, errb.String())
		}
	}

	// 不存在的清单查询报错。
	if _, errText, code := runCLI(t, dbPath, "manifest", "--id", "NOPE"); code != exitBusiness ||
		!strings.Contains(errText, "不存在") {
		t.Fatalf("查询不存在清单应报错(1): code=%d err=%s", code, errText)
	}
}

// 确认与取消并发竞争同一待出站清单：至多一方成功；另一方以状态码 1 退出且不占号。
// 反复多轮以覆盖两种胜出顺序。
func TestConcurrentManifestConfirmVsCancel(t *testing.T) {
	for iter := 0; iter < 8; iter++ {
		dbPath := t.TempDir() + "/ledger.json"
		setupManifestLedger(t, dbPath)
		if _, _, code := runCLI(t, dbPath, "manifest-create",
			"--manifest", "M1", "--station", "站点A",
			"--parcel", "P001", "--parcel", "P002"); code != 0 {
			t.Fatal("创建清单失败")
		}

		_, errs, codes := runParallel(t, 2, func(i int) []string {
			if i == 0 {
				return []string{"--data", dbPath, "manifest-confirm",
					"--manifest", "M1", "--batch", "B1", "--courier", "张三"}
			}
			return []string{"--data", dbPath, "manifest-cancel",
				"--request", "MC1", "--manifest", "M1", "--reason", "计划取消"}
		})
		if got := countCodes(codes, exitOK); got != 1 {
			t.Fatalf("第 %d 轮：确认与取消应恰好一方成功，成功 %d 次（codes=%v errs=%v）",
				iter, got, codes, errs)
		}

		s, err := Open(dbPath)
		if err != nil {
			t.Fatalf("打开台账失败: %v", err)
		}
		m, err := s.ManifestQuery("M1")
		if err != nil {
			t.Fatalf("清单查询失败: %v", err)
		}
		switch {
		case codes[0] == exitOK:
			// 确认胜出：清单已出站，成员配送中；取消号不占。
			if m.Status != manifestConfirmed {
				t.Fatalf("第 %d 轮：确认胜出后清单应已出站，得到 %q", iter, m.Status)
			}
			if s.data.ManifestCancels["MC1"] != nil {
				t.Fatalf("第 %d 轮：失败的取消不得占用 MC1", iter)
			}
			for _, pid := range []string{"P001", "P002"} {
				p, _ := s.Query(pid)
				if p.Status != statusDelivering || currentBatch(p) != "B1" {
					t.Fatalf("第 %d 轮：确认胜出后 %s 应随 B1 配送中: %+v", iter, pid, p)
				}
			}
			// 取消胜出后再取消：拒绝；确认重放仍返回首次结果。
			if _, _, err := s.CancelManifest("MC2", "M1", "再取消", tClock(2026, 10, 5, 12, 0)); err == nil {
				t.Fatalf("第 %d 轮：确认胜出后取消应拒绝", iter)
			}
			if _, replayed, err := s.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 12, 1)); err != nil || !replayed {
				t.Fatalf("第 %d 轮：确认胜出后同内容应可重放: %v replayed=%v", iter, err, replayed)
			}
		default:
			// 取消胜出：清单已取消，成员在站且预留释放；批次号不占。
			if m.Status != manifestCancelled {
				t.Fatalf("第 %d 轮：取消胜出后清单应已取消，得到 %q", iter, m.Status)
			}
			if _, err := s.BatchQuery("B1"); err == nil {
				t.Fatalf("第 %d 轮：失败的确认不得占用批次号 B1", iter)
			}
			for _, pid := range []string{"P001", "P002"} {
				p, _ := s.Query(pid)
				if p.Status != statusInStation || s.ActiveManifest(pid) != nil {
					t.Fatalf("第 %d 轮：取消胜出后 %s 应在站且预留释放: %+v", iter, pid, p)
				}
			}
			// 确认胜出失败后再确认：拒绝；取消重放仍返回首次结果。
			if _, _, err := s.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 12, 0)); err == nil {
				t.Fatalf("第 %d 轮：取消胜出后确认应拒绝", iter)
			}
			if _, replayed, err := s.CancelManifest("MC1", "M1", "计划取消", tClock(2026, 10, 5, 12, 1)); err != nil || !replayed {
				t.Fatalf("第 %d 轮：取消胜出后同内容应可重放: %v replayed=%v", iter, err, replayed)
			}
		}
	}
}

func TestCLIManifestHelpVariants(t *testing.T) {
	for _, args := range [][]string{
		{"manifest-create", "--help"},
		{"manifest-confirm", "-h"},
		{"manifest-cancel", "--help"},
		{"manifest", "-h"},
	} {
		var out, errb bytes.Buffer
		code := run(args, &out, &errb)
		if code != 0 || !strings.Contains(out.String(), "manifest") || errb.Len() != 0 {
			t.Fatalf("%v 帮助应以 0 退出且不含 stderr: code=%d", args, code)
		}
	}
	// 总帮助包含全部清单命令。
	var buf bytes.Buffer
	code := run(nil, &buf, &buf)
	if code != 0 || !strings.Contains(buf.String(), "manifest-create") ||
		!strings.Contains(buf.String(), "manifest-confirm") ||
		!strings.Contains(buf.String(), "manifest-cancel") {
		t.Fatalf("总帮助应包含清单命令: code=%d", code)
	}
}
