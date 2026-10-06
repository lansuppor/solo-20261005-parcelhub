package main

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// testBinary 是 TestMain 在启动时构建的 parcelhub 可执行文件，供多进程并发测试。
var testBinary string

// 这两个环境变量用于让测试二进制以“持锁子进程”方式重新执行自己：
// 设置 PARCELHUB_TEST_LOCK_DIR 后，进程取得该目录台账的排他协调锁，
// 打印 READY 后一直睡眠（等待父进程将其强制终止）。
const (
	envLockTestDir  = "PARCELHUB_TEST_LOCK_DIR"
	lockReadyMarker = "READY"
)

// TestMain 在普通测试前构建一次 parcelhub 二进制；持锁子进程模式下直接持锁睡眠。
func TestMain(m *testing.M) {
	if dir := os.Getenv(envLockTestDir); dir != "" {
		holdLockAndSleep(dir)
		return
	}
	buildDir, err := os.MkdirTemp("", "parcelhub-bin")
	if err != nil {
		panic(err)
	}
	bin := filepath.Join(buildDir, "parcelhub")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		panic(err)
	}
	testBinary = bin
	code := m.Run()
	os.RemoveAll(buildDir)
	os.Exit(code)
}

// holdLockAndSleep 是被父进程重新执行的测试二进制入口：取得排他协调锁后睡眠。
func holdLockAndSleep(dir string) {
	lock, err := acquireLedgerLock(filepath.Join(dir, "ledger.json"), lockModeWrite)
	if err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	defer lock.release()
	os.Stdout.WriteString(lockReadyMarker + "\n")
	time.Sleep(60 * time.Second)
}

// runBinary 运行一次真实的 parcelhub 子进程，返回标准输出、标准错误与退出码。
func runBinary(t *testing.T, dataPath string, args ...string) (string, string, int) {
	t.Helper()
	full := append([]string{"--data", dataPath}, args...)
	cmd := exec.Command(testBinary, full...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err == nil {
		return out.String(), errb.String(), exitOK
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return out.String(), errb.String(), ee.ExitCode()
	}
	// 仅供测试期排查：子进程都在 wg.Wait() 内运行，panic 会使本测试失败。
	panic("运行 parcelhub 失败: " + err.Error())
}

// startLockHolder 启动持锁子进程并等待其 READY：返回时它已持有排他协调锁。
func startLockHolder(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), envLockTestDir+"="+dir)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(pipe)
	line, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		t.Fatalf("等待持锁子进程就绪失败: %v", err)
	}
	if strings.TrimSpace(line) != lockReadyMarker {
		t.Fatalf("持锁子进程未正常就绪，输出 %q", line)
	}
	return cmd
}

// tryLockNB 以非阻塞方式尝试取得协调锁：取得时返回释放函数与 true。
func tryLockNB(t *testing.T, dataPath, mode string) (func(), bool) {
	t.Helper()
	canonical, err := canonicalLedgerPath(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lockPathFor(canonical), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	how := syscall.LOCK_NB
	if mode == lockModeWrite {
		how |= syscall.LOCK_EX
	} else {
		how |= syscall.LOCK_SH
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, false
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true
}

func TestCanonicalLedgerPathAliases(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "ledger.json")
	if err := os.WriteFile(real, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// t.TempDir() 在 macOS 下可能经 /var -> /private/var 符号链接；期望值同样归一。
	expected, err := canonicalLedgerPath(real)
	if err != nil {
		t.Fatal(err)
	}
	// 在另一目录建一个指向台账的符号链接。
	link := filepath.Join(sub, "alias.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	aliases := []string{
		real,
		filepath.Join(dir, ".", "ledger.json"),
		filepath.Join(sub, "..", "ledger.json"),
		filepath.Join(sub, "..", ".", "ledger.json"),
		link,
		filepath.Join(sub, ".", "alias.json"),
	}
	for _, a := range aliases {
		got, err := canonicalLedgerPath(a)
		if err != nil {
			t.Fatalf("canonicalLedgerPath(%q) 失败: %v", a, err)
		}
		if got != expected {
			t.Fatalf("路径 %q 应归一为 %q，得到 %q", a, expected, got)
		}
	}
}

func TestLockCoordinatesAcrossPathAliases(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, "ledger.json")

	holder, err := acquireLedgerLock(ledger, lockModeWrite)
	if err != nil {
		t.Fatalf("取得协调锁失败: %v", err)
	}

	// 同一路径的相对成分写法（含 . 与 ..）必须与持有者互斥。
	for _, alias := range []string{
		filepath.Join(dir, ".", "ledger.json"),
		filepath.Join(sub, "..", "ledger.json"),
	} {
		if release, ok := tryLockNB(t, alias, lockModeWrite); ok {
			release()
			t.Fatalf("通过别名 %q 不应在排他锁持有期间取得锁", alias)
		}
		if release, ok := tryLockNB(t, alias, lockModeRead); ok {
			release()
			t.Fatalf("通过别名 %q 不应在排他锁持有期间取得共享锁", alias)
		}
	}

	holder.release()
	for _, alias := range []string{ledger, filepath.Join(sub, "..", ".", "ledger.json")} {
		release, ok := tryLockNB(t, alias, lockModeRead)
		if !ok {
			t.Fatalf("释放后通过别名 %q 应能取得锁", alias)
		}
		release()
	}
}

func TestSharedLocksCoexistAndBlockWriters(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "ledger.json")
	r1, err := acquireLedgerLock(ledger, lockModeRead)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.release()
	r2, err := acquireLedgerLock(ledger, lockModeRead)
	if err != nil {
		t.Fatalf("两个共享锁应可同时持有: %v", err)
	}
	defer r2.release()
	if _, ok := tryLockNB(t, ledger, lockModeWrite); ok {
		t.Fatal("共享锁持有期间排他锁必须等待")
	}
}

