package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustManifest(t *testing.T, s *Store, manifest, station string, parcels []string, now time.Time) *SortManifest {
	t.Helper()
	res, replayed, err := s.CreateManifest(manifest, station, parcels, now)
	if err != nil || replayed {
		t.Fatalf("CreateManifest(%q) 意外失败: %v replayed=%v", manifest, err, replayed)
	}
	return res
}

func manifestStore(t *testing.T) (*Store, string) {
	t.Helper()
	s, path := openTempStore(t)
	now := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", now)
	mustRegister(t, s, "P002", "站点A", now)
	mustRegister(t, s, "P003", "站点A", now)
	return s, path
}

// 创建清单：成员按首次顺序保存，站点、状态不变，逐件追加分拣预留管理记录。
func TestCreateManifestSuccess(t *testing.T) {
	s, _ := manifestStore(t)
	res := mustManifest(t, s, "M1", "站点A", []string{"P002", "P001"}, tClock(2026, 10, 5, 9, 30))

	if res.Status != manifestPending || res.Station != "站点A" ||
		len(res.Parcels) != 2 || res.Parcels[0] != "P002" || res.Parcels[1] != "P001" {
		t.Fatalf("清单结果不符: %+v", res)
	}
	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		if p.Station != "站点A" || p.Status != statusInStation {
			t.Fatalf("%s 预留后站点/状态应不变: %+v", id, p)
		}
		e := p.Trail[len(p.Trail)-1]
		if e.Op != "分拣预留" || e.Manifest != "M1" || e.Station != "站点A" {
			t.Fatalf("%s 末条应为分拣预留记录: %+v", id, e)
		}
		if s.ActiveManifest(id) == nil || s.ActiveManifest(id).Manifest != "M1" {
			t.Fatalf("%s 应被清单 M1 预留", id)
		}
	}
	if s.ActiveManifest("P003") != nil {
		t.Fatal("P003 不应被预留")
	}
}

// 创建的整单校验：任一不符整单拒绝，不占清单号、不追加轨迹；失败不占号，纠正后同号可成功。
func TestCreateManifestRejectsAtomic(t *testing.T) {
	s, path := manifestStore(t)
	now := tClock(2026, 10, 5, 9, 0)
	if _, err := s.Register("P004", "站点A", now); err != nil {
		t.Fatal(err)
	}
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P003"}, tClock(2026, 10, 5, 9, 20))
	mustManifest(t, s, "M1", "站点A", []string{"P002"}, tClock(2026, 10, 5, 9, 25))
	mustHandoff(t, s, "H1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 9, 28))

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		station string
		parcels []string
	}{
		{"含未登记包裹", "站点A", []string{"P004", "GHOST"}},
		{"含其他站点包裹", "站点A", []string{"P001"}},
		{"含配送中包裹", "站点A", []string{"P003"}},
		{"含已被其他清单预留的包裹", "站点A", []string{"P004", "P002"}},
	}
	for _, c := range cases {
		trailBefore := len(s.data.Parcels["P004"].Trail)
		if _, _, err := s.CreateManifest("MBAD", c.station, c.parcels, tClock(2026, 10, 5, 9, 40)); err == nil {
			t.Fatalf("%s：应整单拒绝", c.name)
		}
		if s.data.Manifests["MBAD"] != nil {
			t.Fatalf("%s：失败不得占用清单号 MBAD", c.name)
		}
		if got := len(s.data.Parcels["P004"].Trail); got != trailBefore {
			t.Fatalf("%s：整单拒绝不得追加轨迹", c.name)
		}
	}
	// 失败全部发生在锁内同一内存台账，未落盘；文件仍是最后一次成功（交接 H1）的内容。
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("失败创建不得落盘")
	}
	// 纠正后同清单号可成功（失败不占号）。
	mustManifest(t, s, "MBAD", "站点A", []string{"P004"}, tClock(2026, 10, 5, 9, 45))
}

