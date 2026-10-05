package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustFreeze(t *testing.T, s *Store, incident, parcel, station, reason string, now time.Time) {
	t.Helper()
	if _, _, err := s.Freeze(incident, parcel, station, reason, now); err != nil {
		t.Fatalf("Freeze(%q) 意外失败: %v", incident, err)
	}
}

func TestFreezeSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)

	t2 := tClock(2026, 10, 5, 10, 0)
	res, replayed, err := s.Freeze("E1", "P001", "站点A", "外包装破损", t2)
	if err != nil || replayed {
		t.Fatalf("冻结应成功且非重放: %v replayed=%v", err, replayed)
	}
	if res.Incident != "E1" || res.Parcel != "P001" || res.Station != "站点A" ||
		res.Reason != "外包装破损" || res.ReleasedBy != "" || !res.Time.Equal(t2) {
		t.Fatalf("冻结结果不符: %+v", res)
	}

	p, _ := s.Query("P001")
	if p.Station != "站点A" || p.Status != statusFrozen {
		t.Fatalf("冻结后站点不变、状态应为异常冻结: %+v", p)
	}
	if len(p.Trail) != 2 {
		t.Fatalf("应有两条轨迹，得到 %d", len(p.Trail))
	}
	e := p.Trail[1]
	if e.Op != "冻结" || e.Station != "站点A" || e.Incident != "E1" ||
		e.Reason != "外包装破损" || e.Request != "" || !e.Time.Equal(t2) {
		t.Fatalf("冻结记录不符: %+v", e)
	}
	af := s.ActiveFreeze("P001")
	if af == nil || af != res {
		t.Fatalf("当前未解除异常应为 E1: %+v", af)
	}
	if s.ActiveFreeze("NOPE") != nil {
		t.Fatal("不存在的包裹应无未解除异常")
	}
}

func TestFreezeFailuresAtomic(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P002"}, tClock(2026, 10, 5, 9, 30))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name             string
		incident, parcel string
		station, reason  string
	}{
		{"包裹未登记", "E1", "GHOST", "站点A", "原因"},
		{"站点不符", "E2", "P001", "站点B", "原因"},
		{"配送中不能冻结", "E3", "P002", "站点A", "原因"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Freeze(c.incident, c.parcel, c.station, c.reason, tClock(2026, 10, 5, 10, 0)); err == nil {
				t.Fatal("条件不满足时冻结必须失败")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("失败冻结不得写入数据文件")
			}
			if _, ok := s.data.Freezes[c.incident]; ok {
				t.Fatalf("失败的首次冻结不得占用异常单号 %q", c.incident)
			}
		})
	}

	// 已有未解除异常单：不能再次冻结。
	mustFreeze(t, s, "E1", "P001", "站点A", "破损", tClock(2026, 10, 5, 10, 0))
	if _, _, err := s.Freeze("E9", "P001", "站点A", "又坏了", tClock(2026, 10, 5, 10, 30)); err == nil {
		t.Fatal("一件包裹同时只能有一张未解除异常单")
	}
	if _, ok := s.data.Freezes["E9"]; ok {
		t.Fatal("被拒绝的冻结不得占用异常单号")
	}
	p1, _ := s.Query("P001")
	if p1.Status != statusFrozen || len(p1.Trail) != 2 {
		t.Fatalf("重复冻结不得改动包裹: %+v", p1)
	}

	// 已签收包裹不能冻结。
	if _, _, err := s.Receipt("RC1", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Freeze("E8", "P002", "站点A", "签收后异常", tClock(2026, 10, 5, 11, 30)); err == nil {
		t.Fatal("已签收包裹不能冻结")
	}

	// 失败不占号：纠正后同异常单号可成功（P001 已被 E1 冻结，这里对新包裹 P003 登记后使用 E3）。
	mustRegister(t, s, "P003", "站点A", t1)
	res, replayed, err := s.Freeze("E3", "P003", "站点A", "原因", tClock(2026, 10, 5, 12, 0))
	if err != nil || replayed || res.Parcel != "P003" {
		t.Fatalf("纠正后同异常单号冻结应成功: %v replayed=%v", err, replayed)
	}
}

func TestFreezeReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	first, _, err := s.Freeze("E1", "P001", "站点A", "破损", tClock(2026, 10, 5, 10, 0))
	if err != nil {
		t.Fatal(err)
	}

	// 同号、同包裹、同站点、清理后的原因：重放首次结果与时间，不再次冻结。
	// （Store 层信任调用方已清洗；两端空白的清洗等价性在 CLI 层验证。）
	again, replayed, err := s.Freeze("E1", "P001", "站点A", "破损", tClock(2026, 10, 5, 10, 30))
	if err != nil || !replayed || again != first {
		t.Fatalf("同内容重复冻结应重放: err=%v replayed=%v", err, replayed)
	}
	if p, _ := s.Query("P001"); len(p.Trail) != 2 {
		t.Fatalf("重放不得追加轨迹，得到 %d 条", len(p.Trail))
	}

	// 同号换内容：冲突。
	for _, c := range []struct {
		parcel, station, reason string
	}{
		{"P9", "站点A", "破损"},
		{"P001", "站点B", "破损"},
		{"P001", "站点A", "别的原因"},
	} {
		if _, _, err := s.Freeze("E1", c.parcel, c.station, c.reason, tClock(2026, 10, 5, 11, 0)); err == nil {
			t.Fatalf("异常单号相同但内容不同（%+v）必须报冲突", c)
		}
	}
	saved := s.data.Freezes["E1"]
	if saved != first || saved.Reason != "破损" {
		t.Fatalf("冲突提交不得改动已有冻结结果: %+v", saved)
	}

	// 解除后同号同内容仍重放首次结果，不检查当前状态、不再次冻结。
	if _, _, err := s.Unfreeze("U1", "E1", "核实放行", tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Handoff("R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 13, 0)); err != nil {
		t.Fatal(err)
	}
	replay, replayed2, err := s.Freeze("E1", "P001", "站点A", "破损", tClock(2026, 10, 5, 14, 0))
	if err != nil || !replayed2 || replay != first {
		t.Fatalf("解除且包裹流转后重放仍须返回首次结果: err=%v replayed=%v", err, replayed2)
	}
	p, _ := s.Query("P001")
	if p.Station != "站点B" || p.Status != statusInStation || len(p.Trail) != 4 {
		t.Fatalf("重放不得移动包裹或追加冻结记录: %+v", p)
	}
	// 异常单解除后不能复用为新冻结。
	if _, _, err := s.Freeze("E1", "P001", "站点B", "破损", tClock(2026, 10, 5, 14, 30)); err == nil {
		t.Fatal("已使用的异常单号不能复用")
	}
}

func TestUnfreezeSuccessAndRefreeze(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustFreeze(t, s, "E1", "P001", "站点A", "破损", tClock(2026, 10, 5, 10, 0))

	t2 := tClock(2026, 10, 5, 11, 0)
	res, replayed, err := s.Unfreeze("U1", "E1", "已核实放行", t2)
	if err != nil || replayed {
		t.Fatalf("解除应成功且非重放: %v replayed=%v", err, replayed)
	}
	if res.Request != "U1" || res.Incident != "E1" || res.Parcel != "P001" ||
		res.Note != "已核实放行" || !res.Time.Equal(t2) {
		t.Fatalf("解除结果不符: %+v", res)
	}

	p, _ := s.Query("P001")
	if p.Station != "站点A" || p.Status != statusInStation {
		t.Fatalf("解除后应在原站恢复在站: %+v", p)
	}
	if len(p.Trail) != 3 {
		t.Fatalf("应有三条轨迹，得到 %d", len(p.Trail))
	}
	e := p.Trail[2]
	if e.Op != "解除冻结" || e.Station != "站点A" || e.Incident != "E1" ||
		e.Request != "U1" || e.Note != "已核实放行" || !e.Time.Equal(t2) {
		t.Fatalf("解除记录不符: %+v", e)
	}
	// 原冻结记录不删改。
	if f := p.Trail[1]; f.Op != "冻结" || f.Incident != "E1" {
		t.Fatalf("原冻结记录被改写: %+v", f)
	}
	f := s.data.Freezes["E1"]
	if f.ReleasedBy != "U1" {
		t.Fatalf("原异常单应永久标记为已解除: %+v", f)
	}
	if s.ActiveFreeze("P001") != nil {
		t.Fatal("解除后应无未解除异常")
	}

	// 可用新异常单再次冻结；旧异常单号同内容提交只是重放首次结果，不会产生第二张异常单。
	mustFreeze(t, s, "E2", "P001", "站点A", "再次异常", tClock(2026, 10, 5, 12, 0))
	if p, _ := s.Query("P001"); p.Status != statusFrozen || len(p.Trail) != 4 {
		t.Fatalf("新异常单应可再次冻结: %+v", p)
	}
	old, replayed, err := s.Freeze("E1", "P001", "站点A", "破损", tClock(2026, 10, 5, 12, 30))
	if err != nil || !replayed || old.Incident != "E1" {
		t.Fatalf("已解除异常单号的同内容提交应重放而非复用: err=%v replayed=%v", err, replayed)
	}
	if p, _ := s.Query("P001"); len(p.Trail) != 4 {
		t.Fatalf("旧异常单号重放不得追加冻结记录，得到 %d 条", len(p.Trail))
	}
	if af := s.ActiveFreeze("P001"); af == nil || af.Incident != "E2" {
		t.Fatalf("当前未解除异常仍应为 E2: %+v", af)
	}
}

