package main

import (
	"strings"
	"testing"
)

// 同一签收的退件与撤销并发竞争：至多一方成功，另一方以状态码 1 退出，台账保持自洽。
func TestConcurrentReceiptReturnVsRevoke(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	if _, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A"); code != 0 {
		t.Fatal("登记失败")
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B1",
		"--station", "站点A", "--courier", "张三", "--parcel", "P001"); code != 0 {
		t.Fatal("出站失败")
	}
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC1",
		"--batch", "B1", "--parcel", "P001", "--result", "签收"); code != 0 {
		t.Fatal("签收失败")
	}

	_, errs, codes := runParallel(t, 2, func(i int) []string {
		if i == 0 {
			return []string{"--data", dbPath, "receipt-return", "--request", "PR1",
				"--receipt", "RC1", "--reason", "收件人拒收"}
		}
		return []string{"--data", dbPath, "receipt-revoke", "--request", "RV1",
			"--receipt", "RC1", "--reason", "误录签收"}
	})
	if got := countCodes(codes, exitOK); got != 1 {
		t.Fatalf("退件与撤销竞争应恰好一方成功，成功 %d 次（codes=%v errs=%v）", got, codes, errs)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	p, _ := s.Query("P001")
	switch codes[0] {
	case exitOK:
		// 退件成功：包裹在站，末条为退件；撤销必须失败。
		if p.Status != statusInStation || p.Station != "站点A" {
			t.Fatalf("退件竞争成功后应在出发站在站: %+v", p)
		}
		if p.Trail[len(p.Trail)-1].Op != "退件" {
			t.Fatalf("末条轨迹应为退件: %+v", p.Trail[len(p.Trail)-1])
		}
		if rc := s.ReceiptOf("RC1"); rc.ReturnedBy != "PR1" || rc.RevokedBy != "" {
			t.Fatalf("退件方成功的关联不符: %+v", rc)
		}
		if _, _, code := runCLI(t, dbPath, "receipt-revoke", "--request", "RV2",
			"--receipt", "RC1", "--reason", "再撤销"); code != exitBusiness {
			t.Fatalf("退件后撤销应失败(1)，得到 %d", code)
		}
	default:
		// 撤销成功：包裹恢复配送中，末条为撤销回执；退件必须失败。
		if p.Status != statusDelivering || currentBatch(p) != "B1" {
			t.Fatalf("撤销竞争成功后应恢复配送中: %+v", p)
		}
		if p.Trail[len(p.Trail)-1].Op != "撤销回执" {
			t.Fatalf("末条轨迹应为撤销回执: %+v", p.Trail[len(p.Trail)-1])
		}
		if rc := s.ReceiptOf("RC1"); rc.RevokedBy != "RV1" || rc.ReturnedBy != "" {
			t.Fatalf("撤销方成功的关联不符: %+v", rc)
		}
		if _, _, code := runCLI(t, dbPath, "receipt-return", "--request", "PR2",
			"--receipt", "RC1", "--reason", "再退"); code != exitBusiness {
			t.Fatalf("撤销后退件应失败(1)，得到 %d", code)
		}
	}
}

// 同号同内容的并发退件：只生效一次，其他提交返回首次结果；轨迹中只有一条退件记录。
func TestConcurrentReceiptReturnSameRequest(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	if _, _, code := runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A"); code != 0 {
		t.Fatal("登记失败")
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B1",
		"--station", "站点A", "--courier", "张三", "--parcel", "P001"); code != 0 {
		t.Fatal("出站失败")
	}
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC1",
		"--batch", "B1", "--parcel", "P001", "--result", "签收"); code != 0 {
		t.Fatal("签收失败")
	}
	const n = 4
	outs, _, codes := runParallel(t, n, func(i int) []string {
		return []string{"--data", dbPath, "receipt-return", "--request", "PR1",
			"--receipt", "RC1", "--reason", "拒收"}
	})
	if got := countCodes(codes, exitOK); got != n {
		t.Fatalf("同号同内容的并发退件应全部返回成功，成功 %d 次", got)
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
	returns := 0
	for _, e := range p.Trail {
		if e.Op == "退件" {
			returns++
		}
	}
	if returns != 1 {
		t.Fatalf("同号同内容只应生效一次（一条退件轨迹），实际 %d 条", returns)
	}
}
