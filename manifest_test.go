package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustCreateManifest(t *testing.T, s *Store, manifest, station string, parcels []string, now time.Time) *ManifestResult {
	t.Helper()
	res, replayed, err := s.CreateManifest(manifest, station, parcels, now)
	if err != nil || replayed {
		t.Fatalf("CreateManifest(%q) 意外失败: %v replayed=%v", manifest, err, replayed)
	}
	return res
}

func mustConfirmManifest(t *testing.T, s *Store, manifest, batch, courier string, now time.Time) *BatchResult {
	t.Helper()
	res, replayed, err := s.ConfirmManifest(manifest, batch, courier, now)
	if err != nil || replayed {
		t.Fatalf("ConfirmManifest(%q) 意外失败: %v replayed=%v", manifest, err, replayed)
	}
	return res
}

func mustCancelManifest(t *testing.T, s *Store, request, manifest, reason string, now time.Time) *ManifestCancelResult {
	t.Helper()
	res, replayed, err := s.CancelManifest(request, manifest, reason, now)
	if err != nil || replayed {
		t.Fatalf("CancelManifest(%q) 意外失败: %v replayed=%v", request, err, replayed)
	}
	return res
}

// 准备三件在站点A 在站的包裹 P001、P002、P003。
func setupManifestBase(t *testing.T, s *Store) {
	t.Helper()
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
}

func TestManifestCreateSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	setupManifestBase(t, s)

	createTime := tClock(2026, 10, 5, 10, 0)
	res := mustCreateManifest(t, s, "M1", "站点A", []string{"P002", "P001"}, createTime)
	if res.Manifest != "M1" || res.Station != "站点A" || !res.Time.Equal(createTime) {
		t.Fatalf("清单结果不符: %+v", res)
	}
	if !sameOrder(res.Parcels, []string{"P002", "P001"}) {
		t.Fatalf("成员应按首次提交顺序保存: %v", res.Parcels)
	}
	if res.ConfirmedBatch != "" || res.CancelledBy != "" {
		t.Fatalf("新清单不应标记已出站或已取消: %+v", res)
	}
	for _, id := range []string{"P001", "P002"} {
		p, err := s.Query(id)
		if err != nil {
			t.Fatalf("Query(%q) 失败: %v", id, err)
		}
		// 预留不改变站点与状态。
		if p.Station != "站点A" || p.Status != statusInStation {
			t.Fatalf("预留后包裹 %q 站点、状态不应改变: %+v", id, p)
		}
		last := p.Trail[len(p.Trail)-1]
		if last.Op != "清单预留" || last.Manifest != "M1" || last.Station != "站点A" || !last.Time.Equal(createTime) {
			t.Fatalf("包裹 %q 缺少含清单、站点和时间的清单预留轨迹: %+v", id, last)
		}
		m := s.ReservedManifest(id)
		if m == nil || m.Manifest != "M1" {
			t.Fatalf("包裹 %q 应被清单 M1 预留", id)
		}
	}
	if s.ReservedManifest("P003") != nil {
		t.Fatal("P003 不在清单内，不应被预留")
	}
}