func TestUnfreezeFailuresAtomic(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustFreeze(t, s, "E1", "P001", "站点A", "破损", tClock(2026, 10, 5, 10, 0))

	// 异常单不存在。
	if _, _, err := s.Unfreeze("U1", "NOPE", "处理", tClock(2026, 10, 5, 10, 30)); err == nil {
		t.Fatal("异常单不存在时解除必须失败")
	}
	if _, ok := s.data.Unfreezes["U1"]; ok {
		t.Fatal("失败的首次解除不得占用请求号")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 首次解除成功。
	if _, _, err := s.Unfreeze("U1", "E1", "放行", tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatal(err)
	}
	// 异常单已解除：再次解除拒绝（即便使用新请求号）。
	if _, _, err := s.Unfreeze("U2", "E1", "再处理", tClock(2026, 10, 5, 11, 30)); err == nil {
		t.Fatal("已解除的异常单不能重复解除")
	}
	after, _ := os.ReadFile(path)
	if string(after) == string(before) {
		t.Fatal("U1 成功解除应已落盘")
	}
	if _, ok := s.data.Unfreezes["U2"]; ok {
		t.Fatal("失败的解除不得占用请求号 U2")
	}
	p1, _ := s.Query("P001")
	if p1.Status != statusInStation || len(p1.Trail) != 3 {
		t.Fatalf("失败解除不得改动包裹: %+v", p1)
	}

	// 旧异常单已解除、新异常单冻结中：用旧单号解除应报“已解除”，不得影响当前冻结。
	mustFreeze(t, s, "E2", "P001", "站点A", "又坏了", tClock(2026, 10, 5, 12, 0))
	stable, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Unfreeze("U3", "E1", "放行", tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatal("已解除异常单不影响后来新建的异常单，旧单仍须拒绝")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(stable) {
		t.Fatal("被拒绝的解除不得写入数据文件")
	}
	if p, _ := s.Query("P001"); p.Status != statusFrozen || len(p.Trail) != 4 {
		t.Fatalf("失败解除不得影响当前冻结: %+v", p)
	}
	if _, ok := s.data.Unfreezes["U3"]; ok {
		t.Fatal("被拒绝的解除不得占用请求号")
	}
}

func TestUnfreezeReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustFreeze(t, s, "E1", "P001", "站点A", "破损", tClock(2026, 10, 5, 10, 0))
	first, _, err := s.Unfreeze("U1", "E1", "放行", tClock(2026, 10, 5, 11, 0))
	if err != nil {
		t.Fatal(err)
	}

	// 同请求号、同异常单、同说明：重放首次结果，不检查当前状态。
	again, replayed, err := s.Unfreeze("U1", "E1", "放行", tClock(2026, 10, 5, 11, 30))
	if err != nil || !replayed || again != first {
		t.Fatalf("同内容重复解除应重放: err=%v replayed=%v", err, replayed)
	}
	if p, _ := s.Query("P001"); len(p.Trail) != 3 {
		t.Fatalf("重放不得追加轨迹，得到 %d 条", len(p.Trail))
	}

	// 后来用新异常单再次冻结：旧解除请求重放仍返回首次结果，不影响现状。
	mustFreeze(t, s, "E2", "P001", "站点A", "又坏了", tClock(2026, 10, 5, 12, 0))
	replay, replayed2, err := s.Unfreeze("U1", "E1", "放行", tClock(2026, 10, 5, 12, 30))
	if err != nil || !replayed2 || replay != first {
		t.Fatalf("新异常单存在时旧解除重放仍须返回首次结果: err=%v replayed=%v", err, replayed2)
	}
	if p, _ := s.Query("P001"); p.Status != statusFrozen || len(p.Trail) != 4 {
		t.Fatalf("重放不得影响后来新建的异常单: %+v", p)
	}

	// 同请求号换异常单或说明：冲突。
	if _, _, err := s.Unfreeze("U1", "E2", "放行", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("换异常单必须报冲突")
	}
	if _, _, err := s.Unfreeze("U1", "E1", "别的说明", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("换处理说明必须报冲突")
	}
	saved := s.data.Unfreezes["U1"]
	if saved != first || saved.Incident != "E1" || saved.Note != "放行" {
		t.Fatalf("冲突提交不得改动已有解除结果: %+v", saved)
	}

	// 失败不占号：U2 首次解除不存在的异常单失败，纠正后可成功解除 E2。
	if _, _, err := s.Unfreeze("U2", "NOPE", "x", tClock(2026, 10, 5, 13, 30)); err == nil {
		t.Fatal("异常单不存在应失败")
	}
	if _, ok := s.data.Unfreezes["U2"]; ok {
		t.Fatal("失败的首次解除不得占用请求号")
	}
	if _, _, err := s.Unfreeze("U2", "E2", "放行", tClock(2026, 10, 5, 14, 0)); err != nil {
		t.Fatalf("纠正后同请求号解除应成功: %v", err)
	}
}

func TestFreezeBlocksBatchOperations(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	mustFreeze(t, s, "E1", "P002", "站点A", "破损", tClock(2026, 10, 5, 10, 0))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 首次交接含冻结包裹：整批拒绝，P001/P003 状态与轨迹不变。
	if _, _, err := s.Handoff("R1", "站点A", "站点B", []string{"P001", "P002", "P003"}, tClock(2026, 10, 5, 11, 0)); err == nil {
		t.Fatal("冻结期间含冻结包裹的交接必须整批拒绝")
	}
	for _, id := range []string{"P001", "P003"} {
		if p, _ := s.Query(id); p.Station != "站点A" || len(p.Trail) != 1 {
			t.Fatalf("整批拒绝不得改动其他成员 %s: %+v", id, p)
		}
	}
	if _, ok := s.data.Handoffs["R1"]; ok {
		t.Fatal("被拒绝的交接不得占用请求号")
	}

	// 首次出站含冻结包裹：整批拒绝。
	if _, _, err := s.Dispatch("B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 11, 30)); err == nil {
		t.Fatal("冻结期间含冻结包裹的出站必须整批拒绝")
	}
	if _, ok := s.data.Batches["B1"]; ok {
		t.Fatal("被拒绝的出站不得占用批次号")
	}
	if p, _ := s.Query("P001"); p.Status != statusInStation || len(p.Trail) != 1 {
		t.Fatalf("整批拒绝不得改动 P001: %+v", p)
	}
	if p, _ := s.Query("P002"); p.Status != statusFrozen {
		t.Fatal("整批拒绝不得解除 P002 的冻结")
	}

	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("被拒绝的整批操作不得写入数据文件")
	}

	// 失败不占号：解除后用相同请求号、批次号可成功。
	if _, _, err := s.Unfreeze("U1", "E1", "放行", tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Handoff("R1", "站点A", "站点B", []string{"P001", "P002", "P003"}, tClock(2026, 10, 5, 13, 0)); err != nil {
		t.Fatalf("解除后同请求号交接应成功: %v", err)
	}
	if _, replayed, err := s.Dispatch("B1", "站点B", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 14, 0)); err != nil || replayed {
		t.Fatalf("解除后同批次号出站应成功（首次）: err=%v replayed=%v", err, replayed)
	}
	_ = path
}

