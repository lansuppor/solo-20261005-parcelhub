package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ledgerLock 通过台账数据文件旁的协调文件（flock）让同一台机器上的多个终端
// （进程）对同一份台账串行作业。协调文件与 --data 路径一一对应：
//
//   - 路径先按本地文件系统规则取绝对路径并求值 . / .. / 符号链接，
//     因此相对、绝对及含 .、.. 的写法只要指向同一文件，就使用同一把锁；
//   - 协调文件命名为 ".<台账文件名>.parcelhub-lock"，位于台账所在目录；
//   - 协调文件不是台账的一部分，内容仅用于崩溃排查，可随时删除重建；
//     进程被强制终止后内核自动释放 flock，其他终端无需手工删除即可继续。
type ledgerLock struct {
	path string // 协调文件路径（与台账规范路径对应）
	f    *os.File
}

// 协调锁模式：只读命令用共享锁（彼此并行），修改命令用排他锁（串行整次事务）。
const (
	lockModeRead  = "r"
	lockModeWrite = "rw"
)

// canonicalLedgerPath 把用户给出的台账路径规范化为同一身份：
// 转绝对路径并对每个路径成分求值（含 .、.. 与符号链接）。
// 文件本身尚不存在时对其父目录求值，文件名部分按字面保留
// （首次创建台账与协调文件遵守同样的协调规则）。
func canonicalLedgerPath(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("定位数据文件失败: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("定位数据文件失败: %w", err)
	}
	// 文件尚不存在：尽量求值已存在的父目录，使台账的不同写法仍归一。
	dir := filepath.Dir(abs)
	if resolvedDir, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(resolvedDir, filepath.Base(abs)), nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("定位数据文件失败: %w", err)
	}
	// 父目录也尚不存在（保存时会创建）：至少对绝对路径做词法归一。
	return abs, nil
}

// lockPathFor 返回台账规范路径对应的协调文件路径。
func lockPathFor(canonical string) string {
	return filepath.Join(filepath.Dir(canonical), "."+filepath.Base(canonical)+".parcelhub-lock")
}

// lockInfo 写入协调文件的持有者信息，仅供排查（flock 才是协调依据）。
type lockInfo struct {
	PID   int    `json:"pid"`
	Since string `json:"since"`
	Path  string `json:"path"`
	Mode  string `json:"mode"`
}

// acquireLedgerLock 打开（必要时创建）协调文件并以阻塞方式取得共享(LOCK_SH)
// 或排他(LOCK_EX)协调锁。争用时等待；持有者进程结束（含被强制终止）后锁由
// 内核自动释放，本调用随后取得锁，无需人工删除协调文件。
func acquireLedgerLock(dataPath, mode string) (*ledgerLock, error) {
	canonical, err := canonicalLedgerPath(dataPath)
	if err != nil {
		return nil, err
	}
	lockPath := lockPathFor(canonical)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("准备台账协调文件失败: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开台账协调文件失败: %w", err)
	}
	acquired := false
	defer func() {
		if !acquired {
			_ = f.Close()
		}
	}()

	how := syscall.LOCK_SH
	if mode == lockModeWrite {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		return nil, fmt.Errorf("等待台账协调锁失败: %w", err)
	}

	// 记录持有者信息（仅排他持有者写入，避免多个共享读者互相截断；
	// 该信息不影响协调，写入失败也被忽略——协调只依赖 flock）。
	if mode == lockModeWrite {
		info := lockInfo{
			PID:   os.Getpid(),
			Since: time.Now().Format(time.RFC3339),
			Path:  canonical,
			Mode:  mode,
		}
		if buf, err := json.Marshal(info); err == nil {
			_ = f.Truncate(0)
			_, _ = f.WriteAt(append(buf, '\n'), 0)
		}
	}
	acquired = true
	return &ledgerLock{path: lockPath, f: f}, nil
}

// release 释放协调锁并关闭协调文件；协调文件保留在磁盘上供后续终端复用。
func (l *ledgerLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