func TestParallelRegisterSameParcelSucceedsOnce(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "ledger.json")

	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	errs := make([]string, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, e, c := runBinary(t, ledger, "register", "--id", "P1", "--station", "站点A")
			codes[i], errs[i] = c, e
		}(i)
	}
	close(start)
	wg.Wait()

	success, duplicate := 0, 0
	for i, c := range codes {
		switch c {
		case exitOK:
			success++
		case exitBusiness:
			if !strings.Contains(errs[i], "已登记") {
				t.Fatalf("失败的并行登记应提示重复登记，得到: %s", errs[i])
			}
			duplicate++
		default:
			t.Fatalf("并行登记只允许退出 0/1，得到 %d: %s", c, errs[i])
		}
	}
	if success != 1 || duplicate != n-1 {
		t.Fatalf("同一包裹并发登记应恰好 1 次成功、%d 次重复失败，得到成功 %d、重复 %d", n-1, success, duplicate)
	}

	out, _, code := runBinary(t, ledger, "query", "--id", "P1")
	if code != 0 || strings.Count(out, "操作: 收件") != 1 {
		t.Fatalf("并发登记后应只剩一条收件记录: code=%d out=%s", code, out)
	}
}

func TestParallelRegisterDifferentParcelsAllKept(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "ledger.json") // 刻意不预先创建：首次创建也受协调

	const n = 24
	var wg sync.WaitGroup
	codes := make([]int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id := "P" + strconv.Itoa(i)
			_, _, codes[i] = runBinary(t, ledger, "register", "--id", id, "--station", "站点A")
		}(i)
	}
	close(start)
	wg.Wait()

	for i, c := range codes {
		if c != exitOK {
			t.Fatalf("不同包裹 P%d 的合法登记应全部成功，P%d 得到退出码 %d", i, i, c)
		}
	}
	for i := 0; i < n; i++ {
		out, _, code := runBinary(t, ledger, "query", "--id", "P"+strconv.Itoa(i))
		if code != 0 || !strings.Contains(out, "当前站点: 站点A") {
			t.Fatalf("并发登记的 P%d 未完整保留: code=%d out=%s", i, code, out)
		}
	}
}

