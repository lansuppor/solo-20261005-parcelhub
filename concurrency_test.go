package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// runParallel 从 n 个“终端”同时执行同一条命令（各自带不同的参数），
// 返回每次调用的 stdout、stderr 与退出码。各调用共享同一数据文件路径。
func runParallel(t *testing.T, n int, mkArgs func(i int) []string) (outs, errs []string, codes []int) {
	t.Helper()
	outs, errs, codes = make([]string, n), make([]string, n), make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			var out, errb strings.Builder
			codes[i] = run(mkArgs(i), &out, &errb)
			outs[i], errs[i] = out.String(), errb.String()
		}(i)
	}
	close(start)
	wg.Wait()
	return outs, errs, codes
}

func countCodes(codes []int, want int) int {
	n := 0
	for _, c := range codes {
		if c == want {
			n++
		}
	}
	return n
}

// 并发登记同一包裹：只能成功一次，其余重复登记退出 1；台账中只有一条收件记录。
func TestConcurrentRegisterSameParcel(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	const n = 8
	_, errs, codes := runParallel(t, n, func(i int) []string {
		return []string{"--data", dbPath, "register", "--id", "P001", "--station", "站点A"}
	})
	if got := countCodes(codes, exitOK); got != 1 {
		t.Fatalf("并发登记同一包裹应恰好成功一次，成功 %d 次（codes=%v errs=%v）", got, codes, errs)
	}
	if got := countCodes(codes, exitBusiness); got != n-1 {
		t.Fatalf("其余登记应以状态码 1 退出，得到 %d 个（codes=%v）", got, codes)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	p, err := s.Query("P001")
	if err != nil {
		t.Fatalf("P001 应已登记: %v", err)
	}
	if len(p.Trail) != 1 || p.Trail[0].Op != "收件" {
		t.Fatalf("P001 应只有一条收件记录，得到 %+v", p.Trail)
	}
}

// 并发登记不同包裹：合法操作全部保留，不能两边都报成功却丢掉一边的数据。
func TestConcurrentRegisterDifferentParcels(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	const n = 12
	_, errs, codes := runParallel(t, n, func(i int) []string {
		return []string{"--data", dbPath, "register", "--id", string(rune('A'+i)) + "P", "--station", "站点A"}
	})
	if got := countCodes(codes, exitOK); got != n {
		t.Fatalf("不同包裹的并发登记应全部成功，成功 %d 次（errs=%v）", got, errs)
	}
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	if len(s.data.Parcels) != n {
		t.Fatalf("台账应保留全部 %d 件包裹，实际 %d 件", n, len(s.data.Parcels))
	}
}

// 首次创建不存在的台账也遵守并发协调：两个终端同时首次登记同一包裹，只成功一次。
func TestConcurrentFirstCreation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sub", "dir", "ledger.json")
	_, _, codes := runParallel(t, 2, func(i int) []string {
		return []string{"--data", dbPath, "register", "--id", "P001", "--station", "站点A"}
	})
	if got := countCodes(codes, exitOK); got != 1 {
		t.Fatalf("首次创建的台账并发登记同一包裹应恰好成功一次，成功 %d 次", got)
	}
}

// 同号同内容的并发交接：同号同内容只生效一次，其他提交返回首次结果；
// 轨迹中只有一条交接记录。
func TestConcurrentHandoffSameRequest(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	if _, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A"); code != 0 {
		t.Fatalf("登记失败")
	}
	const n = 4
	outs, _, codes := runParallel(t, n, func(i int) []string {
		return []string{"--data", dbPath, "handoff", "--request", "R1",
			"--from", "站点A", "--to", "站点B", "--parcel", "P001"}
	})
	if got := countCodes(codes, exitOK); got != n {
		t.Fatalf("同号同内容的并发交接应全部返回成功，成功 %d 次", got)
	}
	replays := 0
	for _, out := range outs {
		if strings.Contains(out, "返回首次保存的结果") {
			replays++
		}
	}
	if replays != n-1 {
		t.Fatalf("应有 %d 次重放首次结果，实际 %d 次", n-1, replays)
	}
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	p, _ := s.Query("P001")
	handoffs := 0
	for _, e := range p.Trail {
		if e.Op == "交接" {
			handoffs++
		}
	}
	if handoffs != 1 {
		t.Fatalf("同号同内容只应生效一次（一条交接轨迹），实际 %d 条", handoffs)
	}
}