func TestManifestCreateValidationFailures(t *testing.T) {
	s, _ := openTempStore(t)
	setupManifestBase(t, s)
	// P004 未登记；P005 在站点B；P006 已随批次出站。
	mustRegister(t, s, "P005", "站点B", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s, "P006", "站点A", tClock(2026, 10, 5, 9, 0))
	if _, _, err := s.Dispatch("B0", "站点A", "张三", []string{"P006"}, tClock(2026, 10, 5, 9, 30)); err != nil {
		t.Fatalf("Dispatch 意外失败: %v", err)
	}
	// P007 已冻结。
	mustRegister(t, s, "P007", "站点A", tClock(2026, 10, 5, 9, 0))
	if _, _, err := s.Freeze("E1", "P007", "站点A", "外包装破损", tClock(2026, 10, 5, 9, 40)); err != nil {
		t.Fatalf("Freeze 意外失败: %v", err)
	}

	now := tClock(2026, 10, 5, 10, 0)
	for _, tc := range []struct {
		name    string
		station string
		parcels []string
	}{
		{"未登记", "站点A", []string{"P001", "P004"}},
		{"不在本站", "站点A", []string{"P001", "P005"}},
		{"非在站", "站点A", []string{"P001", "P006"}},
		{"已冻结", "站点A", []string{"P001", "P007"}},
	} {
		if _, _, err := s.CreateManifest("M1", tc.station, tc.parcels, now); err == nil {
			t.Fatalf("%s：应整单拒绝", tc.name)
		}
		if _, ok := s.data.Manifests["M1"]; ok {
			t.Fatalf("%s：失败的首次创建不得占用清单号", tc.name)
		}
	}
	// 整单拒绝不影响其他包裹：P001 无清单预留轨迹。
	p, _ := s.Query("P001")
	if len(p.Trail) != 1 {
		t.Fatalf("整单拒绝不应追加轨迹，得到 %d 条", len(p.Trail))
	}

	// 首次成功创建后，成员不能加入另一清单。
	mustCreateManifest(t, s, "M1", "站点A", []string{"P001"}, now)
	if _, _, err := s.CreateManifest("M2", "站点A", []string{"P001", "P003"}, now); err == nil ||
		!strings.Contains(err.Error(), "预留") {
		t.Fatalf("已被预留的包裹不能加入另一清单: %v", err)
	}
	if _, ok := s.data.Manifests["M2"]; ok {
		t.Fatal("失败的首次创建不得占用清单号 M2")
	}
}

func TestManifestReservedBlocking(t *testing.T) {
	s, _ := openTempStore(t)
	setupManifestBase(t, s)
	// P001 先经交接 H1 到站点B，再随 P008 一起在站点B 被预留。
	mustRegister(t, s, "P008", "站点A", tClock(2026, 10, 5, 9, 0))
	if _, _, err := s.Handoff("H1", "站点A", "站点B", []string{"P001", "P008"}, tClock(2026, 10, 5, 9, 30)); err != nil {
		t.Fatalf("Handoff 意外失败: %v", err)
	}
	mustCreateManifest(t, s, "M1", "站点B", []string{"P001", "P008"}, tClock(2026, 10, 5, 10, 0))
	now := tClock(2026, 10, 5, 11, 0)

	// 待出站成员不能首次交接、发运、直接 dispatch，涉及它的批量操作整批拒绝。
	if _, _, err := s.Handoff("H2", "站点B", "站点C", []string{"P001"}, now); err == nil {
		t.Fatal("预留件不能首次交接")
	}
	if _, _, err := s.Ship("S1", "站点B", "站点C", []string{"P001"}, now); err == nil {
		t.Fatal("预留件不能发运")
	}
	if _, _, err := s.Dispatch("B1", "站点B", "张三", []string{"P001"}, now); err == nil {
		t.Fatal("预留件不能直接 dispatch")
	}
	// 涉及预留件的整批退回拒绝（P001、P008 的最后流转是交接 H1，本可退回）。
	if _, _, err := s.Return("RT1", "H1", "错发站点", now); err == nil ||
		!strings.Contains(err.Error(), "预留") {
		t.Fatalf("涉及预留件的整批退回应拒绝: %v", err)
	}
	// 批量操作整批拒绝：混合预留件与自由件的交接整批不变。
	mustRegister(t, s, "P009", "站点B", tClock(2026, 10, 5, 9, 0))
	if _, _, err := s.Handoff("H3", "站点B", "站点C", []string{"P009", "P001"}, now); err == nil {
		t.Fatal("混合批次涉及预留件应整批拒绝")
	}
	p9, _ := s.Query("P009")
	if p9.Station != "站点B" || len(p9.Trail) != 1 {
		t.Fatalf("整批拒绝不应影响其他包裹: %+v", p9)
	}

	// 允许冻结与解除，预留保留。
	if _, _, err := s.Freeze("E1", "P001", "站点B", "抽检", now); err != nil {
		t.Fatalf("预留件应可冻结: %v", err)
	}
	if m := s.ReservedManifest("P001"); m == nil || m.Manifest != "M1" {
		t.Fatal("冻结不应解除预留")
	}
	if _, _, err := s.Unfreeze("U1", "E1", "已核实放行", tClock(2026, 10, 5, 11, 30)); err != nil {
		t.Fatalf("预留件应可解除冻结: %v", err)
	}
	if m := s.ReservedManifest("P001"); m == nil || m.Manifest != "M1" {
		t.Fatal("解除冻结不应解除预留")
	}
}

func TestManifestConfirmSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	setupManifestBase(t, s)
	createTime := tClock(2026, 10, 5, 10, 0)
	mustCreateManifest(t, s, "M1", "站点A", []string{"P002", "P001"}, createTime)

	confirmTime := tClock(2026, 10, 5, 11, 0)
	b := mustConfirmManifest(t, s, "M1", "B1", "张三", confirmTime)
	if b.Batch != "B1" || b.Station != "站点A" || b.Courier != "张三" || !b.Time.Equal(confirmTime) {
		t.Fatalf("确认创建的批次不符: %+v", b)
	}
	// 按清单原顺序创建普通配送批次。
	if !sameOrder(b.Parcels, []string{"P002", "P001"}) {
		t.Fatalf("批次成员应按清单原顺序: %v", b.Parcels)
	}
	if b.RelayedFrom != "" || b.RelayRequest != "" {
		t.Fatalf("确认创建的应是普通出站批次: %+v", b)
	}
	m, _ := s.ManifestQuery("M1")
	if m.ConfirmedBatch != "B1" || m.CancelledBy != "" {
		t.Fatalf("清单应永久标记已出站: %+v", m)
	}
	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		if p.Status != statusDelivering || p.Station != "站点A" {
			t.Fatalf("确认后包裹 %q 应转配送中、站点不变: %+v", id, p)
		}
		last := p.Trail[len(p.Trail)-1]
		if last.Op != "出站" || last.Batch != "B1" || last.Courier != "张三" || !last.Time.Equal(confirmTime) {
			t.Fatalf("包裹 %q 缺少出站轨迹: %+v", id, last)
		}
		if s.ReservedManifest(id) != nil {
			t.Fatalf("确认后包裹 %q 的预留应释放", id)
		}
	}
	// dispatch 可按该批次原内容重放。
	if _, replayed, err := s.Dispatch("B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 12, 0)); err != nil || !replayed {
		t.Fatalf("dispatch 应能按确认批次原内容重放: %v replayed=%v", err, replayed)
	}
	// 后续配送作业照常：逐件回执。
	if _, _, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 13, 0)); err != nil {
		t.Fatalf("确认后的批次应可正常回执: %v", err)
	}
}