func TestParallelSameRequestHandoffOneNewOneReplay(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "ledger.json")
	for _, id := range []string{"P1", "P2"} {
		if _, _, c := runBinary(t, ledger, "register", "--id", id, "--station", "站点A"); c != 0 {
			t.Fatalf("登记 %s 失败", id)
		}
	}

	const n = 8
	var wg sync.WaitGroup
	outs := make([]string, n)
	codes := make([]int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			o, _, c := runBinary(t, ledger, "handoff",
				"--request", "R1", "--from", "站点A", "--to", "站点B",
				"--parcel", "P1", "--parcel", "P2")
			outs[i], codes[i] = o, c
		}(i)
	}
	close(start)
	wg.Wait()

	newCount, replayCount := 0, 0
	for i, c := range codes {
		if c != exitOK {
			t.Fatalf("同号同内容的并提交接都应成功（1 次新增、其余重放），第 %d 个退出 %d", i, c)
		}
		if strings.Contains(outs[i], "返回首次保存的结果") {
			replayCount++
		} else {
			newCount++
		}
	}
	if newCount != 1 || replayCount != n-1 {
		t.Fatalf("并提交接应恰好 1 次新增、%d 次重放，得到新增 %d、重放 %d", n-1, newCount, replayCount)
	}
	out, _, _ := runBinary(t, ledger, "query", "--id", "P1")
	if strings.Count(out, "操作: 交接") != 1 {
		t.Fatalf("并提交接后 P1 应只有一条交接记录:\n%s", out)
	}
}

func TestParallelFreezeVsDispatchAtMostOneSucceeds(t *testing.T) {
	for iter := 0; iter < 8; iter++ {
		dir := t.TempDir()
		ledger := filepath.Join(dir, "ledger.json")
		for _, id := range []string{"P1", "P2"} {
			if _, _, c := runBinary(t, ledger, "register", "--id", id, "--station", "站点A"); c != 0 {
				t.Fatalf("iter %d: 登记 %s 失败", iter, id)
			}
		}

		var wg sync.WaitGroup
		var freezeCode, dispatchCode int
		var dispatchErr string
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, freezeCode = runBinary(t, ledger, "freeze",
				"--incident", "E1", "--parcel", "P1", "--station", "站点A", "--reason", "破损")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, dispatchErr, dispatchCode = runBinary(t, ledger, "dispatch",
				"--batch", "B1", "--station", "站点A", "--courier", "张三",
				"--parcel", "P1", "--parcel", "P2")
		}()
		close(start)
		wg.Wait()

		if freezeCode == dispatchCode {
			t.Fatalf("iter %d: 同时首次冻结与首次出站结果必须一胜一负，得到 freeze=%d dispatch=%d (%s)",
				iter, freezeCode, dispatchCode, dispatchErr)
		}

		outP1, _, _ := runBinary(t, ledger, "query", "--id", "P1")
		outP2, _, _ := runBinary(t, ledger, "query", "--id", "P2")
		switch {
		case freezeCode == exitOK:
			// 冻结胜出：失败出站必须整批不变——两件都无出站记录，P2 仍在站。
			if !strings.Contains(outP1, "当前状态: 异常冻结") || strings.Contains(outP1, "操作: 出站") {
				t.Fatalf("iter %d: 冻结胜出但 P1 状态/轨迹不符:\n%s", iter, outP1)
			}
			if !strings.Contains(outP2, "当前状态: 在站") || strings.Contains(outP2, "操作: 出站") {
				t.Fatalf("iter %d: 冻结胜出时失败出站必须整批不变，P2 被改动:\n%s", iter, outP2)
			}
			if _, _, c := runBinary(t, ledger, "batch", "--id", "B1"); c != exitBusiness {
				t.Fatalf("iter %d: 失败出站不得占用批次号 B1", iter)
			}
		case dispatchCode == exitOK:
			// 出站胜出：冻结失败不留记录，两件都在批次 B1 配送中。
			if !strings.Contains(outP1, "当前状态: 配送中") || strings.Contains(outP1, "操作: 冻结") {
				t.Fatalf("iter %d: 出站胜出但 P1 状态/轨迹不符:\n%s", iter, outP1)
			}
			if !strings.Contains(outP2, "当前状态: 配送中") || strings.Contains(outP2, "操作: 冻结") {
				t.Fatalf("iter %d: 出站胜出但 P2 状态/轨迹不符:\n%s", iter, outP2)
			}
			if _, _, c := runBinary(t, ledger, "freeze",
				"--incident", "E1", "--parcel", "P1", "--station", "站点A", "--reason", "破损"); c != exitBusiness {
				t.Fatalf("iter %d: 失败冻结不得占用异常单号（配送中也不能冻结）", iter)
			}
		}
	}
}