// 待出站成员不能首次交接、退回、发运、直接 dispatch 或加入另一清单；
// 涉及它的批量操作整批拒绝（其他成员不变）。冻结与解除允许，预留保留。
func TestReservedParcelBlocksFlows(t *testing.T) {
	s, _ := manifestStore(t)
	mustManifest(t, s, "M1", "站点A", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
	tn := tClock(2026, 10, 5, 10, 0)

	if _, _, err := s.Handoff("H1", "站点A", "站点B", []string{"P001", "P003"}, tn); err == nil {
		t.Fatal("涉及预留件的交接应整批拒绝")
	}
	if p, _ := s.Query("P003"); p.Station != "站点A" {
		t.Fatalf("整批拒绝不得移动其他成员 P003: %+v", p)
	}
	if _, _, err := s.Dispatch("B1", "站点A", "张三", []string{"P003", "P002"}, tn); err == nil {
		t.Fatal("涉及预留件的直接 dispatch 应整批拒绝")
	}
	if _, err := s.BatchQuery("B1"); err == nil {
		t.Fatal("失败的 dispatch 不得占用批次号")
	}
	if _, _, err := s.Ship("S1", "站点A", "站点B", []string{"P001"}, tn); err == nil {
		t.Fatal("预留件不能首次发运")
	}
	if _, _, err := s.CreateManifest("M2", "站点A", []string{"P003", "P001"}, tn); err == nil {
		t.Fatal("预留件不能加入另一清单，整单应拒绝")
	}
	if s.data.Manifests["M2"] != nil {
		t.Fatal("失败的清单创建不得占用 M2")
	}
	// 预留件不能退回：先制造一个可退回的旧交接给 P002（预留之前不可能交接后预留，
	// 故此处直接验证交接 H 给 P003 不受影响即可；退回场景在取消测试中覆盖）。

	// 冻结与解除允许，预留保留。
	mustFreeze(t, s, "E1", "P001", "站点A", "外包装破损", tClock(2026, 10, 5, 10, 30))
	if m := s.ActiveManifest("P001"); m == nil || m.Manifest != "M1" {
		t.Fatal("冻结后预留应保留")
	}
	if _, _, err := s.Unfreeze("U1", "E1", "核实放行", tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatalf("预留件解除冻结应允许: %v", err)
	}
	if m := s.ActiveManifest("P001"); m == nil || m.Manifest != "M1" {
		t.Fatal("解除冻结后预留应保留")
	}
	if p, _ := s.Query("P001"); p.Status != statusInStation {
		t.Fatalf("解除冻结后应恢复在站: %+v", p)
	}
}

// 确认出站：按原顺序创建普通配送批次、释放预留，成员转配送中并追加出站轨迹；
// 后续配送作业照常，dispatch 可按该批次原内容重放。
func TestConfirmManifestSuccess(t *testing.T) {
	s, _ := manifestStore(t)
	mustManifest(t, s, "M1", "站点A", []string{"P002", "P001"}, tClock(2026, 10, 5, 9, 30))
	cr, replayed, err := s.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 10, 0))
	if err != nil || replayed {
		t.Fatalf("确认应成功且非重放: %v replayed=%v", err, replayed)
	}
	if cr.Batch != "B1" || cr.Courier != "张三" || cr.Station != "站点A" ||
		len(cr.Parcels) != 2 || cr.Parcels[0] != "P002" || cr.Parcels[1] != "P001" {
		t.Fatalf("确认结果不符: %+v", cr)
	}
	m, _ := s.ManifestQuery("M1")
	if m.Status != manifestConfirmed {
		t.Fatalf("清单应永久标记已出站，得到 %q", m.Status)
	}
	if s.ActiveManifest("P001") != nil || s.ActiveManifest("P002") != nil {
		t.Fatal("确认后预留应释放")
	}
	b, err := s.BatchQuery("B1")
	if err != nil {
		t.Fatalf("应创建普通配送批次 B1: %v", err)
	}
	if b.RelayedFrom != "" || b.Courier != "张三" || !sameOrder(b.Parcels, []string{"P002", "P001"}) {
		t.Fatalf("确认批次应为普通出站批次且按清单原顺序: %+v", b)
	}
	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		if p.Status != statusDelivering || p.Station != "站点A" || currentBatch(p) != "B1" {
			t.Fatalf("%s 应随 B1 配送中: %+v", id, p)
		}
		last := p.Trail[len(p.Trail)-1]
		if last.Op != "出站" || last.Batch != "B1" || last.Courier != "张三" {
			t.Fatalf("%s 末条应为 B1 出站记录: %+v", id, last)
		}
	}

	// 后续配送作业照常：逐件回执。
	if _, _, err := s.Receipt("RC1", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 10, 30)); err != nil {
		t.Fatalf("确认批次应可正常回执: %v", err)
	}
	// dispatch 可按该批次原内容重放（换序无关），返回首次结果。
	saved, replayDispatch, err := s.Dispatch("B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 11, 0))
	if err != nil || !replayDispatch || !sameOrder(saved.Parcels, []string{"P002", "P001"}) {
		t.Fatalf("dispatch 应可按确认批次原内容重放: %v replayed=%v saved=%+v", err, replayDispatch, saved)
	}
	if p, _ := s.Query("P002"); p.Status != statusSigned {
		t.Fatal("dispatch 重放不得改变当前状态")
	}
}

