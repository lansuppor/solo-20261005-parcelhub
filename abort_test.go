package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupAbortBatch 登记三件包裹并从站点A出站批次 B1（配送员张三）。
func setupAbortBatch(t *testing.T, s *Store, t0 time.Time) {
	t.Helper()
	for _, id := range []string{"P001", "P002", "P003"} {
		mustRegister(t, s, id, "站点A", t0)
	}
	if _, _, err := s.Dispatch("B1", "站点A", "张三", []string{"P001", "P002", "P003"}, t0.Add(time.Hour)); err != nil {
		t.Fatalf("Dispatch 意外失败: %v", err)
	}
}

func TestAbortSuccessRecallsUnreceiptedOnly(t *testing.T) {
	s, _ := openTempStore(t)
	t0 := tClock(2026, 10, 5, 9, 0)
	setupAbortBatch(t, s, t0)

	// P001 签收、P002 失败回执（回到出发站在站），P003 未回执。
	t1 := t0.Add(2 * time.Hour)
	if _, _, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", t1); err != nil {
		t.Fatalf("签收回执意外失败: %v", err)
	}
	if _, _, err := s.Receipt("RC2", "B1", "P002", resultFailed, "收件人不在", t1.Add(time.Minute)); err != nil {
		t.Fatalf("失败回执意外失败: %v", err)
	}

	t2 := t0.Add(3 * time.Hour)
	res, replayed, err := s.Abort("AB1", "B1", "配送员车辆故障", t2)
	if err != nil || replayed {
		t.Fatalf("首次中止应成功且非重放: %v replayed=%v", err, replayed)
	}
	if res.Request != "AB1" || res.Batch != "B1" || res.Station != "站点A" || res.Reason != "配送员车辆故障" ||
		len(res.Parcels) != 1 || res.Parcels[0] != "P003" || !res.Time.Equal(t2) {
		t.Fatalf("中止结果不符: %+v", res)
	}

	// 收回件恢复在站，轨迹追加收回记录。
	p3, _ := s.Query("P003")
	if p3.Status != statusInStation || p3.Station != "站点A" {
		t.Fatalf("P003 收回后应在站点A在站: %+v", p3)
	}
	last := p3.Trail[len(p3.Trail)-1]
	if last.Op != "收回" || last.Batch != "B1" || last.Request != "AB1" ||
		last.Reason != "配送员车辆故障" || last.Station != "站点A" || !last.Time.Equal(t2) {
		t.Fatalf("P003 收回轨迹不符: %+v", last)
	}

	// 已回执成员完全保留：状态、轨迹不变。
	p1, _ := s.Query("P001")
	if p1.Status != statusSigned || p1.Trail[len(p1.Trail)-1].Op != "回执" {
		t.Fatalf("P001 已签收，不应被中止改变: %+v", p1)
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusInStation || p2.Trail[len(p2.Trail)-1].Op != "回执" {
		t.Fatalf("P002 已失败回执，不应被中止改变: %+v", p2)
	}

	// 批次永久关闭：保留原成员顺序、配送员和出站时间。
	b, err := s.BatchQuery("B1")
	if err != nil {
		t.Fatalf("BatchQuery 失败: %v", err)
	}
	if b.AbortedBy != "AB1" || b.Courier != "张三" || len(b.Parcels) != 3 ||
		b.Parcels[0] != "P001" || b.Parcels[2] != "P003" || len(b.Receipts) != 2 {
		t.Fatalf("批次中止后记录不符: %+v", b)
	}
}

func TestAbortReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	t0 := tClock(2026, 10, 5, 9, 0)
	setupAbortBatch(t, s, t0)
	t1 := t0.Add(time.Hour)
	if _, _, err := s.Abort("AB1", "B1", "天气原因", t1); err != nil {
		t.Fatalf("首次中止失败: %v", err)
	}

	// 同号、同批次、同原因重放：返回首次集合与时间，不追加轨迹。
	res, replayed, err := s.Abort("AB1", "B1", "天气原因", t1.Add(5*time.Hour))
	if err != nil || !replayed {
		t.Fatalf("同内容重放应成功: %v replayed=%v", err, replayed)
	}
	if !res.Time.Equal(t1) || len(res.Parcels) != 3 {
		t.Fatalf("重放应返回首次集合与时间: %+v", res)
	}
	p, _ := s.Query("P001")
	if len(p.Trail) != 3 { // 收件、出站、收回
		t.Fatalf("重放不得追加轨迹，得到 %d 条", len(p.Trail))
	}

	// 换内容报冲突。
	if _, _, err := s.Abort("AB1", "B1", "别的原因", t1); err == nil || !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("换原因应报冲突，得到 %v", err)
	}

	// 换请求号再中止同一批次：拒绝。
	if _, _, err := s.Abort("AB2", "B1", "天气原因", t1); err == nil || !strings.Contains(err.Error(), "已中止") {
		t.Fatalf("已中止批次不能再次中止，得到 %v", err)
	}
}

