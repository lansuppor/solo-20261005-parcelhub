package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustFreeze(t *testing.T, s *Store, exception, parcel, station, reason string, now time.Time) {
	t.Helper()
	if _, _, err := s.Freeze(exception, parcel, station, reason, now); err != nil {
		t.Fatalf("Freeze(%q) 意外失败: %v", exception, err)
	}
}

func mustUnfreeze(t *testing.T, s *Store, request, exception, note string, now time.Time) {
	t.Helper()
	if _, _, err := s.Unfreeze(request, exception, note, now); err != nil {
		t.Fatalf("Unfreeze(%q) 意外失败: %v", request, err)
	}
}

func TestFreezeSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)

	t2 := tClock(2026, 10, 5, 10, 0)
	res, replayed, err := s.Freeze("E1", "P001", "站点A", "疑似破损", t2)
	if err != nil || replayed {
		t.Fatalf("冻结应成功且非重放: %v replayed=%v", err, replayed)
	}
	if res.Exception != "E1" || res.Parcel != "P001" || res.Station != "站点A" ||
		res.Reason != "疑似破损" || !res.Time.Equal(t2) || res.ReleasedBy != "" {
		t.Fatalf("冻结结果不符: %+v", res)
	}

	p, _ := s.Query("P001")
	if p.Station != "站点A" || p.Status != statusFrozen || p.FrozenBy != "E1" {
		t.Fatalf("冻结后站点/状态/异常单不符: %+v", p)
	}
	if len(p.Trail) != 2 {
		t.Fatalf("应有两条轨迹，得到 %d", len(p.Trail))
	}
	e := p.Trail[1]
	if e.Op != "冻结" || e.Station != "站点A" || e.Exception != "E1" ||
		e.Reason != "疑似破损" || !e.Time.Equal(t2) {
		t.Fatalf("冻结记录不符: %+v", e)
	}
}

func TestFreezeFailures(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	mustRegister(t, s, "P004", "站点A", t1)
	// P002 配送中；P003 已签收；P004 已冻结。
	if _, _, err := s.Dispatch("B1", "站点A", "张三", []string{"P002", "P003"}, tClock(2026, 10, 5, 10, 0)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Receipt("RC1", "B1", "P003", resultSigned, "", tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatal(err)
	}
	mustFreeze(t, s, "E0", "P004", "站点A", "先冻结", tClock(2026, 10, 5, 11, 30))

	cases := []struct {
		name                            string
		exception, parcel, station, why string
		errSub                          string
	}{
		{"未登记", "E1", "GHOST", "站点A", "x", "未登记"},
		{"站点不符", "E1", "P001", "站点B", "x", "不在指定站点"},
		{"配送中不能冻结", "E1", "P002", "站点A", "x", "配送中"},
		{"已签收不能冻结", "E1", "P003", "站点A", "x", "已签收"},
		{"重复冻结", "E1", "P004", "站点A", "x", "只能有一张未解除异常单"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Freeze(c.exception, c.parcel, c.station, c.why, tClock(2026, 10, 5, 12, 0)); err == nil ||
				!strings.Contains(err.Error(), c.errSub) {
				t.Fatalf("应失败且提示 %q，得到 %v", c.errSub, err)
			}
		})
	}
	// 失败的冻结不占号：E1 未被任何失败占用，可正常使用。
	mustFreeze(t, s, "E1", "P001", "站点A", "复核", tClock(2026, 10, 5, 12, 30))
}

func TestFreezeReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	t1 := tClock(2026, 10, 5, 10, 0)
	first, replayed, err := s.Freeze("E1", "P001", "站点A", "疑似破损", t1)
	if err != nil || replayed {
		t.Fatalf("首次冻结失败: %v replayed=%v", err, replayed)
	}

	// 同号同内容：重放首次结果与时间，不再次冻结。
	t2 := tClock(2026, 10, 5, 11, 0)
	got, replayed, err := s.Freeze("E1", "P001", "站点A", "疑似破损", t2)
	if err != nil || !replayed || !got.Time.Equal(t1) || got != first {
		t.Fatalf("同内容重放应返回首次结果: %v replayed=%v got=%+v", err, replayed, got)
	}
	p, _ := s.Query("P001")
	if len(p.Trail) != 2 {
		t.Fatalf("重放不得追加轨迹，得到 %d 条", len(p.Trail))
	}

	// 同号换内容：冲突。
	for _, c := range [][4]string{
		{"E1", "P001", "站点A", "别的原因"},
		{"E1", "P001", "站点B", "疑似破损"},
	} {
		if _, _, err := s.Freeze(c[0], c[1], c[2], c[3], t2); err == nil || !strings.Contains(err.Error(), "冲突") {
			t.Fatalf("同号换内容应报冲突，得到 %v", err)
		}
	}

	// 解除后异常单号也不能复用：同内容仍重放首次结果，换内容仍冲突。
	mustUnfreeze(t, s, "U1", "E1", "复核无异常", tClock(2026, 10, 5, 12, 0))
	got, replayed, err = s.Freeze("E1", "P001", "站点A", "疑似破损", tClock(2026, 10, 5, 13, 0))
	if err != nil || !replayed || !got.Time.Equal(t1) {
		t.Fatalf("解除后同号同内容应仍重放首次结果: %v replayed=%v", err, replayed)
	}
	if p, _ := s.Query("P001"); p.Status != statusInStation {
		t.Fatalf("重放不得重新冻结: %+v", p)
	}
	if _, _, err := s.Freeze("E1", "P001", "站点A", "新原因", tClock(2026, 10, 5, 13, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("解除后同号换内容应报冲突，得到 %v", err)
	}

	// 可用新异常单再次冻结。
	mustFreeze(t, s, "E2", "P001", "站点A", "再次冻结", tClock(2026, 10, 5, 14, 0))
	if p, _ := s.Query("P001"); p.Status != statusFrozen || p.FrozenBy != "E2" {
		t.Fatalf("新异常单再次冻结不符: %+v", p)
	}
}