func TestManifestConfirmFailures(t *testing.T) {
	s, _ := openTempStore(t)
	setupManifestBase(t, s)
	now := tClock(2026, 10, 5, 10, 0)

	// 清单不存在。
	if _, _, err := s.ConfirmManifest("NOPE", "B1", "张三", now); err == nil {
		t.Fatal("不存在的清单不能确认")
	}
	mustCreateManifest(t, s, "M1", "站点A", []string{"P001", "P002"}, now)

	// 批次号已被出站使用。
	if _, _, err := s.Dispatch("B0", "站点A", "张三", []string{"P003"}, tClock(2026, 10, 5, 10, 30)); err != nil {
		t.Fatalf("Dispatch 意外失败: %v", err)
	}
	if _, _, err := s.ConfirmManifest("M1", "B0", "张三", now); err == nil ||
		!strings.Contains(err.Error(), "已被出站或续接使用") {
		t.Fatalf("批次号已被使用应拒绝: %v", err)
	}

	// 冻结件导致整单拒绝。
	if _, _, err := s.Freeze("E1", "P001", "站点A", "抽检", tClock(2026, 10, 5, 10, 40)); err != nil {
		t.Fatalf("Freeze 意外失败: %v", err)
	}
	if _, _, err := s.ConfirmManifest("M1", "B1", "张三", now); err == nil ||
		!strings.Contains(err.Error(), "冻结") {
		t.Fatalf("冻结件应导致整单拒绝: %v", err)
	}
	if _, ok := s.data.Batches["B1"]; ok {
		t.Fatal("整单拒绝不得创建批次")
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusInStation || len(p2.Trail) != 2 {
		t.Fatalf("整单拒绝不应影响其他成员: %+v", p2)
	}
	// 解除冻结后可确认。
	if _, _, err := s.Unfreeze("U1", "E1", "放行", tClock(2026, 10, 5, 10, 50)); err != nil {
		t.Fatalf("Unfreeze 意外失败: %v", err)
	}
	mustConfirmManifest(t, s, "M1", "B1", "张三", tClock(2026, 10, 5, 11, 0))

	// 已取消的清单不能确认（确认与取消竞争至多一方成功）。
	mustRegister(t, s, "P010", "站点A", tClock(2026, 10, 5, 9, 0))
	mustCreateManifest(t, s, "M3", "站点A", []string{"P010"}, tClock(2026, 10, 5, 11, 40))
	mustCancelManifest(t, s, "MC1", "M3", "计划调整", tClock(2026, 10, 5, 12, 0))
	if _, _, err := s.ConfirmManifest("M3", "B9", "张三", tClock(2026, 10, 5, 12, 30)); err == nil ||
		!strings.Contains(err.Error(), "已取消") {
		t.Fatalf("已取消清单不能确认: %v", err)
	}
}

func TestManifestCancelFlow(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	if _, _, err := s.Handoff("H1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 9, 30)); err != nil {
		t.Fatalf("Handoff 意外失败: %v", err)
	}
	mustCreateManifest(t, s, "M1", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))

	cancelTime := tClock(2026, 10, 5, 11, 0)
	res := mustCancelManifest(t, s, "MC1", "M1", "配送计划调整", cancelTime)
	if res.Request != "MC1" || res.Manifest != "M1" || res.Station != "站点B" ||
		res.Reason != "配送计划调整" || !res.Time.Equal(cancelTime) {
		t.Fatalf("取消结果不符: %+v", res)
	}
	if !sameOrder(res.Parcels, []string{"P001", "P002"}) {
		t.Fatalf("取消结果成员应按清单保存顺序: %v", res.Parcels)
	}
	m, _ := s.ManifestQuery("M1")
	if m.CancelledBy != "MC1" || m.ConfirmedBatch != "" {
		t.Fatalf("清单应永久标记已取消: %+v", m)
	}
	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		// 取消不改变站点、状态。
		if p.Station != "站点B" || p.Status != statusInStation {
			t.Fatalf("取消后包裹 %q 站点、状态不应改变: %+v", id, p)
		}
		last := p.Trail[len(p.Trail)-1]
		if last.Op != "清单取消" || last.Manifest != "M1" || last.Request != "MC1" ||
			last.Reason != "配送计划调整" || last.Station != "站点B" || !last.Time.Equal(cancelTime) {
			t.Fatalf("包裹 %q 缺少清单取消轨迹: %+v", id, last)
		}
		if s.ReservedManifest(id) != nil {
			t.Fatalf("取消后包裹 %q 的预留应释放", id)
		}
	}
	// 取消不算新流转：无其他新流转时，原本可退回的交接 H1 仍可整批退回。
	if _, _, err := s.Return("RT1", "H1", "错发站点", tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatalf("取消后无新流转，原本可退回的交接应仍可退回: %v", err)
	}
}