func TestAbortRejectsInvalidBatches(t *testing.T) {
	s, _ := openTempStore(t)
	t0 := tClock(2026, 10, 5, 9, 0)
	setupAbortBatch(t, s, t0)
	now := t0.Add(2 * time.Hour)

	// 批次不存在。
	if _, _, err := s.Abort("AB1", "NOPE", "原因", now); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("不存在的批次应拒绝，得到 %v", err)
	}
	// 失败的首次中止不占号：AB1 之后可正常使用。
	if _, _, err := s.Abort("AB1", "B1", "原因", now); err != nil {
		t.Fatalf("失败的中止不应占号: %v", err)
	}

	// 正常完成的批次不能中止。
	setupAbortBatch2(t, s, t0)
	if _, _, err := s.Abort("AB2", "B2", "原因", now); err == nil || !strings.Contains(err.Error(), "已正常完成") {
		t.Fatalf("已完成批次应拒绝中止，得到 %v", err)
	}
}

// setupAbortBatch2 建立两件包裹的批次 B2 并全部回执签收（正常完成）。
func setupAbortBatch2(t *testing.T, s *Store, t0 time.Time) {
	t.Helper()
	for _, id := range []string{"P101", "P102"} {
		mustRegister(t, s, id, "站点A", t0)
	}
	if _, _, err := s.Dispatch("B2", "站点A", "李四", []string{"P101", "P102"}, t0.Add(time.Hour)); err != nil {
		t.Fatalf("Dispatch B2 意外失败: %v", err)
	}
	for i, id := range []string{"P101", "P102"} {
		if _, _, err := s.Receipt("RCB2-"+id, "B2", id, resultSigned, "", t0.Add(time.Duration(2+i)*time.Hour)); err != nil {
			t.Fatalf("B2 回执意外失败: %v", err)
		}
	}
}

func TestAbortReceiptRulesAfterRecall(t *testing.T) {
	s, _ := openTempStore(t)
	t0 := tClock(2026, 10, 5, 9, 0)
	setupAbortBatch(t, s, t0)
	t1 := t0.Add(time.Hour)
	if _, _, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", t1); err != nil {
		t.Fatalf("回执意外失败: %v", err)
	}
	if _, _, err := s.Abort("AB1", "B1", "车辆故障", t1.Add(time.Hour)); err != nil {
		t.Fatalf("中止意外失败: %v", err)
	}

	// 收回件不能首次回执旧批次。
	if _, _, err := s.Receipt("RC9", "B1", "P002", resultSigned, "", t1.Add(2*time.Hour)); err == nil {
		t.Fatal("收回件首次回执旧批次必须拒绝")
	}
	// 失败的首次回执不占号：RC9 之后可用于新批次。
	// 收回件可再次出站。
	if _, _, err := s.Dispatch("B9", "站点A", "王五", []string{"P002", "P003"}, t1.Add(3*time.Hour)); err != nil {
		t.Fatalf("收回件再次出站应成功: %v", err)
	}
	if _, _, err := s.Receipt("RC9", "B9", "P002", resultSigned, "", t1.Add(4*time.Hour)); err != nil {
		t.Fatalf("RC9 用于新批次回执应成功: %v", err)
	}

	// 已有回执的同内容重放仍返回历史结果。
	res, replayed, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", t1.Add(5*time.Hour))
	if err != nil || !replayed || !res.Time.Equal(t1) {
		t.Fatalf("已有回执重放应返回历史结果: %v replayed=%v res=%+v", err, replayed, res)
	}
	// 原 dispatch 的同内容重放仍返回历史结果，不重开批次。
	bres, replayed, err := s.Dispatch("B1", "站点A", "张三", []string{"P001", "P002", "P003"}, t1.Add(6*time.Hour))
	if err != nil || !replayed || bres.AbortedBy != "AB1" {
		t.Fatalf("原 dispatch 重放应返回历史结果: %v replayed=%v abortedBy=%q", err, replayed, bres.AbortedBy)
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusSigned { // 在 B9 中已签收，重放 B1 不得改变
		t.Fatalf("dispatch 重放不得改变包裹状态: %+v", p2)
	}
}