func TestParallelDispatchSameParcelOneBatchWins(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "ledger.json")
	if _, _, c := runBinary(t, ledger, "register", "--id", "P1", "--station", "站点A"); c != 0 {
		t.Fatal("登记失败")
	}

	var wg sync.WaitGroup
	codes := make([]int, 2)
	start := make(chan struct{})
	for i, batch := range []string{"B1", "B2"} {
		wg.Add(1)
		go func(i int, batch string) {
			defer wg.Done()
			<-start
			_, _, codes[i] = runBinary(t, ledger, "dispatch",
				"--batch", batch, "--station", "站点A", "--courier", "张三", "--parcel", "P1")
		}(i, batch)
	}
	close(start)
	wg.Wait()
	if codes[0] == codes[1] {
		t.Fatalf("同一包裹的两个并发批次必须一胜一负，得到 %v", codes)
	}
	wins := 0
	for _, b := range []string{"B1", "B2"} {
		if _, _, c := runBinary(t, ledger, "batch", "--id", b); c == exitOK {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("失败出站不得占用批次号，应恰好 1 个批次存在，得到 %d", wins)
	}
}

func TestLockAutoRecoversAfterForceKill(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "ledger.json")
	if _, _, c := runBinary(t, ledger, "register", "--id", "P1", "--station", "站点A"); c != 0 {
		t.Fatal("预置台账失败")
	}

	holder := startLockHolder(t, dir)
	// 持有者在 READY 后一直占着排他锁；此时新终端必须等待，不能越过。
	if release, ok := tryLockNB(t, ledger, lockModeWrite); ok {
		release()
		t.Fatal("持锁进程存活期间不应取得锁")
	}

	// 先启动一个真实查询（会阻塞在协调锁上），再强制终止持锁进程。
	queryDone := make(chan struct {
		out  string
		code int
	}, 1)
	go func() {
		o, _, c := runBinary(t, ledger, "query", "--id", "P1")
		queryDone <- struct {
			out  string
			code int
		}{o, c}
	}()
	time.Sleep(300 * time.Millisecond) // 确保查询已启动并在等待
	if err := holder.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()

	// 被阻塞的查询在持锁进程终止后应读到完整台账并成功。
	select {
	case r := <-queryDone:
		if r.code != exitOK || !strings.Contains(r.out, "P1") {
			t.Fatalf("被阻塞的查询应在持锁进程终止后读到完整台账并成功: code=%d out=%s", r.code, r.out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("持锁进程终止后被阻塞的查询仍未继续，存在死锁")
	}

	// 查询已结束、锁全部释放：内核自动释放了被杀进程的 flock，
	// 无需人工删除协调文件即可继续作业。
	if release, ok := tryLockNB(t, ledger, lockModeWrite); !ok {
		t.Fatal("持锁进程被强制终止后应无需删除协调文件即可取得锁")
	} else {
		release()
	}

	// 正常作业结束后其他终端继续可用。
	if _, _, c := runBinary(t, ledger, "register", "--id", "P2", "--station", "站点A"); c != exitOK {
		t.Fatal("恢复后新登记应成功")
	}
}

func TestQueriesSeeOneCommittedSnapshotAndNeverRewrite(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "ledger.json")
	if _, _, c := runBinary(t, ledger, "register", "--id", "P1", "--station", "站点A"); c != 0 {
		t.Fatal("登记失败")
	}

	// 查询期间另一终端持续提交；查询读到的必须是某一份完整已提交台账，
	// 且查询本身绝不改写数据文件（不更新时间、不留临时文件）。
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				id := "Q" + strconv.Itoa(i)
				_, _, _ = runBinary(t, ledger, "register", "--id", id, "--station", "站点A")
				i++
			}
		}
	}()
	for k := 0; k < 20; k++ {
		out, _, code := runBinary(t, ledger, "query", "--id", "P1")
		if code != exitOK || !strings.Contains(out, "当前站点: 站点A") {
			t.Fatalf("并发提交期间查询应始终读到一致快照: code=%d out=%s", code, out)
		}
	}
	close(stop)
	wg.Wait()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".parcelhub-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("目录中残留临时文件 %s", e.Name())
		}
	}
	// 查询不改变 P1 的记录；台账仍可被解析为完整 JSON（由后续查询隐式校验）。
	out, _, code := runBinary(t, ledger, "query", "--id", "P1")
	if code != exitOK || strings.Count(out, "操作: 收件") != 1 {
		t.Fatalf("查询不得改写台账: code=%d out=%s", code, out)
	}
}