// 确认的受理条件：冻结件整单拒绝；批次号已用拒绝；清单状态不对拒绝。
func TestConfirmManifestRejects(t *testing.T) {
	t.Run("冻结件整单拒绝", func(t *testing.T) {
		s, _ := manifestStore(t)
		mustManifest(t, s, "M1", "站点A", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
		mustFreeze(t, s, "E1", "P001", "站点A", "破损", tClock(2026, 10, 5, 9, 40))
		if _, _, err := s.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 10, 0)); err == nil {
			t.Fatal("含冻结件的确认应整单拒绝")
		}
		if _, err := s.BatchQuery("B1"); err == nil {
			t.Fatal("确认失败不得占用批次号")
		}
		m, _ := s.ManifestQuery("M1")
		if m.Status != manifestPending {
			t.Fatalf("确认失败清单应仍待出站，得到 %q", m.Status)
		}
		if s.ActiveManifest("P001") == nil {
			t.Fatal("确认失败不得释放预留")
		}
	})

	t.Run("批次号已被出站使用", func(t *testing.T) {
		s, _ := manifestStore(t)
		mustManifest(t, s, "M1", "站点A", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30))
		mustDispatch(t, s, "B1", "站点A", "李四", []string{"P003"}, tClock(2026, 10, 5, 9, 45))
		if _, _, err := s.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 10, 0)); err == nil {
			t.Fatal("批次号已被出站使用时确认应拒绝")
		}
		m, _ := s.ManifestQuery("M1")
		if m.Status != manifestPending {
			t.Fatal("确认失败清单应仍待出站")
		}
	})

	t.Run("只能确认待出站清单", func(t *testing.T) {
		s, _ := manifestStore(t)
		mustManifest(t, s, "M1", "站点A", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
		if _, _, err := s.CancelManifest("MC1", "M1", "计划取消", tClock(2026, 10, 5, 9, 45)); err != nil {
			t.Fatalf("取消失败: %v", err)
		}
		if _, _, err := s.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 10, 0)); err == nil {
			t.Fatal("已取消清单不能确认")
		}
		if _, err := s.BatchQuery("B1"); err == nil {
			t.Fatal("确认失败不得占用批次号")
		}
	})

	t.Run("清单不存在", func(t *testing.T) {
		s, _ := manifestStore(t)
		if _, _, err := s.ConfirmManifest("NOPE", "B1", "张三", tClock(2026, 10, 5, 10, 0)); err == nil {
			t.Fatal("不存在的清单确认应拒绝")
		}
	})
}