func TestFreezeBlocksReturnContainingFrozenParcel(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	mustFreeze(t, s, "E1", "P002", "站点B", "破损", tClock(2026, 10, 5, 11, 0))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 整批退回 R1（含冻结中的 P002）：整批拒绝，P001 也不动。
	if _, _, err := s.Return("RT1", "R1", "错发", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("冻结期间含冻结包裹的整批退回必须拒绝")
	}
	// 未冻结成员 P001 保持交接后状态；冻结成员 P002 保持冻结，冻结记录不被改动。
	if p, _ := s.Query("P001"); p.Station != "站点B" || p.Status != statusInStation || len(p.Trail) != 2 {
		t.Fatalf("整批拒绝不得改动 P001: %+v", p)
	}
	if p, _ := s.Query("P002"); p.Station != "站点B" || p.Status != statusFrozen || len(p.Trail) != 3 {
		t.Fatalf("整批拒绝不得改动 P002: %+v", p)
	}
	if _, ok := s.data.Returns["RT1"]; ok {
		t.Fatal("被拒绝的退回不得占用请求号")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("被拒绝的退回不得写入数据文件")
	}
	_ = path
}

func TestFrozenReplayReturnsHistoricalResults(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	// 历史交接 R1 后冻结。
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	mustFreeze(t, s, "E1", "P001", "站点B", "破损", tClock(2026, 10, 5, 11, 0))

	// 冻结期间历史交接同内容重放：返回首次结果，不移动包裹、不解除冻结。
	h, replayedH, err := s.Handoff("R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 11, 30))
	if err != nil || !replayedH || h.To != "站点B" {
		t.Fatalf("冻结期间历史交接重放应返回首次结果: err=%v replayed=%v", err, replayedH)
	}
	if p, _ := s.Query("P001"); p.Station != "站点B" || p.Status != statusFrozen || len(p.Trail) != 3 {
		t.Fatalf("交接重放不得移动包裹或解除冻结: %+v", p)
	}

	// 解除后完成一次历史退回 RT1（验证冻结不算流转），再重新冻结，退回重放仍返回历史结果。
	if _, _, err := s.Unfreeze("U1", "E1", "放行", tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatal(err)
	}
	rt, _, err := s.Return("RT1", "R1", "错发", tClock(2026, 10, 5, 13, 0))
	if err != nil {
		t.Fatalf("解除后无新流转时旧交接应仍可退回: %v", err)
	}
	mustFreeze(t, s, "E2", "P001", "站点A", "又坏了", tClock(2026, 10, 5, 14, 0))
	again, replayed, err := s.Return("RT1", "R1", "错发", tClock(2026, 10, 5, 14, 30))
	if err != nil || !replayed || again != rt {
		t.Fatalf("冻结期间历史退回应重放首次结果: err=%v replayed=%v", err, replayed)
	}
	if p, _ := s.Query("P001"); p.Station != "站点A" || p.Status != statusFrozen || len(p.Trail) != 6 {
		t.Fatalf("退回重放不得移动包裹或解除冻结: %+v", p)
	}

	// 历史出站/回执的同内容重放同样不受冻结影响（用另一件包裹构造）。
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P002"}, tClock(2026, 10, 5, 15, 0))
	if _, _, err := s.Receipt("RC1", "B1", "P002", resultFailed, "无人", tClock(2026, 10, 5, 15, 30)); err != nil {
		t.Fatal(err)
	}
	mustFreeze(t, s, "E3", "P002", "站点A", "破损", tClock(2026, 10, 5, 16, 0))
	if _, replayed, err := s.Dispatch("B1", "站点A", "张三", []string{"P002"}, tClock(2026, 10, 5, 16, 30)); err != nil || !replayed {
		t.Fatalf("冻结期间历史出站重放应返回首次结果: err=%v replayed=%v", err, replayed)
	}
	if _, replayed, err := s.Receipt("RC1", "B1", "P002", resultFailed, "无人", tClock(2026, 10, 5, 16, 45)); err != nil || !replayed {
		t.Fatalf("冻结期间历史回执重放应返回首次结果: err=%v replayed=%v", err, replayed)
	}
	if p, _ := s.Query("P002"); p.Status != statusFrozen {
		t.Fatalf("历史结果重放不得解除冻结: %+v", p)
	}
}

func TestFreezeNotANewFlow(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))

	// 冻结再解除：没有其他新流转，原本可退回的交接仍可整批退回。
	mustFreeze(t, s, "E1", "P001", "站点B", "破损", tClock(2026, 10, 5, 11, 0))
	if _, _, err := s.Unfreeze("U1", "E1", "放行", tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Return("RT1", "R1", "错发", tClock(2026, 10, 5, 13, 0)); err != nil {
		t.Fatalf("冻结/解除不算新流转，旧交接应仍可退回: %v", err)
	}
	p, _ := s.Query("P001")
	if p.Station != "站点A" || p.Status != statusInStation {
		t.Fatalf("退回应成功且包裹回到站点A: %+v", p)
	}

	// 发生过新的交接、退回、出站或回执后，仍不能退回旧交接（冻结/解除穿插其间）。
	mustHandoff(t, s, "R2", "站点A", "站点C", []string{"P001"}, tClock(2026, 10, 5, 14, 0))
	mustFreeze(t, s, "E2", "P001", "站点C", "破损", tClock(2026, 10, 5, 14, 30))
	if _, _, err := s.Unfreeze("U2", "E2", "放行", tClock(2026, 10, 5, 15, 0)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Return("RT2", "R1", "错发", tClock(2026, 10, 5, 15, 30)); err == nil {
		t.Fatal("发生新交接后不能退回旧交接")
	}
}

func TestFreezeIDNamespacesIndependent(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "X", "站点A", t1)
	// 异常单号可与包裹号、交接请求号、批次号、各类请求号同名。
	mustHandoff(t, s, "X", "站点A", "站点B", []string{"X"}, tClock(2026, 10, 5, 10, 0))
	if _, _, err := s.Freeze("X", "X", "站点B", "同名异常", tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatalf("异常单号与其他业务编号同名应可用: %v", err)
	}
	// 解除请求号同样可与异常单号、包裹号同名。
	if _, _, err := s.Unfreeze("X", "X", "同名解除", tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatalf("解除请求号与其他编号同名应可用: %v", err)
	}
	// 同名交接的重放不受影响。
	if _, replayed, err := s.Handoff("X", "站点A", "站点B", []string{"X"}, tClock(2026, 10, 5, 13, 0)); err != nil || !replayed {
		t.Fatalf("同名编号不得影响交接去重: err=%v replayed=%v", err, replayed)
	}
	// 同内容冻结/解除重放各自命中自己的去重表。
	if _, replayed, err := s.Freeze("X", "X", "站点B", "同名异常", tClock(2026, 10, 5, 13, 30)); err != nil || !replayed {
		t.Fatalf("冻结重放失败: err=%v replayed=%v", err, replayed)
	}
	if _, replayed, err := s.Unfreeze("X", "X", "同名解除", tClock(2026, 10, 5, 14, 0)); err != nil || !replayed {
		t.Fatalf("解除重放失败: err=%v replayed=%v", err, replayed)
	}
}