func TestFreezeBlocksHandoffDispatchReturn(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P003"}, tClock(2026, 10, 5, 9, 30))
	mustFreeze(t, s, "E1", "P001", "站点A", "疑似破损", tClock(2026, 10, 5, 10, 0))
	mustFreeze(t, s, "E2", "P003", "站点B", "疑似破损", tClock(2026, 10, 5, 10, 0))

	// 含冻结包裹的首次交接：整批拒绝，其余成员不变。
	if _, _, err := s.Handoff("R2", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 11, 0)); err == nil ||
		!strings.Contains(err.Error(), "整次交接未执行") {
		t.Fatalf("含冻结包裹的交接应整批拒绝，得到 %v", err)
	}
	p2, _ := s.Query("P002")
	if p2.Station != "站点A" || p2.Status != statusInStation || len(p2.Trail) != 1 {
		t.Fatalf("整批拒绝不得改动其他成员: %+v", p2)
	}
	if _, ok := s.data.Handoffs["R2"]; ok {
		t.Fatal("整批拒绝不得占用交接请求号")
	}

	// 含冻结包裹的首次出站：整批拒绝。
	if _, _, err := s.Dispatch("B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 11, 0)); err == nil ||
		!strings.Contains(err.Error(), "整批出站未执行") {
		t.Fatalf("含冻结包裹的出站应整批拒绝，得到 %v", err)
	}
	if _, ok := s.data.Batches["B1"]; ok {
		t.Fatal("整批拒绝不得占用批次号")
	}

	// 含冻结包裹的整批退回：整批拒绝。
	if _, _, err := s.Return("RT1", "R1", "错发", tClock(2026, 10, 5, 11, 0)); err == nil {
		t.Fatal("冻结包裹的整批退回应拒绝")
	}
	if _, ok := s.data.Returns["RT1"]; ok {
		t.Fatal("整批拒绝不得占用退回请求号")
	}

	// 冻结期间，已有交接的同内容重放仍返回历史结果，不移动包裹、不解除冻结。
	got, replayed, err := s.Handoff("R1", "站点A", "站点B", []string{"P003"}, tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || got.To != "站点B" {
		t.Fatalf("冻结期间历史交接重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	p3, _ := s.Query("P003")
	if p3.Station != "站点B" || p3.Status != statusFrozen || len(p3.Trail) != 3 {
		t.Fatalf("重放不得移动包裹或解除冻结: %+v", p3)
	}
}

func TestUnfreezeSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustFreeze(t, s, "E1", "P001", "站点A", "疑似破损", tClock(2026, 10, 5, 10, 0))

	t2 := tClock(2026, 10, 5, 11, 0)
	res, replayed, err := s.Unfreeze("U1", "E1", "复核无异常", t2)
	if err != nil || replayed {
		t.Fatalf("解除应成功且非重放: %v replayed=%v", err, replayed)
	}
	if res.Request != "U1" || res.Exception != "E1" || res.Note != "复核无异常" || !res.Time.Equal(t2) {
		t.Fatalf("解除结果不符: %+v", res)
	}

	p, _ := s.Query("P001")
	if p.Station != "站点A" || p.Status != statusInStation || p.FrozenBy != "" {
		t.Fatalf("解除后应在原站恢复在站: %+v", p)
	}
	if len(p.Trail) != 3 {
		t.Fatalf("应有三条轨迹，得到 %d", len(p.Trail))
	}
	e := p.Trail[2]
	if e.Op != "解除" || e.Station != "站点A" || e.Exception != "E1" ||
		e.Request != "U1" || e.Note != "复核无异常" || !e.Time.Equal(t2) {
		t.Fatalf("解除记录不符: %+v", e)
	}

	// 原冻结记录不删改，仅永久标记为已解除。
	f, ok := s.FreezeQuery("E1")
	if !ok || f.Reason != "疑似破损" || f.ReleasedBy != "U1" {
		t.Fatalf("原冻结记录应保留并标记已解除: %+v", f)
	}
}

func TestUnfreezeFailures(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustFreeze(t, s, "E1", "P001", "站点A", "疑似破损", tClock(2026, 10, 5, 10, 0))

	if _, _, err := s.Unfreeze("U1", "NOPE", "x", tClock(2026, 10, 5, 11, 0)); err == nil ||
		!strings.Contains(err.Error(), "不存在") {
		t.Fatalf("解除不存在的异常单应失败，得到 %v", err)
	}
	mustUnfreeze(t, s, "U1", "E1", "复核无异常", tClock(2026, 10, 5, 11, 0))
	if _, _, err := s.Unfreeze("U2", "E1", "再解除", tClock(2026, 10, 5, 12, 0)); err == nil ||
		!strings.Contains(err.Error(), "已解除") {
		t.Fatalf("重复解除应失败，得到 %v", err)
	}
	// 失败的解除不占号：U2 未被占用，可用于解除新异常单。
	mustFreeze(t, s, "E2", "P001", "站点A", "再次冻结", tClock(2026, 10, 5, 13, 0))
	mustUnfreeze(t, s, "U2", "E2", "处理完毕", tClock(2026, 10, 5, 14, 0))
}

func TestUnfreezeReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	t1 := tClock(2026, 10, 5, 10, 0)
	mustFreeze(t, s, "E1", "P001", "站点A", "疑似破损", t1)
	t2 := tClock(2026, 10, 5, 11, 0)
	first, replayed, err := s.Unfreeze("U1", "E1", "复核无异常", t2)
	if err != nil || replayed {
		t.Fatalf("首次解除失败: %v replayed=%v", err, replayed)
	}

	// 之后发生新的冻结与流转。
	mustFreeze(t, s, "E2", "P001", "站点A", "再次冻结", tClock(2026, 10, 5, 12, 0))

	// 同请求号同内容：重放首次结果与时间，不检查当前状态，不影响新异常单。
	got, replayed, err := s.Unfreeze("U1", "E1", "复核无异常", tClock(2026, 10, 5, 13, 0))
	if err != nil || !replayed || !got.Time.Equal(t2) || got != first {
		t.Fatalf("同内容重放应返回首次结果: %v replayed=%v got=%+v", err, replayed, got)
	}
	p, _ := s.Query("P001")
	if p.Status != statusFrozen || p.FrozenBy != "E2" || len(p.Trail) != 4 {
		t.Fatalf("重放不得影响后来新建的异常单: %+v", p)
	}

	// 同请求号换内容：冲突。
	if _, _, err := s.Unfreeze("U1", "E2", "复核无异常", tClock(2026, 10, 5, 13, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同号换异常单应报冲突，得到 %v", err)
	}
	if _, _, err := s.Unfreeze("U1", "E1", "别的说明", tClock(2026, 10, 5, 13, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("同号换说明应报冲突，得到 %v", err)
	}
}

func TestFreezeUnfreezeRequestScopeIndependent(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 9, 30))

	// 异常单号可与包裹号、交接请求号同名；解除请求号可与异常单号同名。
	mustFreeze(t, s, "P001", "P001", "站点B", "与包裹同名的异常单", tClock(2026, 10, 5, 10, 0))
	mustUnfreeze(t, s, "P001", "P001", "解除请求号与异常单号同名", tClock(2026, 10, 5, 11, 0))
	mustFreeze(t, s, "R1", "P001", "站点B", "与交接请求号同名的异常单", tClock(2026, 10, 5, 12, 0))
	mustUnfreeze(t, s, "R1", "R1", "解除请求号与交接请求号同名", tClock(2026, 10, 5, 13, 0))

	p, _ := s.Query("P001")
	if p.Status != statusInStation || p.Station != "站点B" {
		t.Fatalf("独立去重范围互不占号: %+v", p)
	}
}

func TestReturnAfterUnfreezeStillAllowed(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))

	// 冻结再解除只是管理记录，不算新流转：原本可退回的交接仍可整批退回。
	mustFreeze(t, s, "E1", "P001", "站点B", "疑似破损", tClock(2026, 10, 5, 11, 0))
	mustUnfreeze(t, s, "U1", "E1", "复核无异常", tClock(2026, 10, 5, 12, 0))
	res, _, err := s.Return("RT1", "R1", "错发站点", tClock(2026, 10, 5, 13, 0))
	if err != nil {
		t.Fatalf("解除后原本可退回的交接应仍可退回: %v", err)
	}
	if res.From != "站点B" || res.To != "站点A" {
		t.Fatalf("退回结果不符: %+v", res)
	}
	p, _ := s.Query("P001")
	if p.Station != "站点A" || p.Status != statusInStation {
		t.Fatalf("退回后不符: %+v", p)
	}
}