// seedDeliveringBatch 准备一个含 n 个在站包裹的批次，返回台账路径与包裹编号。
func seedDeliveringBatch(t *testing.T, batch, station, courier string, n int) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	ledger := filepath.Join(dir, "ledger.json")
	ids := make([]string, 0, n)
	args := []string{"dispatch", "--batch", batch, "--station", station, "--courier", courier}
	for i := 0; i < n; i++ {
		id := "P" + strconv.Itoa(i)
		ids = append(ids, id)
		if _, _, c := runBinary(t, ledger, "register", "--id", id, "--station", station); c != 0 {
			t.Fatalf("登记 %s 失败", id)
		}
		args = append(args, "--parcel", id)
	}
	if _, _, c := runBinary(t, ledger, args...); c != 0 {
		t.Fatal("预置批次出站失败")
	}
	return ledger, ids
}

func TestParallelReceiptSameParcelOnlyOneEffective(t *testing.T) {
	ledger, ids := seedDeliveringBatch(t, "B1", "站点A", "张三", 2)

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, codes[i] = runBinary(t, ledger, "receipt",
				"--request", "RC"+strconv.Itoa(i), "--batch", "B1", "--parcel", ids[0], "--result", "签收")
		}(i)
	}
	close(start)
	wg.Wait()

	success := 0
	for _, c := range codes {
		if c == exitOK {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("同一包裹并发首次回执应恰好 1 次成功，得到 %d 次", success)
	}
	// 成功一次后批次已含该件有效回执；其余成员不受影响仍可回执。
	out, _, code := runBinary(t, ledger, "receipt",
		"--request", "RCX", "--batch", "B1", "--parcel", ids[1], "--result", "签收")
	if code != exitOK {
		t.Fatalf("未参与竞争的成员应仍可回执: %s", out)
	}
	out, _, _ = runBinary(t, ledger, "batch", "--id", "B1")
	if !strings.Contains(out, "批次状态: 已完成") {
		t.Fatalf("两件均回执后批次应已完成:\n%s", out)
	}
}