// 对同件包裹同时首次冻结和首次出站：至多一方成功；
// 包含该件的失败出站须整批不变（批次号不占用、轨迹不追加）。
func TestConcurrentFreezeVsDispatch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	if _, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A"); code != 0 {
		t.Fatalf("登记失败")
	}
	_, _, codes := runParallel(t, 2, func(i int) []string {
		if i == 0 {
			return []string{"--data", dbPath, "freeze", "--incident", "E1",
				"--parcel", "P001", "--station", "站点A", "--reason", "外包装破损"}
		}
		return []string{"--data", dbPath, "dispatch", "--batch", "B1",
			"--station", "站点A", "--courier", "张三", "--parcel", "P001"}
	})
	if got := countCodes(codes, exitOK); got != 1 {
		t.Fatalf("同时首次冻结与首次出站应至多一方成功，成功 %d 次（codes=%v）", got, codes)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	p, _ := s.Query("P001")
	switch {
	case codes[0] == exitOK:
		// 冻结成功：包裹冻结，批次 B1 不得存在，轨迹不含出站。
		if p.Status != statusFrozen {
			t.Fatalf("冻结成功后包裹应为异常冻结，得到 %q", p.Status)
		}
		if _, err := s.BatchQuery("B1"); err == nil {
			t.Fatalf("失败的出站不得占用批次号 B1")
		}
		for _, e := range p.Trail {
			if e.Op == "出站" {
				t.Fatalf("失败的出站不得追加出站轨迹: %+v", p.Trail)
			}
		}
	default:
		// 出站成功：包裹配送中，异常单 E1 不得存在，轨迹不含冻结。
		if p.Status != statusDelivering {
			t.Fatalf("出站成功后包裹应为配送中，得到 %q", p.Status)
		}
		if s.data.Freezes["E1"] != nil {
			t.Fatalf("失败的冻结不得占用异常单号 E1")
		}
		for _, e := range p.Trail {
			if e.Op == "冻结" {
				t.Fatalf("失败的冻结不得追加冻结轨迹: %+v", p.Trail)
			}
		}
	}
}

// 并发回执不同包裹（同一批次）：合法操作全部保留，批次两件都回执。
func TestConcurrentReceiptsSameBatch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	for _, id := range []string{"P001", "P002"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败", id)
		}
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B1",
		"--station", "站点A", "--courier", "张三", "--parcel", "P001", "--parcel", "P002"); code != 0 {
		t.Fatalf("出站失败")
	}
	_, errs, codes := runParallel(t, 2, func(i int) []string {
		return []string{"--data", dbPath, "receipt", "--request", "RC" + string(rune('1'+i)),
			"--batch", "B1", "--parcel", "P00" + string(rune('1'+i)), "--result", "签收"}
	})
	if got := countCodes(codes, exitOK); got != 2 {
		t.Fatalf("不同包裹的并发回执应全部成功，成功 %d 次（errs=%v）", got, errs)
	}
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	b, err := s.BatchQuery("B1")
	if err != nil {
		t.Fatalf("批次查询失败: %v", err)
	}
	if !b.Done() || b.Effective() != 2 {
		t.Fatalf("两件回执都应保留并完成批次，有效回执 %d/2", b.Effective())
	}
}

// 同一签收同时办理实物退件与撤销：竞争至多一方成功；另一方以状态码 1 退出
// 且不占用编号。反复多次以覆盖两种胜出顺序。
func TestConcurrentReceiptReturnVsRevoke(t *testing.T) {
	for iter := 0; iter < 6; iter++ {
		dbPath := filepath.Join(t.TempDir(), "ledger.json")
		if _, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A"); code != 0 {
			t.Fatalf("登记失败")
		}
		if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B1",
			"--station", "站点A", "--courier", "张三", "--parcel", "P001"); code != 0 {
			t.Fatalf("出站失败")
		}
		if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC1",
			"--batch", "B1", "--parcel", "P001", "--result", "签收"); code != 0 {
			t.Fatalf("签收失败")
		}
		_, errs, codes := runParallel(t, 2, func(i int) []string {
			if i == 0 {
				return []string{"--data", dbPath, "receipt-return", "--request", "RR1",
					"--receipt", "RC1", "--reason", "客户退货"}
			}
			return []string{"--data", dbPath, "receipt-revoke", "--request", "RV1",
				"--receipt", "RC1", "--reason", "误录签收"}
		})
		if got := countCodes(codes, exitOK); got != 1 {
			t.Fatalf("第 %d 轮：退件与撤销应恰好一方成功，成功 %d 次（codes=%v errs=%v）", iter, got, codes, errs)
		}

		s, err := Open(dbPath)
		if err != nil {
			t.Fatalf("打开台账失败: %v", err)
		}
		p, _ := s.Query("P001")
		switch {
		case codes[0] == exitOK:
			// 退件胜出：包裹在站、末条退件轨迹；签收未撤销且标记退件；撤销号不占。
			if p.Status != statusInStation || p.Station != "站点A" {
				t.Fatalf("第 %d 轮：退件胜出后应在站: %+v", iter, p)
			}
			if p.Trail[len(p.Trail)-1].Op != "退件" {
				t.Fatalf("第 %d 轮：退件胜出后末条应为退件: %+v", iter, p.Trail)
			}
			rc := s.ReceiptOf("RC1")
			if rc.RevokedBy != "" || rc.ReturnedBy != "RR1" {
				t.Fatalf("第 %d 轮：退件胜出后签收应标记退件未撤销: %+v", iter, rc)
			}
			if s.data.Revokes["RV1"] != nil {
				t.Fatalf("第 %d 轮：失败的撤销不得占用 RV1", iter)
			}
			if _, _, err := s.RevokeReceipt("RV1", "RC1", "误录签收", tClock(2026, 10, 5, 12, 0)); err == nil {
				t.Fatalf("第 %d 轮：退件胜出后再次撤销仍应拒绝", iter)
			}
		default:
			// 撤销胜出：包裹配送中、末条撤销轨迹；退件号不占。
			if p.Status != statusDelivering || p.Station != "站点A" {
				t.Fatalf("第 %d 轮：撤销胜出后应恢复配送中: %+v", iter, p)
			}
			if p.Trail[len(p.Trail)-1].Op != "撤销回执" {
				t.Fatalf("第 %d 轮：撤销胜出后末条应为撤销回执: %+v", iter, p.Trail)
			}
			rc := s.ReceiptOf("RC1")
			if rc.ReturnedBy != "" || rc.RevokedBy != "RV1" {
				t.Fatalf("第 %d 轮：撤销胜出后签收应标记撤销无退件: %+v", iter, rc)
			}
			if s.data.ReceiptReturns["RR1"] != nil {
				t.Fatalf("第 %d 轮：失败的退件不得占用 RR1", iter)
			}
		}
	}
}