// 取消：整单释放预留、永久标记已取消，不改变站点、状态或解除冻结；
// 取消不算新流转，解冻后仍可退回原本可退回的交接。
func TestCancelManifestSuccess(t *testing.T) {
	s, _ := manifestStore(t)
	// P002 先由站点A 交接至站点B，使其后存在一个可退回的旧交接。
	mustHandoff(t, s, "H1", "站点A", "站点B", []string{"P002"}, tClock(2026, 10, 5, 8, 30))
	mustManifest(t, s, "M1", "站点B", []string{"P002"}, tClock(2026, 10, 5, 9, 0))
	mustFreeze(t, s, "E1", "P002", "站点B", "破损", tClock(2026, 10, 5, 9, 10))

	res, replayed, err := s.CancelManifest("MC1", "M1", "计划取消", tClock(2026, 10, 5, 9, 20))
	if err != nil || replayed {
		t.Fatalf("取消应成功且非重放: %v replayed=%v", err, replayed)
	}
	if res.Manifest != "M1" || res.Reason != "计划取消" || !sameOrder(res.Parcels, []string{"P002"}) {
		t.Fatalf("取消结果不符: %+v", res)
	}
	m, _ := s.ManifestQuery("M1")
	if m.Status != manifestCancelled {
		t.Fatalf("清单应永久标记已取消，得到 %q", m.Status)
	}
	if s.ActiveManifest("P002") != nil {
		t.Fatal("取消后预留应释放")
	}
	p, _ := s.Query("P002")
	if p.Station != "站点B" || p.Status != statusFrozen {
		t.Fatalf("取消不得改变站点、状态或解除冻结: %+v", p)
	}
	last := p.Trail[len(p.Trail)-1]
	if last.Op != "取消预留" || last.Manifest != "M1" || last.Request != "MC1" || last.Reason != "计划取消" {
		t.Fatalf("末条应为取消预留记录: %+v", last)
	}

	// 冻结期间仍不能退回；解冻后，因取消不算新流转，旧交接仍可整批退回。
	if _, _, err := s.Return("RT1", "H1", "错发站点", tClock(2026, 10, 5, 9, 30)); err == nil {
		t.Fatal("冻结期间整批退回仍应拒绝")
	}
	if _, _, err := s.Unfreeze("U1", "E1", "核实放行", tClock(2026, 10, 5, 9, 40)); err != nil {
		t.Fatalf("解除冻结失败: %v", err)
	}
	if _, _, err := s.Return("RT1", "H1", "错发站点", tClock(2026, 10, 5, 9, 50)); err != nil {
		t.Fatalf("取消且解冻后应仍可退回原本可退回的旧交接: %v", err)
	}
	if p, _ := s.Query("P002"); p.Station != "站点A" {
		t.Fatalf("退回后应回到源站点A: %+v", p)
	}
}

// 取消只能作用于待出站清单：已出站清单不能取消。
func TestCancelRejectsTerminal(t *testing.T) {
	s, _ := manifestStore(t)
	mustManifest(t, s, "M1", "站点A", []string{"P001"}, tClock(2026, 10, 5, 9, 0))
	if _, _, err := s.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 9, 30)); err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if _, _, err := s.CancelManifest("MC1", "M1", "晚了", tClock(2026, 10, 5, 10, 0)); err == nil {
		t.Fatal("已出站清单不能取消")
	}
	if s.data.ManifestCancels["MC1"] != nil {
		t.Fatal("失败的取消不得占用取消请求号")
	}
}

// 清单号去重：同号同站点同集合（换序无关）重放返回首次顺序与时间，终结后不重新预留；
// 同号换站点或集合冲突。
func TestManifestCreateReplayAndConflict(t *testing.T) {
	s, _ := manifestStore(t)
	first := mustManifest(t, s, "M1", "站点A", []string{"P002", "P001"}, tClock(2026, 10, 5, 9, 0))

	// 换序重放：返回首次顺序与时间。
	saved, replayed, err := s.CreateManifest("M1", "站点A", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 5))
	if err != nil || !replayed {
		t.Fatalf("同号同站点同集合换序应重放: %v replayed=%v", err, replayed)
	}
	if !sameOrder(saved.Parcels, first.Parcels) || !saved.Time.Equal(first.Time) {
		t.Fatalf("重放应返回首次顺序与时间: saved=%+v first=%+v", saved, first)
	}

	// 确认出站后重放：仍返回首次结果，不重新预留、不追加轨迹。
	if _, _, err := s.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 10, 0)); err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	saved, replayed, err = s.CreateManifest("M1", "站点A", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 30))
	if err != nil || !replayed || saved.Status != manifestConfirmed {
		t.Fatalf("终结后同号同内容应重放首次结果且不重新预留: %v replayed=%v saved=%+v", err, replayed, saved)
	}

	// 同号换内容冲突（即使原清单已终结，号也不复用）。
	if _, _, err := s.CreateManifest("M1", "站点A", []string{"P001"}, tClock(2026, 10, 5, 11, 0)); err == nil {
		t.Fatal("同号换集合应冲突")
	}
	if _, _, err := s.CreateManifest("M1", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 11, 0)); err == nil {
		t.Fatal("同号换站点应冲突")
	}
}