func TestParallelAbortVsRelayAtMostOneTerminalState(t *testing.T) {
	for iter := 0; iter < 6; iter++ {
		ledger, _ := seedDeliveringBatch(t, "B1", "站点A", "张三", 2)

		var wg sync.WaitGroup
		var abortCode, relayCode int
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, abortCode = runBinary(t, ledger, "abort",
				"--request", "A1", "--batch", "B1", "--reason", "车辆故障")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _, relayCode = runBinary(t, ledger, "relay",
				"--request", "T1", "--from", "B1", "--to", "B2", "--courier", "李四", "--reason", "换班")
		}()
		close(start)
		wg.Wait()

		if abortCode == relayCode {
			t.Fatalf("iter %d: 并发中止与续接必须一胜一负，得到 abort=%d relay=%d", iter, abortCode, relayCode)
		}
		outB1, _, c := runBinary(t, ledger, "batch", "--id", "B1")
		if c != 0 {
			t.Fatalf("iter %d: B1 必须始终存在", iter)
		}

		var p0, p1 string
		if abortCode == exitOK {
			// 中止胜出：批次已中止，续接失败不留新批次、不占新号，两件收回在站。
			if !strings.Contains(outB1, "批次状态: 已中止") || strings.Contains(outB1, "已转交") {
				t.Fatalf("iter %d: 中止胜出但 B1 状态不符:\n%s", iter, outB1)
			}
			if _, _, c := runBinary(t, ledger, "batch", "--id", "B2"); c != exitBusiness {
				t.Fatalf("iter %d: 失败续接不得创建新批次 B2", iter)
			}
			for _, id := range []string{"P0", "P1"} {
				p0, _, _ = runBinary(t, ledger, "query", "--id", id)
				if !strings.Contains(p0, "当前状态: 在站") || !strings.Contains(p0, "操作: 收回") || strings.Contains(p0, "操作: 续接") {
					t.Fatalf("iter %d: 中止胜出时 %s 应收回在站且无续接轨迹:\n%s", iter, id, p0)
				}
			}
			// 失败且未占用的续接号在纠正条件后仍可使用：此时 B1 已中止，
			// 换一个仍开放的场景不适用；只验证 T1 没有被占用为冲突结果——
			// 对已中止批次再次续接应是受理失败而非“请求号冲突”。
			_, errText, c := runBinary(t, ledger, "relay",
				"--request", "T1", "--from", "B1", "--to", "B3", "--courier", "李四", "--reason", "换班")
			if c != exitBusiness || strings.Contains(errText, "冲突") {
				t.Fatalf("iter %d: 失败续接不应占用请求号 T1（不应报冲突）: code=%d err=%s", iter, c, errText)
			}
		} else {
			// 续接胜出：B1 已转交，B2 存在且两件在 B2 配送中；中止失败不留记录。
			if !strings.Contains(outB1, "批次状态: 已转交") || strings.Contains(outB1, "已中止") {
				t.Fatalf("iter %d: 续接胜出但 B1 状态不符:\n%s", iter, outB1)
			}
			outB2, _, c := runBinary(t, ledger, "batch", "--id", "B2")
			if c != exitOK || !strings.Contains(outB2, "批次状态: 配送中") {
				t.Fatalf("iter %d: 续接胜出但 B2 不符:\n%s", iter, outB2)
			}
			for _, id := range []string{"P0", "P1"} {
				p1, _, _ = runBinary(t, ledger, "query", "--id", id)
				if !strings.Contains(p1, "当前状态: 配送中") || !strings.Contains(p1, "操作: 续接") || strings.Contains(p1, "操作: 收回") {
					t.Fatalf("iter %d: 续接胜出时 %s 应在新批次配送中:\n%s", iter, id, p1)
				}
			}
			// 失败中止不占号：对已转交批次以 A1 再中止，应是受理失败而非请求号冲突。
			_, errText, c := runBinary(t, ledger, "abort",
				"--request", "A1", "--batch", "B1", "--reason", "车辆故障")
			if c != exitBusiness || strings.Contains(errText, "冲突") {
				t.Fatalf("iter %d: 失败中止不应占用请求号 A1（不应报冲突）: code=%d err=%s", iter, c, errText)
			}
		}
	}
}