func TestAbortImportWithLateReceiptRejected(t *testing.T) {
	s, _ := openTempStore(t)
	t0 := tClock(2026, 10, 5, 9, 0)
	setupAbortBatch(t, s, t0)
	t1 := t0.Add(time.Hour)
	if _, _, err := s.Abort("AB1", "B1", "车辆故障", t1); err != nil {
		t.Fatalf("中止意外失败: %v", err)
	}

	// 导入含迟到回执（针对已收回件）：整份拒绝，新号不占用。
	records := []ReceiptImportRecord{
		{Request: "IMP1", Batch: "B1", Parcel: "P002", Result: resultSigned},
		{Request: "IMP2", Batch: "B1", Parcel: "P003", Result: resultSigned},
	}
	if _, err := s.ImportReceipts(records, t1.Add(time.Hour)); err == nil {
		t.Fatal("含迟到回执的导入必须整份拒绝")
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusInStation || len(p2.Trail) != 3 {
		t.Fatalf("导入拒绝后包裹状态与轨迹不应改变: %+v", p2)
	}
	// 新号未占用：IMP1 可用于合法回执（P002 再次出站后）。
	if _, _, err := s.Dispatch("B9", "站点A", "王五", []string{"P002"}, t1.Add(2*time.Hour)); err != nil {
		t.Fatalf("再次出站失败: %v", err)
	}
	if _, _, err := s.Receipt("IMP1", "B9", "P002", resultSigned, "", t1.Add(3*time.Hour)); err != nil {
		t.Fatalf("导入失败后 IMP1 不应被占用: %v", err)
	}
}

func TestAbortCountsAsNewFlowForReturn(t *testing.T) {
	s, _ := openTempStore(t)
	t0 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t0)
	if _, _, err := s.Handoff("H1", "站点A", "站点B", []string{"P001"}, t0.Add(time.Hour)); err != nil {
		t.Fatalf("交接意外失败: %v", err)
	}
	if _, _, err := s.Dispatch("B1", "站点B", "张三", []string{"P001"}, t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("出站意外失败: %v", err)
	}
	if _, _, err := s.Abort("AB1", "B1", "车辆故障", t0.Add(3*time.Hour)); err != nil {
		t.Fatalf("中止意外失败: %v", err)
	}
	// 收回算新流转：不能因此退回出站前的旧交接 H1。
	if _, _, err := s.Return("RT1", "H1", "错发站点", t0.Add(4*time.Hour)); err == nil {
		t.Fatal("收回后不能退回出站前的旧交接")
	}
}

func TestAbortRecalledParcelHandoffAndFreeze(t *testing.T) {
	s, _ := openTempStore(t)
	t0 := tClock(2026, 10, 5, 9, 0)
	setupAbortBatch(t, s, t0)
	if _, _, err := s.Abort("AB1", "B1", "车辆故障", t0.Add(time.Hour)); err != nil {
		t.Fatalf("中止意外失败: %v", err)
	}
	// 收回件可交接。
	if _, _, err := s.Handoff("H1", "站点A", "站点B", []string{"P001"}, t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("收回件交接应成功: %v", err)
	}
	// 收回件可冻结。
	if _, _, err := s.Freeze("E1", "P002", "站点A", "外包装破损", t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("收回件冻结应成功: %v", err)
	}
}

func TestAbortPersistsAcrossRestart(t *testing.T) {
	s, path := openTempStore(t)
	t0 := tClock(2026, 10, 5, 9, 0)
	setupAbortBatch(t, s, t0)
	t1 := t0.Add(time.Hour)
	if _, _, err := s.Abort("AB1", "B1", "车辆故障", t1); err != nil {
		t.Fatalf("中止意外失败: %v", err)
	}

	// 重启后规则不变：重放返回首次结果，再中止仍拒绝。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重启打开失败: %v", err)
	}
	res, replayed, err := s2.Abort("AB1", "B1", "车辆故障", t1.Add(9*time.Hour))
	if err != nil || !replayed || !res.Time.Equal(t1) || len(res.Parcels) != 3 {
		t.Fatalf("重启后重放不符: %v replayed=%v res=%+v", err, replayed, res)
	}
	if _, _, err := s2.Abort("AB2", "B1", "车辆故障", t1); err == nil {
		t.Fatal("重启后已中止批次仍应拒绝再次中止")
	}
	if _, _, err := s2.Receipt("RC9", "B1", "P001", resultSigned, "", t1); err == nil {
		t.Fatal("重启后收回件仍不能首次回执旧批次")
	}
}