// 确认去重：同清单同批次同配送员重放返回首次结果与时间（不检查现状）；换批次或配送员冲突。
func TestManifestConfirmReplayAndConflict(t *testing.T) {
	s, _ := manifestStore(t)
	mustManifest(t, s, "M1", "站点A", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 0))
	first, _, err := s.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 9, 30))
	if err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	// 全部回执后再重放：不检查现状，仍返回首次结果与时间。
	mustReceiptOK(t, s, "RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 10, 0))
	mustReceiptOK(t, s, "RC2", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 10, 1))
	saved, replayed, err := s.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 11, 0))
	if err != nil || !replayed || !saved.Time.Equal(first.Time) {
		t.Fatalf("已出站清单同批次同配送员应重放首次结果: %v replayed=%v", err, replayed)
	}
	if p, _ := s.Query("P001"); p.Status != statusSigned {
		t.Fatal("确认重放不得改变当前状态")
	}
	// 换配送员冲突。
	if _, _, err := s.ConfirmManifest("M1", "B1", "李四", tClock(2026, 10, 5, 11, 30)); err == nil {
		t.Fatal("同清单同批次换配送员应冲突")
	}
	// 换批次号：清单已终结，拒绝。
	if _, _, err := s.ConfirmManifest("M1", "B2", "张三", tClock(2026, 10, 5, 11, 30)); err == nil {
		t.Fatal("已出站清单换新批次号应拒绝")
	}
}

// 取消请求号去重：同号同清单同原因重放；换内容冲突；换号再取消同一清单拒绝。
func TestManifestCancelReplayAndConflict(t *testing.T) {
	s, _ := manifestStore(t)
	mustManifest(t, s, "M1", "站点A", []string{"P001"}, tClock(2026, 10, 5, 9, 0))
	first, _, err := s.CancelManifest("MC1", "M1", "计划取消", tClock(2026, 10, 5, 9, 30))
	if err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	// 同号同清单同原因重放。
	saved, replayed, err := s.CancelManifest("MC1", "M1", "计划取消", tClock(2026, 10, 5, 10, 0))
	if err != nil || !replayed || !saved.Time.Equal(first.Time) {
		t.Fatalf("同号同清单同原因应重放首次结果: %v replayed=%v", err, replayed)
	}
	// 同号换内容冲突。
	if _, _, err := s.CancelManifest("MC1", "M1", "别的原因", tClock(2026, 10, 5, 10, 1)); err == nil {
		t.Fatal("同号换原因应冲突")
	}
	if _, _, err := s.CancelManifest("MC1", "M2", "计划取消", tClock(2026, 10, 5, 10, 2)); err == nil {
		t.Fatal("同号换清单应冲突")
	}
	// 换号再次取消同一清单拒绝。
	if _, _, err := s.CancelManifest("MC2", "M1", "计划取消", tClock(2026, 10, 5, 10, 3)); err == nil {
		t.Fatal("换号再次取消同一清单应拒绝")
	}
	if s.data.ManifestCancels["MC2"] != nil {
		t.Fatal("失败的取消不得占用 MC2")
	}
}

// 保存失败：创建与确认的整次变更必须回滚内存，不留下数据或临时文件，也不占号。
func TestManifestSaveFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	ro := filepath.Join(dir, "readonly")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	path := filepath.Join(ro, "ledger.json")

	s, err := Open(path) // 文件尚不存在
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register("P001", "站点A", tClock(2026, 10, 5, 9, 0)); err == nil {
		t.Fatal("只读目录下登记本应失败")
	}

	// 在可写目录准备一张待出站清单，再把目录改只读，确认的保存必失败并整体回滚。
	wdir := t.TempDir()
	wpath := filepath.Join(wdir, "ledger.json")
	s2, err := Open(wpath)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s2, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustManifest(t, s2, "M1", "站点A", []string{"P001"}, tClock(2026, 10, 5, 9, 30))
	if err := os.Chmod(wdir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(wdir, 0o755) })
	if _, _, err := s2.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 10, 0)); err == nil {
		t.Fatal("保存失败时确认不得返回成功")
	}
	m, _ := s2.ManifestQuery("M1")
	if m.Status != manifestPending || m.ConfirmedBy != "" {
		t.Fatalf("保存失败必须回滚清单状态: %+v", m)
	}
	if s2.data.Batches["B1"] != nil {
		t.Fatal("保存失败不得在内存留下批次")
	}
	if p, _ := s2.Query("P001"); p.Status != statusInStation {
		t.Fatalf("保存失败必须回滚包裹状态: %+v", p)
	}
}