func TestParallelReceiptImportsBothFullyKept(t *testing.T) {
	ledger, ids := seedDeliveringBatch(t, "B1", "站点A", "张三", 4)

	// 两份导入文件各回执两件互不重叠的包裹，并发提交后必须全部保留。
	writeImport := func(t *testing.T, path string, rows [][2]string) {
		t.Helper()
		var b strings.Builder
		b.WriteString("[")
		for i, r := range rows {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"request":"` + r[0] + `","batch":"B1","parcel":"` + r[1] + `","result":"签收"}`)
		}
		b.WriteString("]")
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f1 := filepath.Join(filepath.Dir(ledger), "imp1.json")
	f2 := filepath.Join(filepath.Dir(ledger), "imp2.json")
	writeImport(t, f1, [][2]string{{"RC1", ids[0]}, {"RC2", ids[1]}})
	writeImport(t, f2, [][2]string{{"RC3", ids[2]}, {"RC4", ids[3]}})

	var wg sync.WaitGroup
	codes := make([]int, 2)
	outs := make([]string, 2)
	start := make(chan struct{})
	for i, f := range []string{f1, f2} {
		wg.Add(1)
		go func(i int, f string) {
			defer wg.Done()
			<-start
			outs[i], _, codes[i] = runBinary(t, ledger, "receipt-import", "--file", f)
		}(i, f)
	}
	close(start)
	wg.Wait()
	for i, c := range codes {
		if c != exitOK {
			t.Fatalf("互不冲突的并发导入 %d 应整体成功: %s", i, outs[i])
		}
	}
	out, _, code := runBinary(t, ledger, "batch", "--id", "B1")
	if code != exitOK || !strings.Contains(out, "逐件回执（4/4 已回执）") || !strings.Contains(out, "批次状态: 已完成") {
		t.Fatalf("两份并发导入后四件都应有效回执、批次完成:\n%s", out)
	}
}

func TestParallelImportContainingOverlapIsAllOrNothing(t *testing.T) {
	ledger, _ := seedDeliveringBatch(t, "B1", "站点A", "张三", 3)

	// 文件 A 回执 P0、P1；文件 B 也回执 P1（不同请求号）+ P2。
	// 两者并发：后到者因 P1 已在该批次回执而整份拒绝，A 的两件必须完整保留，
	// B 不产生任何部分变更、不占用新请求号。
	dir := filepath.Dir(ledger)
	writeImp := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f1 := writeImp("a.json", `[{"request":"RC1","batch":"B1","parcel":"P0","result":"签收"},{"request":"RC2","batch":"B1","parcel":"P1","result":"签收"}]`)
	f2 := writeImp("b.json", `[{"request":"RC3","batch":"B1","parcel":"P1","result":"签收"},{"request":"RC4","batch":"B1","parcel":"P2","result":"签收"}]`)

	var wg sync.WaitGroup
	c1, c2 := -1, -1
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _, _, c1 = runBinary(t, ledger, "receipt-import", "--file", f1) }()
	go func() { defer wg.Done(); <-start; _, _, c2 = runBinary(t, ledger, "receipt-import", "--file", f2) }()
	close(start)
	wg.Wait()
	if c1 == c2 {
		t.Fatalf("重叠的并发导入必须一成一败，得到 c1=%d c2=%d", c1, c2)
	}
	out, _, _ := runBinary(t, ledger, "batch", "--id", "B1")
	// 成功方两件、失败方零件：总共恰有 2 件有效回执，失败方不产生部分变更、不占号。
	if !strings.Contains(out, "逐件回执（2/3 已回执）") {
		t.Fatalf("失败导入必须整份撤销、不留部分变更，实际进度:\n%s", out)
	}
	// 按实际胜出方核对：胜方的两个请求号都在，败方的两个请求号都不在。
	winner := [2]string{"RC1", "RC2"}
	loser := [2]string{"RC3", "RC4"}
	if c2 == exitOK {
		winner, loser = [2]string{"RC3", "RC4"}, [2]string{"RC1", "RC2"}
	}
	for _, rc := range winner {
		if !strings.Contains(out, "请求号: "+rc) {
			t.Fatalf("胜出导入的请求号 %s 应已生效:\n%s", rc, out)
		}
	}
	for _, rc := range loser {
		if strings.Contains(out, "请求号: "+rc) {
			t.Fatalf("失败导入不得占用请求号 %s:\n%s", rc, out)
		}
	}
}

func TestParallelFreezeThenUnfreezeChainsConsistently(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "ledger.json")
	if _, _, c := runBinary(t, ledger, "register", "--id", "P1", "--station", "站点A"); c != 0 {
		t.Fatal("登记失败")
	}

	// 多个不同异常单并发冻结同一件：至多一张成功。
	const n = 6
	var wg sync.WaitGroup
	codes := make([]int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, codes[i] = runBinary(t, ledger, "freeze",
				"--incident", "E"+strconv.Itoa(i), "--parcel", "P1", "--station", "站点A", "--reason", "异常")
		}(i)
	}
	close(start)
	wg.Wait()
	success := 0
	for _, c := range codes {
		if c == exitOK {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("并发首次冻结应恰好 1 次成功，得到 %d", success)
	}
	out, _, _ := runBinary(t, ledger, "query", "--id", "P1")
	if strings.Count(out, "操作: 冻结") != 1 || !strings.Contains(out, "当前状态: 异常冻结") {
		t.Fatalf("并发冻结后应恰有一张未解除异常单:\n%s", out)
	}
	// 找出实际胜出的异常单号并解除它：冻结-解除链路在并发后仍最终一致。
	var winner string
	for i := 0; i < n; i++ {
		no := "E" + strconv.Itoa(i)
		if strings.Contains(out, "异常单号: "+no) {
			winner = no
			break
		}
	}
	if winner == "" {
		t.Fatalf("未能从查询中辨认胜出异常单:\n%s", out)
	}
	if _, _, c := runBinary(t, ledger, "unfreeze",
		"--request", "U1", "--incident", winner, "--note", "核实放行"); c != exitOK {
		t.Fatalf("胜出异常单 %s 应可解除", winner)
	}
	out, _, _ = runBinary(t, ledger, "query", "--id", "P1")
	if !strings.Contains(out, "当前状态: 在站") || !strings.Contains(out, "操作: 解除冻结") {
		t.Fatalf("解除后应恢复在站并保留解除轨迹:\n%s", out)
	}
}