func TestFreezePersistenceAcrossRestart(t *testing.T) {
	_, path := openTempStore(t)
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s1, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustFreeze(t, s1, "E1", "P001", "站点A", "破损", tClock(2026, 10, 5, 10, 0))

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	p, _ := s2.Query("P001")
	if p.Status != statusFrozen || p.Station != "站点A" || len(p.Trail) != 2 ||
		p.Trail[1].Op != "冻结" || p.Trail[1].Incident != "E1" {
		t.Fatalf("重启后冻结状态不符: %+v", p)
	}
	if af := s2.ActiveFreeze("P001"); af == nil || af.Incident != "E1" || af.Reason != "破损" {
		t.Fatalf("重启后未解除异常查询不符: %+v", af)
	}
	// 重启后冻结重放成立、换内容冲突、冻结拦截仍生效。
	if _, replayed, err := s2.Freeze("E1", "P001", "站点A", "破损", tClock(2026, 10, 5, 10, 30)); err != nil || !replayed {
		t.Fatalf("重启后冻结重放失败: err=%v replayed=%v", err, replayed)
	}
	if _, _, err := s2.Handoff("R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 11, 0)); err == nil {
		t.Fatal("重启后冻结期间仍须拒绝交接")
	}
	// 解除后再次重启：解除记录持久化，旧单已解除、可换新单再冻结。
	if _, _, err := s2.Unfreeze("U1", "E1", "放行", tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("再次重新打开失败: %v", err)
	}
	if _, _, err := s3.Unfreeze("U2", "E1", "x", tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatal("重启后已解除异常单仍须拒绝重复解除")
	}
	if _, replayed, err := s3.Unfreeze("U1", "E1", "放行", tClock(2026, 10, 5, 13, 0)); err != nil || !replayed {
		t.Fatalf("重启后解除重放失败: err=%v replayed=%v", err, replayed)
	}
	if _, _, err := s3.Freeze("E2", "P001", "站点A", "新异常", tClock(2026, 10, 5, 13, 30)); err != nil {
		t.Fatalf("重启后应可用新异常单再冻结: %v", err)
	}
}

func TestCorruptFreezeDataRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")

	parcel := `"id":"P1","station":"A","status":"在站","registered":"2026-10-05T09:00:00Z"`
	reg := `{"op":"收件","station":"A","time":"2026-10-05T09:00:00Z"}`
	frozenParcel := `"id":"P1","station":"A","status":"异常冻结","registered":"2026-10-05T09:00:00Z"`
	freezeEvent := `{"op":"冻结","station":"A","time":"2026-10-05T10:00:00Z","incident":"E1","reason":"x"}`
	freezeResult := `"freezes":{"E1":{"incident":"E1","parcel":"P1","station":"A","reason":"x","time":"2026-10-05T10:00:00Z"}}`
	cases := map[string]string{
		"冻结结果为null": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":[` + reg + `]}},"handoffs":{},` +
			`"freezes":{"E1":null}}`,
		"冻结结果缺少原因": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":[` + reg + `]}},"handoffs":{},` +
			`"freezes":{"E1":{"incident":"E1","parcel":"P1","station":"A","reason":"","time":"2026-10-05T10:00:00Z"}}}`,
		"冻结结果引用不存在包裹": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":[` + reg + `]}},"handoffs":{},` +
			`"freezes":{"E1":{"incident":"E1","parcel":"P9","station":"A","reason":"x","time":"2026-10-05T10:00:00Z"}}}`,
		"冻结轨迹引用不存在的异常单": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":[` + reg + `,` + freezeEvent + `]}},"handoffs":{},"freezes":{}}`,
		"冻结轨迹与结果不匹配": `{"version":1,"parcels":{"P1":{` + frozenParcel + `,"trail":[` + reg + `,` +
			`{"op":"冻结","station":"A","time":"2026-10-05T10:00:00Z","incident":"E1","reason":"别的"}]}},"handoffs":{},` +
			freezeResult + `}`,
		"状态冻结但无未解除冻结记录": `{"version":1,"parcels":{"P1":{` + frozenParcel + `,"trail":[` + reg + `]}},"handoffs":{},"freezes":{}}`,
		"存在未解除异常单但状态在站": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":[` + reg + `]}},"handoffs":{},` +
			freezeResult + `}`,
		"解除记录引用不存在的异常单": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":[` + reg + `]}},"handoffs":{},` +
			`"unfreezes":{"U1":{"request":"U1","incident":"NOPE","parcel":"P1","note":"n","time":"2026-10-05T11:00:00Z"}}}`,
		"异常单解除标记无法对应": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":[` + reg + `]}},"handoffs":{},` +
			`"freezes":{"E1":{"incident":"E1","parcel":"P1","station":"A","reason":"x","time":"2026-10-05T10:00:00Z","releasedBy":"U9"}},"unfreezes":{}}`,
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
	// 没有 freeze 相关字段的旧台账应可直接使用新功能。
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	legacy := `{"version":1,"parcels":{"P1":{"id":"P1","station":"A","status":"在站",` +
		`"registered":"2026-10-05T09:00:00Z","trail":[{"op":"收件","station":"A","time":"2026-10-05T09:00:00Z"}]}},"handoffs":{}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("旧台账应可直接打开: %v", err)
	}
	if _, _, err := s.Freeze("E1", "P1", "A", "破损", tClock(2026, 10, 5, 10, 0)); err != nil {
		t.Fatalf("旧台账升级后应可冻结: %v", err)
	}
	if _, _, err := s.Unfreeze("U1", "E1", "放行", tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatalf("旧台账升级后应可解除: %v", err)
	}
}