func TestManifestCancelKeepsFreeze(t *testing.T) {
	s, _ := openTempStore(t)
	setupManifestBase(t, s)
	mustCreateManifest(t, s, "M1", "站点A", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	if _, _, err := s.Freeze("E1", "P001", "站点A", "抽检", tClock(2026, 10, 5, 10, 30)); err != nil {
		t.Fatalf("Freeze 意外失败: %v", err)
	}
	// 冻结件不阻止取消；取消不改变状态、不解除冻结。
	mustCancelManifest(t, s, "MC1", "M1", "计划调整", tClock(2026, 10, 5, 11, 0))
	p1, _ := s.Query("P001")
	if p1.Status != statusFrozen {
		t.Fatalf("取消不得解除冻结，得到状态 %q", p1.Status)
	}
	if f := s.ActiveFreeze("P001"); f == nil || f.Incident != "E1" {
		t.Fatal("取消后异常单应仍未解除")
	}
}

func TestManifestReplay(t *testing.T) {
	s, _ := openTempStore(t)
	setupManifestBase(t, s)
	createTime := tClock(2026, 10, 5, 10, 0)
	mustCreateManifest(t, s, "M1", "站点A", []string{"P001", "P002"}, createTime)

	// 同号同站点同集合（换序）重放：返回首次顺序和时间，不追加轨迹、不改写文件。
	res, replayed, err := s.CreateManifest("M1", "站点A", []string{"P002", "P001"}, tClock(2026, 10, 5, 11, 0))
	if err != nil || !replayed {
		t.Fatalf("同内容重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	if !sameOrder(res.Parcels, []string{"P001", "P002"}) || !res.Time.Equal(createTime) {
		t.Fatalf("重放应返回首次顺序与时间: %+v", res)
	}
	p, _ := s.Query("P001")
	if len(p.Trail) != 2 {
		t.Fatalf("重放不应追加轨迹，得到 %d 条", len(p.Trail))
	}
	// 换内容冲突：换站点、换集合均拒绝。
	if _, _, err := s.CreateManifest("M1", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 11, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("换站点应报冲突: %v", err)
	}
	if _, _, err := s.CreateManifest("M1", "站点A", []string{"P001"}, tClock(2026, 10, 5, 11, 0)); err == nil ||
		!strings.Contains(err.Error(), "冲突") {
		t.Fatalf("换集合应报冲突: %v", err)
	}

	// 确认后重放：按相同批次和配送员确认返回首次结果与时间。
	confirmTime := tClock(2026, 10, 5, 12, 0)
	mustConfirmManifest(t, s, "M1", "B1", "张三", confirmTime)
	b, replayed, err := s.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 13, 0))
	if err != nil || !replayed {
		t.Fatalf("确认重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	if b.Batch != "B1" || !b.Time.Equal(confirmTime) {
		t.Fatalf("确认重放应返回首次批次与时间: %+v", b)
	}
	// 换批次号或配送员确认冲突。
	if _, _, err := s.ConfirmManifest("M1", "B2", "张三", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("换批次号确认应报冲突")
	}
	if _, _, err := s.ConfirmManifest("M1", "B1", "李四", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("换配送员确认应报冲突")
	}
	// 终结后同号同内容创建重放仍返回首次结果，不重新预留。
	res, replayed, err = s.CreateManifest("M1", "站点A", []string{"P002", "P001"}, tClock(2026, 10, 5, 14, 0))
	if err != nil || !replayed {
		t.Fatalf("终结后同内容创建重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	if !res.Time.Equal(createTime) {
		t.Fatalf("终结后重放应返回首次时间: %+v", res)
	}

	// 取消重放：同号同清单同原因返回首次结果与时间。
	mustCreateManifest(t, s, "M2", "站点A", []string{"P003"}, tClock(2026, 10, 5, 15, 0))
	cancelTime := tClock(2026, 10, 5, 16, 0)
	mustCancelManifest(t, s, "MC1", "M2", "计划调整", cancelTime)
	c, replayed, err := s.CancelManifest("MC1", "M2", "计划调整", tClock(2026, 10, 5, 17, 0))
	if err != nil || !replayed {
		t.Fatalf("取消重放应返回首次结果: %v replayed=%v", err, replayed)
	}
	if !c.Time.Equal(cancelTime) || c.Manifest != "M2" {
		t.Fatalf("取消重放应返回首次结果与时间: %+v", c)
	}
	p3, _ := s.Query("P003")
	trailLen := len(p3.Trail)
	// 重放不追加轨迹。
	if _, _, err := s.CancelManifest("MC1", "M2", "计划调整", tClock(2026, 10, 5, 18, 0)); err != nil {
		t.Fatalf("取消重放不应失败: %v", err)
	}
	p3, _ = s.Query("P003")
	if len(p3.Trail) != trailLen {
		t.Fatal("取消重放不应追加轨迹")
	}
	// 换内容冲突：换清单、换原因。
	if _, _, err := s.CancelManifest("MC1", "M1", "计划调整", tClock(2026, 10, 5, 18, 0)); err == nil {
		t.Fatal("换清单应报冲突")
	}
	if _, _, err := s.CancelManifest("MC1", "M2", "别的原因", tClock(2026, 10, 5, 18, 0)); err == nil {
		t.Fatal("换原因应报冲突")
	}
	// 换号再次取消同一清单拒绝。
	if _, _, err := s.CancelManifest("MC2", "M2", "计划调整", tClock(2026, 10, 5, 18, 0)); err == nil ||
		!strings.Contains(err.Error(), "已取消") {
		t.Fatalf("换号再次取消同一清单应拒绝: %v", err)
	}
}

func TestManifestTerminatedNoRestriction(t *testing.T) {
	s, _ := openTempStore(t)
	setupManifestBase(t, s)
	// 取消后成员可正常作业：直接 dispatch。
	mustCreateManifest(t, s, "M1", "站点A", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	mustCancelManifest(t, s, "MC1", "M1", "计划调整", tClock(2026, 10, 5, 11, 0))
	if _, _, err := s.Dispatch("B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatalf("取消后成员应可直接 dispatch: %v", err)
	}
	// 确认出站并失败回执后，包裹回到在站，可加入新清单。
	mustCreateManifest(t, s, "M2", "站点A", []string{"P002"}, tClock(2026, 10, 5, 10, 0))
	mustConfirmManifest(t, s, "M2", "B2", "李四", tClock(2026, 10, 5, 11, 0))
	if _, _, err := s.Receipt("RC1", "B2", "P002", resultFailed, "收件人不在", tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatalf("Receipt 意外失败: %v", err)
	}
	mustCreateManifest(t, s, "M3", "站点A", []string{"P002"}, tClock(2026, 10, 5, 13, 0))
	if m := s.ReservedManifest("P002"); m == nil || m.Manifest != "M3" {
		t.Fatal("终结清单不应限制后续合法作业：P002 应被新清单 M3 预留")
	}
}

func TestManifestSaveFailureRollback(t *testing.T) {
	// 数据文件位于只读目录中，首次保存（新建文件）必然失败。
	dir := t.TempDir()
	ro := filepath.Join(dir, "readonly")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ro, "ledger.json")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s, "P002", "站点A", tClock(2026, 10, 5, 9, 0))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })

	// 创建清单保存失败：不占用清单号、不追加轨迹、不改写原数据。
	if _, _, err := s.CreateManifest("M1", "站点A", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0)); err == nil {
		t.Fatal("保存失败时创建不得返回成功")
	}
	if _, ok := s.data.Manifests["M1"]; ok {
		t.Fatal("保存失败必须回滚清单结果")
	}
	p, _ := s.Query("P001")
	if len(p.Trail) != 1 {
		t.Fatal("保存失败必须回滚轨迹")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(before) {
		t.Fatal("保存失败不得改写原有有效数据")
	}
}