func TestAbortCorruptLedgerRejected(t *testing.T) {
	s, path := openTempStore(t)
	t0 := tClock(2026, 10, 5, 9, 0)
	setupAbortBatch(t, s, t0)
	if _, _, err := s.Abort("AB1", "B1", "车辆故障", t0.Add(time.Hour)); err != nil {
		t.Fatalf("中止意外失败: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 篡改：删除中止记录，收回轨迹失去对应 —— 必须明确拒绝读写。
	tampered := strings.Replace(string(raw), `"AB1"`, `"ABX"`, 1)
	if tampered == string(raw) {
		t.Fatal("篡改未生效")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("中止记录与收回轨迹不一致应报损坏，得到 %v", err)
	}

	// 篡改：清空批次的中止标记 —— 中止记录无法对应，同样拒绝。
	tampered2 := strings.Replace(string(raw), `"abortedBy": "AB1"`, `"abortedBy": ""`, 1)
	if tampered2 == string(raw) {
		t.Fatal("篡改2未生效")
	}
	bad2 := filepath.Join(dir, "bad2.json")
	if err := os.WriteFile(bad2, []byte(tampered2), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(bad2); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("批次缺少中止标记应报损坏，得到 %v", err)
	}

	// 原有效台账不受影响，仍可直接使用。
	if _, err := Open(path); err != nil {
		t.Fatalf("原台账应保持有效: %v", err)
	}
}

func TestCLIAbortEndToEnd(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"

	for _, id := range []string{"P001", "P002"} {
		if _, _, code := runCLI(t, dbPath, "register", "--id", id, "--station", "站点A"); code != 0 {
			t.Fatalf("登记 %s 失败", id)
		}
	}
	if _, _, code := runCLI(t, dbPath, "dispatch", "--batch", "B1", "--station", "站点A", "--courier", "张三",
		"--parcel", "P001", "--parcel", "P002"); code != 0 {
		t.Fatal("出站失败")
	}
	if _, _, code := runCLI(t, dbPath, "receipt", "--request", "RC1", "--batch", "B1", "--parcel", "P001", "--result", "签收"); code != 0 {
		t.Fatal("回执失败")
	}

	// 参数清理后为空：退出码 1。
	if _, errText, code := runCLI(t, dbPath, "abort", "--request", "AB1", "--batch", "B1", "--reason", "   "); code != exitBusiness {
		t.Fatalf("空原因应以 1 退出: code=%d err=%s", code, errText)
	}

	out, _, code := runCLI(t, dbPath, "abort", "--request", " AB1 ", "--batch", " B1 ", "--reason", " 车辆故障 ")
	if code != 0 || !strings.Contains(out, "中止成功") || !strings.Contains(out, "P002") ||
		!strings.Contains(out, "车辆故障") || strings.Contains(out, "P001\n") {
		t.Fatalf("中止输出不符: code=%d out=%s", code, out)
	}
	if strings.Contains(out, "  - P001\n") {
		t.Fatalf("已回执成员不应列入收回集合: %s", out)
	}

	// 同内容重放。
	out, _, code = runCLI(t, dbPath, "abort", "--request", "AB1", "--batch", "B1", "--reason", "车辆故障")
	if code != 0 || !strings.Contains(out, "重复提交") {
		t.Fatalf("重放提示不符: code=%d out=%s", code, out)
	}

	// batch 展示：已中止，收回件不列为未回执。
	out, _, code = runCLI(t, dbPath, "batch", "--id", "B1")
	if code != 0 || !strings.Contains(out, "批次状态: 已中止") || !strings.Contains(out, "中止请求号: AB1") ||
		!strings.Contains(out, "中止原因: 车辆故障") {
		t.Fatalf("batch 中止信息不符: code=%d out=%s", code, out)
	}
	if !strings.Contains(out, "P002    已收回") || strings.Contains(out, "P002    未回执") {
		t.Fatalf("收回件不应列为未回执: %s", out)
	}
	if !strings.Contains(out, "P001    已回执    结果: 签收") {
		t.Fatalf("已回执成员应保留真实回执: %s", out)
	}

	// query 展示收回轨迹。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P002")
	if code != 0 || !strings.Contains(out, "操作: 收回") || !strings.Contains(out, "中止请求号: AB1") ||
		!strings.Contains(out, "当前状态: 在站") {
		t.Fatalf("query 收回信息不符: code=%d out=%s", code, out)
	}

	// 收回件不能首次回执旧批次：退出码 1。
	if _, errText, code := runCLI(t, dbPath, "receipt", "--request", "RC9", "--batch", "B1", "--parcel", "P002", "--result", "签收"); code != exitBusiness {
		t.Fatalf("收回件回执旧批次应以 1 退出: code=%d err=%s", code, errText)
	}
}