// 同一路径的相对、绝对及含 .、.. 的写法须归一到同一协调文件。
func TestCoordinationPathForms(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "ledger.json")
	forms := []string{
		base,
		filepath.Join(dir, ".", "ledger.json"),
		filepath.Join(dir, "sub", "..", "ledger.json"),
	}
	var want string
	for i, f := range forms {
		got, err := coordinationPath(f)
		if err != nil {
			t.Fatalf("coordinationPath(%q) 失败: %v", f, err)
		}
		if i == 0 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("路径写法 %q 的协调文件 %q 与 %q 不一致", f, got, want)
		}
	}

	// 相对路径与绝对路径也归一到同一把锁。
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取工作目录失败: %v", err)
	}
	rel, err := filepath.Rel(cwd, base)
	if err != nil {
		t.Fatalf("计算相对路径失败: %v", err)
	}
	got, err := coordinationPath(rel)
	if err != nil {
		t.Fatalf("coordinationPath(%q) 失败: %v", rel, err)
	}
	if got != want {
		t.Fatalf("相对路径 %q 的协调文件 %q 与绝对路径的 %q 不一致", rel, got, want)
	}
}

// 不同路径写法按同一台账协调：相对路径登记后，含 .、.. 的绝对写法
// 看到的仍是同一份台账（重复登记退出 1）。
func TestPathFormsShareLedger(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if _, _, code := runCLI(t, "ledger.json", "register", "--id", "P001", "--station", "站点A"); code != 0 {
		t.Fatalf("相对路径登记失败")
	}
	alt := filepath.Join(dir, ".", "sub", "..", "ledger.json")
	_, errText, code := runCLI(t, alt, "register", "--id", "P001", "--station", "站点B")
	if code != exitBusiness || !strings.Contains(errText, "已登记") {
		t.Fatalf("含 .、.. 的写法应识别为同一台账并拒绝重复登记: code=%d err=%s", code, errText)
	}
}

// 持有协调锁的进程被强制终止（相当于文件描述符被直接关闭）后，
// 其他终端无需人工删除协调文件即可继续作业。
func TestLockReleasedAfterAbruptTermination(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	lockPath, err := coordinationPath(dbPath)
	if err != nil {
		t.Fatalf("coordinationPath 失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	// 模拟另一个进程持锁后被 kill -9：锁随描述符关闭由系统释放，协调文件仍在。
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("打开协调文件失败: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("加锁失败: %v", err)
	}
	_ = f.Close() // 进程终止：不主动解锁、不删除协调文件

	if _, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A"); code != 0 {
		t.Fatalf("持有者异常终止后应无需人工清理即可继续登记: code=%d", code)
	}
}

// query 不加锁、不改写数据文件，也不会产生协调文件。
func TestQueryDoesNotTouchFiles(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.json")
	if _, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A"); code != 0 {
		t.Fatalf("登记失败")
	}
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("读取数据文件失败: %v", err)
	}
	if _, _, code := runCLI(t, dbPath, "query", "--id", "P001"); code != 0 {
		t.Fatalf("查询失败")
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("读取数据文件失败: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("查询不得改写数据文件")
	}

	// 对不存在的台账查询：既不建数据文件也不建协调文件。
	ghost := filepath.Join(t.TempDir(), "ghost.json")
	if _, _, code := runCLI(t, ghost, "query", "--id", "P001"); code != exitBusiness {
		t.Fatalf("查询不存在的包裹应以 1 退出，得到 %d", code)
	}
	if _, err := os.Stat(ghost); !os.IsNotExist(err) {
		t.Fatalf("查询不得创建数据文件")
	}
	lockPath, _ := coordinationPath(ghost)
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("查询不得创建协调文件")
	}
}