func TestManifestConfirmSaveFailureRollback(t *testing.T) {
	s, path := openTempStore(t)
	setupManifestBase(t, s)
	mustCreateManifest(t, s, "M1", "站点A", []string{"P001"}, tClock(2026, 10, 5, 10, 0))

	// 把数据文件换到只读目录：确认时保存必然失败。
	dir := filepath.Dir(path)
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	roPath := filepath.Join(ro, "ledger.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(roPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })

	s2, err := Open(roPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.ConfirmManifest("M1", "B1", "张三", tClock(2026, 10, 5, 11, 0)); err == nil {
		t.Fatal("保存失败时确认不得返回成功")
	}
	if _, ok := s2.data.Batches["B1"]; ok {
		t.Fatal("保存失败必须回滚批次")
	}
	m := s2.data.Manifests["M1"]
	if m.ConfirmedBatch != "" {
		t.Fatal("保存失败必须回滚清单确认标记")
	}
	p, _ := s2.Query("P001")
	if p.Status != statusInStation || len(p.Trail) != 2 {
		t.Fatal("保存失败必须回滚包裹状态与轨迹")
	}
	// 取消保存失败同样回滚。
	if _, _, err := s2.CancelManifest("MC1", "M1", "计划调整", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("保存失败时取消不得返回成功")
	}
	if _, ok := s2.data.ManifestCancels["MC1"]; ok {
		t.Fatal("保存失败必须回滚取消结果")
	}
	if m.CancelledBy != "" {
		t.Fatal("保存失败必须回滚清单取消标记")
	}
	// 磁盘上的原数据不变。
	got, err := os.ReadFile(roPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatal("保存失败不得改写原有有效数据")
	}
}

func TestManifestLegacyDataCompat(t *testing.T) {
	// 旧版数据文件没有 manifests/manifestCancels 字段：旧有效台账直接使用。
	s, path := openTempStore(t)
	setupManifestBase(t, s)
	if _, _, err := s.Dispatch("B0", "站点A", "张三", []string{"P003"}, tClock(2026, 10, 5, 10, 0)); err != nil {
		t.Fatalf("Dispatch 意外失败: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "manifests")
	delete(doc, "manifestCancels")
	legacy := path + ".legacy"
	buf, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, buf, 0o644); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(legacy)
	if err != nil {
		t.Fatalf("旧版数据文件应直接可用: %v", err)
	}
	// 旧数据保持可用：既有批次可回执。
	if _, _, err := s2.Receipt("RC1", "B0", "P003", resultSigned, "", tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatalf("旧台账上的既有作业应照常: %v", err)
	}
	// 新功能在旧台账上可用。
	mustCreateManifest(t, s2, "M1", "站点A", []string{"P001", "P002"}, tClock(2026, 10, 5, 12, 0))
	mustConfirmManifest(t, s2, "M1", "B1", "李四", tClock(2026, 10, 5, 13, 0))
	// 重载后一致。
	s3, err := Open(legacy)
	if err != nil {
		t.Fatalf("重载失败: %v", err)
	}
	m, err := s3.ManifestQuery("M1")
	if err != nil || m.ConfirmedBatch != "B1" {
		t.Fatalf("重载后清单结果应完整: %+v err=%v", m, err)
	}
}

func TestManifestCorruptReloadRejected(t *testing.T) {
	s, path := openTempStore(t)
	setupManifestBase(t, s)
	mustCreateManifest(t, s, "M1", "站点A", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	mustCreateManifest(t, s, "M2", "站点A", []string{"P003"}, tClock(2026, 10, 5, 10, 30))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tamper := func(mut func(map[string]any)) string {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		mut(doc)
		buf, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		bad := path + ".bad"
		if err := os.WriteFile(bad, buf, 0o644); err != nil {
			t.Fatal(err)
		}
		return bad
	}

	// 新增关联空缺：轨迹引用的清单不存在。
	bad := tamper(func(doc map[string]any) { delete(doc, "manifests") })
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("清单关联空缺应拒绝读写: %v", err)
	}
	// 重复预留：两件待出站清单包含同一包裹。
	bad = tamper(func(doc map[string]any) {
		m2 := doc["manifests"].(map[string]any)["M2"].(map[string]any)
		m2["parcels"] = []any{"P003", "P001"}
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("重复预留应拒绝读写: %v", err)
	}
	// 清单与轨迹矛盾：清单成员缺少清单预留轨迹。
	bad = tamper(func(doc map[string]any) {
		m1 := doc["manifests"].(map[string]any)["M1"].(map[string]any)
		m1["parcels"] = []any{"P001", "P002", "P003"}
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("清单与轨迹矛盾应拒绝读写: %v", err)
	}
	// 清单与确认批次矛盾：确认批次成员与清单不一致。
	bad = tamper(func(doc map[string]any) {
		m1 := doc["manifests"].(map[string]any)["M1"].(map[string]any)
		m1["confirmedBatch"] = "B9"
		doc["batches"].(map[string]any)["B9"] = map[string]any{
			"batch": "B9", "station": "站点A", "courier": "张三",
			"parcels": []any{"P001"}, "time": "2026-10-05T11:00:00Z", "receipts": map[string]any{},
		}
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("清单与确认批次矛盾应拒绝读写: %v", err)
	}
	// 取消标记无法对应：清单标记了不存在的取消请求号。
	bad = tamper(func(doc map[string]any) {
		m1 := doc["manifests"].(map[string]any)["M1"].(map[string]any)
		m1["cancelledBy"] = "MC9"
	})
	if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("取消标记无法对应应拒绝读写: %v", err)
	}
	// 原文件未被篡改，仍可正常打开。
	if _, err := Open(path); err != nil {
		t.Fatalf("原台账不应受影响: %v", err)
	}
}