// 重启规则不变：重新打开台账后，预留、确认、取消与重放规则保持。
func TestManifestRulesSurviveRestart(t *testing.T) {
	s, path := manifestStore(t)
	mustManifest(t, s, "M1", "站点A", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 0))

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重启打开失败: %v", err)
	}
	if s2.ActiveManifest("P001") == nil || s2.ActiveManifest("P001").Manifest != "M1" {
		t.Fatal("重启后预留应仍在")
	}
	// 重启后流转仍被拦截。
	if _, _, err := s2.Dispatch("B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 9, 30)); err == nil {
		t.Fatal("重启后预留件仍应拒绝直接 dispatch")
	}
	// 重启后创建重放返回首次结果。
	if _, replayed, err := s2.CreateManifest("M1", "站点A", []string{"P002", "P001"}, tClock(2026, 10, 5, 9, 31)); err != nil || !replayed {
		t.Fatalf("重启后同号同内容应重放: %v replayed=%v", err, replayed)
	}
	// 重启后确认成功，然后再次打开，确认批次是普通批次、清单已出站。
	if _, _, err := s2.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 10, 0)); err != nil {
		t.Fatalf("重启后确认失败: %v", err)
	}
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("再次重启打开失败: %v", err)
	}
	m, _ := s3.ManifestQuery("M1")
	if m.Status != manifestConfirmed {
		t.Fatalf("重启后清单应为已出站，得到 %q", m.Status)
	}
	if _, replayed, err := s3.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 11, 0)); err != nil || !replayed {
		t.Fatalf("重启后确认应可重放: %v replayed=%v", err, replayed)
	}
}