func TestNewFlowAfterUnfreezeBlocksOldReturn(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	mustFreeze(t, s, "E1", "P001", "站点B", "疑似破损", tClock(2026, 10, 5, 11, 0))
	mustUnfreeze(t, s, "U1", "E1", "复核无异常", tClock(2026, 10, 5, 12, 0))

	// 解除后发生新的交接：旧交接不能退回（即使包裹又回到了原交接目的站）。
	mustHandoff(t, s, "R2", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 13, 0))
	mustHandoff(t, s, "R3", "站点C", "站点B", []string{"P001"}, tClock(2026, 10, 5, 14, 0))
	if _, _, err := s.Return("RT1", "R1", "错发", tClock(2026, 10, 5, 15, 0)); err == nil ||
		!strings.Contains(err.Error(), "新的流转") {
		t.Fatalf("新交接后退回旧交接应失败，得到 %v", err)
	}
}

func TestFreezePersistenceAcrossRestart(t *testing.T) {
	_, path := openTempStore(t)
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s1, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustFreeze(t, s1, "E1", "P001", "站点A", "疑似破损", tClock(2026, 10, 5, 10, 0))
	mustUnfreeze(t, s1, "U1", "E1", "复核无异常", tClock(2026, 10, 5, 11, 0))
	mustFreeze(t, s1, "E2", "P001", "站点A", "再次冻结", tClock(2026, 10, 5, 12, 0))

	// 重新打开：状态、轨迹、异常单与去重规则不变。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	p, err := s2.Query("P001")
	if err != nil || p.Status != statusFrozen || p.FrozenBy != "E2" || len(p.Trail) != 4 {
		t.Fatalf("重启后数据不符: %+v err=%v", p, err)
	}
	if _, replayed, err := s2.Freeze("E1", "P001", "站点A", "疑似破损", tClock(2026, 10, 5, 13, 0)); err != nil || !replayed {
		t.Fatalf("重启后冻结重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	if _, replayed, err := s2.Unfreeze("U1", "E1", "复核无异常", tClock(2026, 10, 5, 13, 0)); err != nil || !replayed {
		t.Fatalf("重启后解除重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	if _, _, err := s2.Unfreeze("U9", "E1", "换请求号再解除", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("重启后已解除的异常单不能再次解除")
	}
	if _, _, err := s2.Handoff("R9", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("重启后冻结包裹仍不能交接")
	}
}

func TestCorruptFreezeDataRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")

	parcel := `"parcels":{"P001":{"id":"P001","station":"站点A","status":"异常冻结","registered":"2026-10-05T09:00:00Z",` +
		`"frozenBy":"E1","trail":[{"op":"收件","station":"站点A","time":"2026-10-05T09:00:00Z"}]}}`
	freeze := `"freezes":{"E1":{"exception":"E1","parcel":"P001","station":"站点A","reason":"疑似破损","time":"2026-10-05T10:00:00Z"}}`

	cases := map[string]string{
		"冻结引用不存在异常单": `{"version":1,"handoffs":{},` + parcel + `,"freezes":{}}`,
		"冻结引用不匹配异常单": `{"version":1,"handoffs":{},` + strings.Replace(parcel, `"frozenBy":"E1"`, `"frozenBy":"E2"`, 1) + `,` + freeze + `}`,
		"冻结状态缺少异常单":  `{"version":1,"handoffs":{},` + strings.Replace(parcel, `,"frozenBy":"E1"`, ``, 1) + `,` + freeze + `}`,
		"异常单引用丢失包裹":  `{"version":1,"handoffs":{},"parcels":{},` + freeze + `}`,
		"解除记录无法对应": `{"version":1,"handoffs":{},` + parcel + `,` + strings.Replace(freeze, `"time":"2026-10-05T10:00:00Z"}`,
			`"time":"2026-10-05T10:00:00Z","releasedBy":"U9"}`, 1) + `,"unfreezes":{"U1":{"request":"U1","exception":"E1","note":"x","time":"2026-10-05T11:00:00Z"}}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil {
				t.Fatal("损坏文件必须被拒绝打开")
			} else if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("应返回 ErrCorrupt，得到 %v", err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != content {
				t.Fatal("损坏文件不得被覆盖")
			}
		})
	}
}

func TestLegacyLedgerWithoutFreezesLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	legacy := `{"version":1,"parcels":{"P001":{"id":"P001","station":"站点A","status":"在站",` +
		`"registered":"2026-10-05T09:00:00Z","trail":[{"op":"收件","station":"站点A","time":"2026-10-05T09:00:00Z"}]}},"handoffs":{}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("旧有效台账应可直接使用: %v", err)
	}
	// 旧台账上可直接冻结与解除。
	mustFreeze(t, s, "E1", "P001", "站点A", "疑似破损", tClock(2026, 10, 5, 10, 0))
	mustUnfreeze(t, s, "U1", "E1", "复核无异常", tClock(2026, 10, 5, 11, 0))
	if p, _ := s.Query("P001"); p.Status != statusInStation {
		t.Fatalf("旧台账解除后应恢复在站: %+v", p)
	}
}

func TestCLIFreezeUnfreezeEndToEnd(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"

	runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A")
	runCLI(t, dbPath, "register", "--id", "P002", "--station", "站点A")

	// 冻结：输出可辨认异常单号、包裹、站点、原因。
	out, _, code := runCLI(t, dbPath, "freeze", "--exception", "E1", "--parcel", "P001",
		"--station", "站点A", "--reason", "疑似破损")
	if code != 0 || !strings.Contains(out, "冻结成功") ||
		!strings.Contains(out, "异常单号: E1") || !strings.Contains(out, "包裹编号: P001") ||
		!strings.Contains(out, "所在站点: 站点A") || !strings.Contains(out, "冻结原因: 疑似破损") {
		t.Fatalf("冻结输出不符: code=%d out=%s", code, out)
	}

	// query 展示当前状态、当前未解除异常与冻结轨迹。
	out, _, code = runCLI(t, dbPath, "query", "--id", "P001")
	if code != 0 || !strings.Contains(out, "当前状态: 异常冻结") ||
		!strings.Contains(out, "当前异常（未解除）") || !strings.Contains(out, "异常单号: E1") ||
		!strings.Contains(out, "冻结原因: 疑似破损") ||
		!strings.Contains(out, "2. 操作: 冻结") || !strings.Contains(out, "原因: 疑似破损") {
		t.Fatalf("冻结后查询不符: code=%d out=%s", code, out)
	}

	// 冻结期间整批交接拒绝，其余成员不变。
	_, errText, code := runCLI(t, dbPath, "handoff", "--request", "R1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002")
	if code != exitBusiness || !strings.Contains(errText, "整次交接未执行") {
		t.Fatalf("冻结包裹的交接应整批拒绝: code=%d err=%s", code, errText)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P002")
	if !strings.Contains(out, "当前站点: 站点A") || strings.Contains(out, "交接") {
		t.Fatalf("整批拒绝不得改动其他成员: %s", out)
	}

	// 同号同内容重放：返回首次结果，不再次冻结。
	out, _, code = runCLI(t, dbPath, "freeze", "--exception", " E1 ", "--parcel", " P001 ",
		"--station", " 站点A ", "--reason", " 疑似破损 ")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("冻结重放不符: code=%d out=%s", code, out)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if strings.Count(out, "操作: 冻结") != 1 {
		t.Fatalf("重放不得追加冻结轨迹:\n%s", out)
	}

	// 同号换内容：冲突。
	_, errText, code = runCLI(t, dbPath, "freeze", "--exception", "E1", "--parcel", "P001",
		"--station", "站点A", "--reason", "别的原因")
	if code != exitBusiness || !strings.Contains(errText, "冲突") {
		t.Fatalf("冻结同号换内容应冲突: code=%d err=%s", code, errText)
	}

	// 解除：包裹在原站恢复在站。
	out, _, code = runCLI(t, dbPath, "unfreeze", "--request", "U1", "--exception", "E1", "--note", "复核无异常")
	if code != 0 || !strings.Contains(out, "解除成功") ||
		!strings.Contains(out, "解除请求号: U1") || !strings.Contains(out, "异常单号: E1") ||
		!strings.Contains(out, "包裹编号: P001") || !strings.Contains(out, "处理说明: 复核无异常") {
		t.Fatalf("解除输出不符: code=%d out=%s", code, out)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if !strings.Contains(out, "当前状态: 在站") || strings.Contains(out, "当前异常（未解除）") ||
		!strings.Contains(out, "3. 操作: 解除") || !strings.Contains(out, "解除请求号: U1") ||
		!strings.Contains(out, "处理说明: 复核无异常") {
		t.Fatalf("解除后查询不符:\n%s", out)
	}

	// 解除重放：同请求号同内容返回首次结果。
	out, _, code = runCLI(t, dbPath, "unfreeze", "--request", "U1", "--exception", "E1", "--note", "复核无异常")
	if code != 0 || !strings.Contains(out, "返回首次保存的结果") {
		t.Fatalf("解除重放不符: code=%d out=%s", code, out)
	}
	out, _, _ = runCLI(t, dbPath, "query", "--id", "P001")
	if strings.Count(out, "操作: 解除") != 1 {
		t.Fatalf("重放不得追加解除轨迹:\n%s", out)
	}

	// 解除后交接恢复可用。
	out, _, code = runCLI(t, dbPath, "handoff", "--request", "R1", "--from", "站点A", "--to", "站点B",
		"--parcel", "P001", "--parcel", "P002")
	if code != 0 || !strings.Contains(out, "交接成功") {
		t.Fatalf("解除后交接应成功: code=%d out=%s", code, out)
	}
}

func TestCLIFreezeUnfreezeValidation(t *testing.T) {
	dbPath := t.TempDir() + "/ledger.json"
	runCLI(t, dbPath, "register", "--id", "P001", "--station", "站点A")

	cases := []struct {
		args   []string
		errSub string
	}{
		{[]string{"freeze", "--exception", "  ", "--parcel", "P001", "--station", "站点A", "--reason", "x"}, "不可为空"},
		{[]string{"freeze", "--exception", "E1", "--parcel", "", "--station", "站点A", "--reason", "x"}, "不可为空"},
		{[]string{"freeze", "--exception", "E1", "--parcel", "P001", "--station", "站点A", "--reason", " \t"}, "不可为空"},
		{[]string{"freeze", "--exception", "E1", "--parcel", "GHOST", "--station", "站点A", "--reason", "x"}, "未登记"},
		{[]string{"unfreeze", "--request", "", "--exception", "E1", "--note", "x"}, "不可为空"},
		{[]string{"unfreeze", "--request", "U1", "--exception", "E1", "--note", "  "}, "不可为空"},
		{[]string{"unfreeze", "--request", "U1", "--exception", "NOPE", "--note", "x"}, "不存在"},
	}
	for _, c := range cases {
		var out, errb strings.Builder
		full := append([]string{"--data", dbPath}, c.args...)
		code := run(full, &out, &errb)
		if code != exitBusiness || !strings.Contains(errb.String(), c.errSub) {
			t.Fatalf("%v 应失败(1)且提示 %q，得到 code=%d err=%q", c.args, c.errSub, code, errb.String())
		}
	}
}

func TestCLIFreezeUnfreezeHelpVariants(t *testing.T) {
	for _, args := range [][]string{
		{"freeze", "--help"},
		{"unfreeze", "-h"},
	} {
		var out, errb strings.Builder
		code := run(args, &out, &errb)
		if code != 0 || !strings.Contains(out.String(), appName) || errb.Len() != 0 {
			t.Fatalf("%v 帮助应以 0 退出且不含 stderr: code=%d", args, code)
		}
	}
}