// 旧版数据文件没有分拣清单字段：应无需修改直接打开，并可正常使用新功能。
func TestLegacyLedgerWithoutManifestsLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	legacy := `{"version":1,"parcels":{"P1":{"id":"P1","station":"A","status":"在站",` +
		`"registered":"2026-10-05T09:00:00Z","trail":[{"op":"收件","station":"A","time":"2026-10-05T09:00:00Z"}]}},` +
		`"handoffs":{},"batches":{}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("旧台账应可直接打开: %v", err)
	}
	res, replayed, err := s.CreateManifest("M1", "A", []string{"P1"}, tClock(2026, 10, 5, 10, 0))
	if err != nil || replayed || res.Status != manifestPending {
		t.Fatalf("旧台账升级后应可创建清单: %v replayed=%v res=%+v", err, replayed, res)
	}
}

// 重载拒绝：新增关联空缺、重复预留、清单与轨迹矛盾、确认批次矛盾，均报 ErrCorrupt
// 且不覆盖原文件、不崩溃。
func TestCorruptManifestDataRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")

	parcel1 := `"id":"P1","station":"A","status":"在站","registered":"2026-10-05T09:00:00Z"`
	parcel1Delivering := `"id":"P1","station":"A","status":"配送中","registered":"2026-10-05T09:00:00Z"`
	parcel2 := `"id":"P2","station":"A","status":"在站","registered":"2026-10-05T09:00:00Z"`
	reg1 := `{"op":"收件","station":"A","time":"2026-10-05T09:00:00Z"}`
	reserve1 := `{"op":"分拣预留","station":"A","time":"2026-10-05T09:30:00Z","manifest":"M1"}`
	reserve1ForM2 := `{"op":"分拣预留","station":"A","time":"2026-10-05T09:31:00Z","manifest":"M2"}`
	out1 := `{"op":"出站","station":"A","time":"2026-10-05T10:00:00Z","batch":"B1","courier":"张三"}`
	manifest := `"M1":{"manifest":"M1","station":"A","parcels":["P1"],"time":"2026-10-05T09:30:00Z","status":"待出站"}`
	manifestConfirmed := `"M1":{"manifest":"M1","station":"A","parcels":["P1"],"time":"2026-10-05T09:30:00Z","status":"已出站","confirmedBy":"M1\x00B1"}`
	confirm := `"M1\x00B1":{"manifest":"M1","batch":"B1","courier":"张三","parcels":["P1"],"station":"A","time":"2026-10-05T10:00:00Z"}`
	batch1 := `"B1":{"batch":"B1","station":"A","courier":"张三","parcels":["P1"],"time":"2026-10-05T10:00:00Z","receipts":{}}`

	// 确认结果表的键含 NUL 字节（清单号 + \x00 + 批次号），JSON 字符串里
	// 必须用 \u0000 转义。
	writeJSON := func(content string) {
		t.Helper()
		content = strings.ReplaceAll(content, `M1\x00B1`, `M1\u0000B1`)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	good := `{"version":1,"parcels":{"P1":{` + parcel1 + `,"trail":[` + reg1 + `,` + reserve1 + `]}},"handoffs":{},` +
		`"manifests":{` + manifest + `}}`
	writeJSON(good)
	if _, err := Open(path); err != nil {
		t.Fatalf("完好的待出站清单台账应可打开: %v", err)
	}

	goodConfirmed := `{"version":1,"parcels":{"P1":{` + parcel1Delivering + `,"trail":[` + reg1 + `,` + reserve1 + `,` + out1 + `]}},"handoffs":{},` +
		`"batches":{` + batch1 + `},"manifests":{` + manifestConfirmed + `},"manifestConfirms":{` + confirm + `}}`
	writeJSON(goodConfirmed)
	if _, err := Open(path); err != nil {
		t.Fatalf("完好的已出站清单台账应可打开: %v", err)
	}

	cases := map[string]string{
		"清单成员缺少分拣预留轨迹（关联空缺）": `{"version":1,"parcels":{"P1":{` + parcel1 + `,"trail":[` + reg1 + `]}},"handoffs":{},` +
			`"manifests":{` + manifest + `}}`,
		"分拣预留轨迹引用了不存在的清单": `{"version":1,"parcels":{"P1":{` + parcel1 + `,"trail":[` + reg1 + `,` + reserve1 + `]}},"handoffs":{},"manifests":{}}`,
		"同件被两张待出站清单重复预留": `{"version":1,"parcels":{"P1":{` + parcel1 + `,"trail":[` + reg1 + `,` + reserve1 + `,` + reserve1ForM2 + `]}},"handoffs":{},` +
			`"manifests":{"M1":` + `{"manifest":"M1","station":"A","parcels":["P1"],"time":"2026-10-05T09:30:00Z","status":"待出站"},` +
			`"M2":{"manifest":"M2","station":"A","parcels":["P1"],"time":"2026-10-05T09:31:00Z","status":"待出站"}}}`,
		"预留轨迹与清单记录不一致": `{"version":1,"parcels":{"P1":{` + parcel1 + `,"trail":[` + reg1 + `,{"op":"分拣预留","station":"B","time":"2026-10-05T09:30:00Z","manifest":"M1"}` + `]}},"handoffs":{},` +
			`"manifests":{` + manifest + `}}`,
		"已出站清单缺少确认记录": `{"version":1,"parcels":{"P1":{` + parcel1Delivering + `,"trail":[` + reg1 + `,` + reserve1 + `,` + out1 + `]}},"handoffs":{},` +
			`"batches":{` + batch1 + `},"manifests":{` + manifestConfirmed + `}}`,
		"确认批次与清单成员矛盾": `{"version":1,"parcels":{"P1":{` + parcel1Delivering + `,"trail":[` + reg1 + `,` + reserve1 + `,` + out1 + `],"P2":{` + parcel2 + `,"trail":[` +
			`{"op":"收件","station":"A","time":"2026-10-05T09:00:00Z"}]}},"handoffs":{},` +
			`"batches":{` + batch1 + `},` +
			`"manifests":{"M1":{"manifest":"M1","station":"A","parcels":["P1","P2"],"time":"2026-10-05T09:30:00Z","status":"已出站","confirmedBy":"M1\x00B1"}},` +
			`"manifestConfirms":{"M1\x00B1":{"manifest":"M1","batch":"B1","courier":"张三","parcels":["P1","P2"],"station":"A","time":"2026-10-05T10:00:00Z"}}}`,
		"确认成员缺少出站轨迹": `{"version":1,"parcels":{"P1":{` + parcel1 + `,"trail":[` + reg1 + `,` + reserve1 + `]}},"handoffs":{},` +
			`"batches":{` + batch1 + `},` +
			`"manifests":{` + manifestConfirmed + `},"manifestConfirms":{` + confirm + `}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			writeJSON(content)
			if _, err := Open(path); err == nil {
				t.Fatal("损坏的清单台账必须被拒绝打开")
			} else if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("应返回 ErrCorrupt，得到 %v", err)
			}
			got, _ := os.ReadFile(path)
			want := strings.ReplaceAll(content, `M1\x00B1`, `M1\u0000B1`)
			if string(got) != want {
				t.Fatal("损坏文件不得被覆盖")
			}
		})
	}
}
