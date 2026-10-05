package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tClock(y int, mo time.Month, d, h, min int) time.Time {
	return time.Date(y, mo, d, h, min, 0, 0, time.UTC)
}

func openTempStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open 不存在的文件失败: %v", err)
	}
	return s, path
}

func mustRegister(t *testing.T, s *Store, id, station string, now time.Time) {
	t.Helper()
	if _, err := s.Register(id, station, now); err != nil {
		t.Fatalf("Register(%q) 意外失败: %v", id, err)
	}
}

func TestRegisterAndQuery(t *testing.T) {
	s, _ := openTempStore(t)
	now := tClock(2026, 10, 5, 9, 0)

	p, err := s.Register("P001", "站点A", now)
	if err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	if p.Station != "站点A" || p.Status != statusInStation {
		t.Fatalf("登记后站点/状态不符: %+v", p)
	}
	if len(p.Trail) != 1 || p.Trail[0].Op != "收件" || p.Trail[0].Station != "站点A" ||
		!p.Trail[0].Time.Equal(now) || p.Trail[0].Request != "" {
		t.Fatalf("收件记录不符: %+v", p.Trail)
	}

	got, err := s.Query("P001")
	if err != nil {
		t.Fatalf("Query 失败: %v", err)
	}
	if got.Station != "站点A" || len(got.Trail) != 1 {
		t.Fatalf("查询结果不符: %+v", got)
	}

	if _, err := s.Query("NOPE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的包裹应返回 ErrNotFound，得到 %v", err)
	}
}

func TestDuplicateRegisterDoesNotOverwrite(t *testing.T) {
	s, _ := openTempStore(t)
	mustRegister(t, s, "P001", "站点A", tClock(2026, 10, 5, 9, 0))

	if _, err := s.Register("P001", "站点B", tClock(2026, 10, 5, 10, 0)); err == nil {
		t.Fatal("重复登记必须报错")
	}
	p, _ := s.Query("P001")
	if p.Station != "站点A" || len(p.Trail) != 1 {
		t.Fatalf("重复登记不得覆盖原记录: %+v", p)
	}
}

func TestHandoffSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)

	t2 := tClock(2026, 10, 5, 11, 30)
	res, replayed, err := s.Handoff("R1", "站点A", "站点B", []string{"P001", "P002"}, t2)
	if err != nil || replayed {
		t.Fatalf("交接应成功且非重放: %v replayed=%v", err, replayed)
	}
	if res.Request != "R1" || res.From != "站点A" || res.To != "站点B" ||
		len(res.Parcels) != 2 || res.Parcels[0] != "P001" || res.Parcels[1] != "P002" {
		t.Fatalf("交接结果不符: %+v", res)
	}

	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		if p.Station != "站点B" || p.Status != statusInStation {
			t.Fatalf("%s 交接后应在站点B且在站: %+v", id, p)
		}
		if len(p.Trail) != 2 {
			t.Fatalf("%s 应有两条轨迹，得到 %d", id, len(p.Trail))
		}
		e := p.Trail[1]
		if e.Op != "交接" || e.Station != "站点B" || e.Request != "R1" || !e.Time.Equal(t2) {
			t.Fatalf("%s 交接记录不符: %+v", id, e)
		}
	}
}

func TestHandoffFailsAtomicWhenAnyParcelInvalid(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点C", t1) // 不属于源站点 A
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		parcels []string
	}{
		{"含未登记包裹", []string{"P001", "GHOST"}},
		{"含非源站点包裹", []string{"P001", "P003"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Handoff("RX", "站点A", "站点B", c.parcels, tClock(2026, 10, 5, 12, 0)); err == nil {
				t.Fatal("存在不合规包裹时整次交接必须失败")
			}
			for _, id := range []string{"P001", "P002", "P003"} {
				p, _ := s.Query(id)
				wantStation := map[string]string{"P001": "站点A", "P002": "站点A", "P003": "站点C"}[id]
				if p.Station != wantStation || len(p.Trail) != 1 {
					t.Fatalf("失败交接不得改动 %s: station=%q trail=%d", id, p.Station, len(p.Trail))
				}
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("失败交接不得写入数据文件")
			}
		})
	}
}

func TestFailedHandoffDoesNotOccupyRequest(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)

	// 首次提交失败（包裹在站点C，不在源站点B）。
	if _, _, err := s.Handoff("R1", "站点B", "站点X", []string{"P001"}, t1); err == nil {
		t.Fatal("首次提交应失败")
	}
	// 纠正原因后用同一请求号重试，应当成功。
	res, replayed, err := s.Handoff("R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	if err != nil || replayed {
		t.Fatalf("纠正后重试应作为新交接成功: %v replayed=%v", err, replayed)
	}
	if res.To != "站点B" {
		t.Fatalf("重试结果不符: %+v", res)
	}
}

func TestHandoffIdempotentReplay(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)

	first, _, err := s.Handoff("R1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	if err != nil {
		t.Fatal(err)
	}

	// 相同内容、集合顺序不同：必须返回首次结果且不追加轨迹。
	again, replayed, err := s.Handoff("R1", "站点A", "站点B", []string{"P002", "P001"}, tClock(2026, 10, 5, 11, 0))
	if err != nil || !replayed {
		t.Fatalf("同内容重复提交应重放: %v replayed=%v", err, replayed)
	}
	if again != first {
		t.Fatal("重放必须返回首次保存的结果对象")
	}
	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		if len(p.Trail) != 2 {
			t.Fatalf("重放不得追加轨迹，%s 有 %d 条", id, len(p.Trail))
		}
	}

	// 包裹后来已交往其他站点：重放仍按已保存结果返回，不再检查源站条件。
	if _, _, err := s.Handoff("R2", "站点B", "站点C", []string{"P001", "P002"}, tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatal(err)
	}
	replay, replayed2, err := s.Handoff("R1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 13, 0))
	if err != nil || !replayed2 || replay != first {
		t.Fatalf("包裹已流向他站后重放仍须返回首次结果: err=%v replayed=%v", err, replayed2)
	}
	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		if len(p.Trail) != 3 {
			t.Fatalf("重放不得追加轨迹，%s 有 %d 条", id, len(p.Trail))
		}
	}
}

func TestHandoffRequestConflict(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	first, _, err := s.Handoff("R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	if err != nil {
		t.Fatal(err)
	}

	conflicts := []struct {
		name    string
		from    string
		to      string
		parcels []string
	}{
		{"目的不同", "站点A", "站点X", []string{"P001"}},
		{"源不同", "站点X", "站点B", []string{"P001"}},
		{"集合不同", "站点A", "站点B", []string{"P001", "P002"}},
	}
	for _, c := range conflicts {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Handoff("R1", c.from, c.to, c.parcels, tClock(2026, 10, 5, 11, 0)); err == nil {
				t.Fatal("请求号相同但业务内容不同必须报冲突")
			}
		})
	}
	// 已有结果保持不变。
	saved := s.data.Handoffs["R1"]
	if saved != first || saved.To != "站点B" || len(saved.Parcels) != 1 {
		t.Fatalf("冲突提交不得改动已有结果: %+v", saved)
	}
	p, _ := s.Query("P001")
	if p.Station != "站点B" || len(p.Trail) != 2 {
		t.Fatalf("冲突提交不得改动包裹归属与轨迹: %+v", p)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	_, path := openTempStore(t)
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s1, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	if _, _, err := s1.Handoff("R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0)); err != nil {
		t.Fatal(err)
	}

	// 重新打开：查询、轨迹与请求去重规则仍成立。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	p, err := s2.Query("P001")
	if err != nil || p.Station != "站点B" || p.Status != statusInStation || len(p.Trail) != 2 {
		t.Fatalf("重启后数据不符: %+v err=%v", p, err)
	}
	if e := p.Trail[1]; e.Request != "R1" {
		t.Fatalf("交接记录应含请求号: %+v", e)
	}
	got, replayed, err := s2.Handoff("R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 11, 0))
	if err != nil || !replayed || got.To != "站点B" {
		t.Fatalf("重启后重复请求应重放首次结果: %v replayed=%v", err, replayed)
	}
}

func TestCorruptFileRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")

	cases := map[string]string{
		"非法JSON":   "{not json",
		"空文件":      "",
		"空白文件":     "   \n ",
		"缺少字段":     `{"version":1}`,
		"版本不支持":    `{"version":99,"parcels":{},"handoffs":{}}`,
		"包裹记录损坏":   `{"version":1,"parcels":{"P001":null},"handoffs":{}}`,
		"交接引用丢失包裹": `{"version":1,"parcels":{},"handoffs":{"R1":{"request":"R1","from":"A","to":"B","parcels":["P9"],"time":"2026-10-05T09:00:00Z"}}}`,
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

func TestSaveFailureRollsBack(t *testing.T) {
	// 数据文件位于只读目录中，首次保存（新建文件）必然失败。
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

	s, err := Open(path) // 文件尚不存在，内存台账可建立
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register("P001", "站点A", tClock(2026, 10, 5, 9, 0)); err == nil {
		t.Fatal("保存失败时登记不得返回成功")
	}
	if _, ok := s.data.Parcels["P001"]; ok {
		t.Fatal("保存失败必须回滚内存变更")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("保存失败不得留下数据文件或临时文件, stat err=%v", err)
	}
}

func TestSameSet(t *testing.T) {
	if !sameSet(nil, nil) {
		t.Fatal("两个空集合应相同")
	}
	if sameSet([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("长度不同应不同")
	}
	if !sameSet([]string{"a", "b"}, []string{"b", "a"}) {
		t.Fatal("顺序不应影响集合判定")
	}
	if sameSet([]string{"a", "b"}, []string{"a", "c"}) {
		t.Fatal("元素不同应不同")
	}
}

func TestWhitespaceIDsAreDataNotAcceptedByCaller(t *testing.T) {
	// Store 层信任调用方已清洗；空白清洗规则在 CLI 层强制。
	v, err := cleanID("包裹编号", "  P1  ")
	if err != nil || v != "P1" {
		t.Fatalf("cleanID 应去除两端空白: %q %v", v, err)
	}
	if _, err := cleanID("包裹编号", "   \t "); err == nil || !strings.Contains(err.Error(), "不可为空") {
		t.Fatalf("纯空白应报错，得到 %v", err)
	}
}

func mustHandoff(t *testing.T, s *Store, req, from, to string, parcels []string, now time.Time) {
	t.Helper()
	if _, _, err := s.Handoff(req, from, to, parcels, now); err != nil {
		t.Fatalf("Handoff(%q) 意外失败: %v", req, err)
	}
}

func TestReturnSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	t2 := tClock(2026, 10, 5, 10, 0)
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001", "P002"}, t2)

	t3 := tClock(2026, 10, 5, 12, 0)
	res, replayed, err := s.Return("RT1", "R1", "错发站点", t3)
	if err != nil || replayed {
		t.Fatalf("退回应成功且非重放: %v replayed=%v", err, replayed)
	}
	if res.Request != "RT1" || res.Handoff != "R1" || res.From != "站点B" || res.To != "站点A" ||
		res.Reason != "错发站点" || len(res.Parcels) != 2 || res.Parcels[0] != "P001" || res.Parcels[1] != "P002" ||
		!res.Time.Equal(t3) {
		t.Fatalf("退回结果不符: %+v", res)
	}

	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		if p.Station != "站点A" || p.Status != statusInStation {
			t.Fatalf("%s 退回后应在站点A且在站: %+v", id, p)
		}
		if len(p.Trail) != 3 {
			t.Fatalf("%s 应有三条轨迹，得到 %d", id, len(p.Trail))
		}
		// 原收件、交接轨迹保留不变。
		if p.Trail[0].Op != "收件" || p.Trail[0].Station != "站点A" {
			t.Fatalf("%s 收件记录被改写: %+v", id, p.Trail[0])
		}
		if p.Trail[1].Op != "交接" || p.Trail[1].Station != "站点B" || p.Trail[1].Request != "R1" {
			t.Fatalf("%s 交接记录被改写: %+v", id, p.Trail[1])
		}
		e := p.Trail[2]
		if e.Op != "退回" || e.Station != "站点A" || e.From != "站点B" ||
			e.Request != "RT1" || e.RefRequest != "R1" || e.Reason != "错发站点" || !e.Time.Equal(t3) {
			t.Fatalf("%s 退回记录不符: %+v", id, e)
		}
	}
	if s.data.Handoffs["R1"].ReturnedBy != "RT1" {
		t.Fatalf("原交接应标记已退回: %+v", s.data.Handoffs["R1"])
	}
}

func TestReturnFailuresAtomic(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	// P002 经另一交接离开目的站 B。
	mustHandoff(t, s, "R2", "站点B", "站点C", []string{"P002"}, tClock(2026, 10, 5, 11, 0))
	// P003 交接后又回到 B，但最后一条流转不是 R1。
	mustHandoff(t, s, "R3", "站点A", "站点B", []string{"P003"}, tClock(2026, 10, 5, 10, 30))
	mustHandoff(t, s, "R4", "站点B", "站点C", []string{"P003"}, tClock(2026, 10, 5, 11, 30))
	mustHandoff(t, s, "R5", "站点C", "站点B", []string{"P003"}, tClock(2026, 10, 5, 12, 0))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		req     string
		handoff string
		reason  string
	}{
		{"原交接不存在", "RT1", "NOPE", "错发"},
		{"批次含已离开目的站的包裹", "RT2", "R1", "错发"},
		{"回到同一站点但不能退回旧交接", "RT3", "R3", "错发"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Return(c.req, c.handoff, c.reason, tClock(2026, 10, 5, 13, 0)); err == nil {
				t.Fatal("条件不满足时整批退回必须失败")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("失败退回不得写入数据文件")
			}
			if _, ok := s.data.Returns[c.req]; ok {
				t.Fatalf("失败的首次退回不得占用退回请求号 %q", c.req)
			}
		})
	}

	// 包裹归属与轨迹均未变。
	p1, _ := s.Query("P001")
	if p1.Station != "站点B" || len(p1.Trail) != 2 {
		t.Fatalf("失败退回不得改动 P001: %+v", p1)
	}
	p3, _ := s.Query("P003")
	if p3.Station != "站点B" || len(p3.Trail) != 4 {
		t.Fatalf("失败退回不得改动 P003: %+v", p3)
	}

	// 失败的请求号纠正后可复用：先把 P002 交回 B 不满足（最后一条须为 R1），
	// 改用未退回的 R3 也不行；这里验证失败号 RT2 可用于一次新的失败重试后仍不占用。
	if _, _, err := s.Return("RT2", "R3", "错发", tClock(2026, 10, 5, 13, 30)); err == nil {
		t.Fatal("R3 的包裹最后一条流转不是 R3，应失败")
	}
	if _, ok := s.data.Returns["RT2"]; ok {
		t.Fatal("再次失败的退回仍不得占用请求号 RT2")
	}
}

func TestReturnOnlyOncePerHandoff(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	if _, _, err := s.Return("RT1", "R1", "错发", tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatal(err)
	}
	// 换一个退回请求号再次退回同一原交接：必须失败。
	if _, _, err := s.Return("RT2", "R1", "再次退回", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("已退回的原交接不能再次退回")
	}
	p, _ := s.Query("P001")
	if p.Station != "站点A" || len(p.Trail) != 3 {
		t.Fatalf("二次退回不得改动包裹: %+v", p)
	}
}

func TestReturnReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))

	first, _, err := s.Return("RT1", "R1", "错发站点", tClock(2026, 10, 5, 11, 0))
	if err != nil {
		t.Fatal(err)
	}
	// 相同退回请求号、相同原交接与原因：重放首次结果，不追加轨迹。
	again, replayed, err := s.Return("RT1", "R1", "错发站点", tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayed || again != first {
		t.Fatalf("同内容重复退回应重放: err=%v replayed=%v", err, replayed)
	}
	// 包裹之后又被交接：重放仍返回首次结果，不重新检查、不改动现状。
	mustHandoff(t, s, "R2", "站点A", "站点C", []string{"P001", "P002"}, tClock(2026, 10, 5, 13, 0))
	replay, replayed2, err := s.Return("RT1", "R1", "错发站点", tClock(2026, 10, 5, 14, 0))
	if err != nil || !replayed2 || replay != first {
		t.Fatalf("包裹再交接后重放仍须返回首次结果: err=%v replayed=%v", err, replayed2)
	}
	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		if p.Station != "站点C" || len(p.Trail) != 4 {
			t.Fatalf("重放不得追加轨迹或改变站点，%s: station=%q trail=%d", id, p.Station, len(p.Trail))
		}
	}

	// 同一退回请求号换原交接或原因：冲突。
	mustHandoff(t, s, "R9", "站点C", "站点A", []string{"P001"}, tClock(2026, 10, 5, 15, 0))
	if _, _, err := s.Return("RT1", "R9", "错发站点", tClock(2026, 10, 5, 16, 0)); err == nil {
		t.Fatal("换原交接必须报冲突")
	}
	if _, _, err := s.Return("RT1", "R1", "别的原因", tClock(2026, 10, 5, 16, 0)); err == nil {
		t.Fatal("换原因必须报冲突")
	}
	saved := s.data.Returns["RT1"]
	if saved != first || saved.Handoff != "R1" || saved.Reason != "错发站点" {
		t.Fatalf("冲突提交不得改动已有退回结果: %+v", saved)
	}
}

func TestReturnRequestScopeIndependent(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	// 退回请求号与交接请求号分属独立去重范围，允许同名。
	res, replayed, err := s.Return("R1", "R1", "错发", tClock(2026, 10, 5, 11, 0))
	if err != nil || replayed || res.Request != "R1" || res.Handoff != "R1" {
		t.Fatalf("退回请求号与交接请求号同名应可用: %v replayed=%v res=%+v", err, replayed, res)
	}
	// 原 handoff 请求同内容重放照常返回首次交接结果，不重新移动包裹。
	h, replayedH, err := s.Handoff("R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 12, 0))
	if err != nil || !replayedH || h.To != "站点B" {
		t.Fatalf("已退回的原交接重放应返回首次交接结果: err=%v replayed=%v", err, replayedH)
	}
	p, _ := s.Query("P001")
	if p.Station != "站点A" || len(p.Trail) != 3 {
		t.Fatalf("交接重放不得重新移动包裹: %+v", p)
	}
	// 原 handoff 请求换内容仍冲突。
	if _, _, err := s.Handoff("R1", "站点A", "站点C", []string{"P001"}, tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("原交接请求号换内容仍须报冲突")
	}
}

func TestReturnPersistenceAcrossRestart(t *testing.T) {
	_, path := openTempStore(t)
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s1, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustHandoff(t, s1, "R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	if _, _, err := s1.Return("RT1", "R1", "错发", tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	p, _ := s2.Query("P001")
	if p.Station != "站点A" || len(p.Trail) != 3 || p.Trail[2].Op != "退回" || p.Trail[2].RefRequest != "R1" {
		t.Fatalf("重启后退回轨迹不符: %+v", p)
	}
	// 重启后退回重放仍成立。
	if _, replayed, err := s2.Return("RT1", "R1", "错发", tClock(2026, 10, 5, 12, 0)); err != nil || !replayed {
		t.Fatalf("重启后退回重放失败: %v replayed=%v", err, replayed)
	}
	// 重启后同一原交接仍不能再次退回。
	if _, _, err := s2.Return("RT2", "R1", "再来", tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatal("重启后已退回的原交接仍须拒绝再次退回")
	}
	// 重启后原交接重放仍成立。
	if _, replayedH, err := s2.Handoff("R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 13, 0)); err != nil || !replayedH {
		t.Fatalf("重启后原交接重放失败: %v replayed=%v", err, replayedH)
	}
}

func TestCorruptReturnDataRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")

	parcel := `"id":"P1","station":"A","status":"在站","registered":"2026-10-05T09:00:00Z"`
	goodTrail := `"op":"收件","station":"A","time":"2026-10-05T09:00:00Z"`
	cases := map[string]string{
		"轨迹缺少操作": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":[{"station":"A","time":"2026-10-05T09:00:00Z"}]}},"handoffs":{},"returns":{}}`,
		"轨迹缺少站点": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":[{"op":"收件","time":"2026-10-05T09:00:00Z"}]}},"handoffs":{},"returns":{}}`,
		"轨迹缺少时间": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":[{"op":"收件","station":"A"}]}},"handoffs":{},"returns":{}}`,
		"退回引用不存在的原交接": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":[{` + goodTrail + `}]}},"handoffs":{},` +
			`"returns":{"RT1":{"request":"RT1","handoff":"NOPE","from":"B","to":"A","reason":"x","parcels":["P1"],"time":"2026-10-05T10:00:00Z"}}}`,
		"退回轨迹引用不存在的原交接": `{"version":1,"parcels":{"P1":{` + parcel + `,"trail":[{` + goodTrail + `},` +
			`{"op":"退回","station":"A","from":"B","time":"2026-10-05T10:00:00Z","request":"RT1","refRequest":"NOPE","reason":"x"}]}},"handoffs":{},"returns":{}}`,
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

func TestLegacyLedgerWithoutReturnsLoads(t *testing.T) {
	// 早期版本的数据文件没有 returns 字段，应无需手工修改即可使用。
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
	if _, _, err := s.Handoff("R1", "A", "B", []string{"P1"}, tClock(2026, 10, 5, 10, 0)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Return("RT1", "R1", "错发", tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatalf("旧台账升级后应可退回: %v", err)
	}
}

func mustDispatch(t *testing.T, s *Store, batch, station, courier string, parcels []string, now time.Time) {
	t.Helper()
	if _, _, err := s.Dispatch(batch, station, courier, parcels, now); err != nil {
		t.Fatalf("Dispatch(%q) 意外失败: %v", batch, err)
	}
}

func TestDispatchSuccess(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)

	t2 := tClock(2026, 10, 5, 10, 0)
	res, replayed, err := s.Dispatch("B1", "站点A", "张三", []string{"P001", "P002"}, t2)
	if err != nil || replayed {
		t.Fatalf("出站应成功且非重放: %v replayed=%v", err, replayed)
	}
	if res.Batch != "B1" || res.Station != "站点A" || res.Courier != "张三" ||
		len(res.Parcels) != 2 || res.Parcels[0] != "P001" || res.Parcels[1] != "P002" ||
		!res.Time.Equal(t2) || res.Done() {
		t.Fatalf("出站结果不符: %+v", res)
	}

	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		if p.Station != "站点A" || p.Status != statusDelivering {
			t.Fatalf("%s 出站后应仍记出发站且配送中: %+v", id, p)
		}
		if len(p.Trail) != 2 {
			t.Fatalf("%s 应有两条轨迹，得到 %d", id, len(p.Trail))
		}
		e := p.Trail[1]
		if e.Op != "出站" || e.Station != "站点A" || e.Batch != "B1" || e.Courier != "张三" || !e.Time.Equal(t2) {
			t.Fatalf("%s 出站记录不符: %+v", id, e)
		}
	}
}

func TestDispatchFailsAtomic(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点C", t1)
	mustDispatch(t, s, "B0", "站点A", "张三", []string{"P002"}, tClock(2026, 10, 5, 9, 30)) // P002 已在配送中
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		parcels []string
	}{
		{"含未登记包裹", []string{"P001", "GHOST"}},
		{"含非出发站包裹", []string{"P001", "P003"}},
		{"含配送中包裹", []string{"P001", "P002"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Dispatch("B1", "站点A", "张三", c.parcels, tClock(2026, 10, 5, 10, 0)); err == nil {
				t.Fatal("存在不合规包裹时整批出站必须失败")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("失败出站不得写入数据文件")
			}
			if _, ok := s.data.Batches["B1"]; ok {
				t.Fatal("失败的首次出站不得占用批次号")
			}
			p1, _ := s.Query("P001")
			if p1.Status != statusInStation || len(p1.Trail) != 1 {
				t.Fatalf("失败出站不得改动 P001: %+v", p1)
			}
		})
	}

	// 失败不占号：纠正后同批次号可成功。
	if _, replayed, err := s.Dispatch("B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 11, 0)); err != nil || replayed {
		t.Fatalf("纠正后同批次号出站应成功: %v replayed=%v", err, replayed)
	}
}

func TestDispatchReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	first, _, err := s.Dispatch("B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	if err != nil {
		t.Fatal(err)
	}

	// 同批次号、同站点、同配送员、集合换序：重放首次结果及时间，不追加轨迹。
	again, replayed, err := s.Dispatch("B1", "站点A", "张三", []string{"P002", "P001"}, tClock(2026, 10, 5, 11, 0))
	if err != nil || !replayed || again != first {
		t.Fatalf("同内容重复出站应重放: err=%v replayed=%v", err, replayed)
	}
	for _, id := range []string{"P001", "P002"} {
		p, _ := s.Query(id)
		if len(p.Trail) != 2 {
			t.Fatalf("重放不得追加轨迹，%s 有 %d 条", id, len(p.Trail))
		}
	}

	// 同批次号换内容：冲突。
	for _, c := range []struct {
		station, courier string
		parcels          []string
	}{
		{"站点B", "张三", []string{"P001", "P002"}},
		{"站点A", "李四", []string{"P001", "P002"}},
		{"站点A", "张三", []string{"P001"}},
	} {
		if _, _, err := s.Dispatch("B1", c.station, c.courier, c.parcels, tClock(2026, 10, 5, 12, 0)); err == nil {
			t.Fatalf("批次号相同但内容不同（%v）必须报冲突", c)
		}
	}

	// 批次完成后也不能复用：全部回执后同号换内容仍冲突，同内容仍重放。
	if _, _, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 13, 0)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Receipt("RC2", "B1", "P002", resultSigned, "", tClock(2026, 10, 5, 13, 30)); err != nil {
		t.Fatal(err)
	}
	if !s.data.Batches["B1"].Done() {
		t.Fatal("全部回执后批次应已完成")
	}
	if _, _, err := s.Dispatch("B1", "站点A", "张三", []string{"P001"}, tClock(2026, 10, 5, 14, 0)); err == nil {
		t.Fatal("批次完成后同号换内容仍须报冲突")
	}
	if _, replayed, err := s.Dispatch("B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 14, 30)); err != nil || !replayed {
		t.Fatalf("批次完成后同内容仍须重放: %v replayed=%v", err, replayed)
	}
}

func TestReceiptSignedAndFailed(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))

	// 签收：状态为已签收，站点保留。
	t3 := tClock(2026, 10, 5, 11, 0)
	res, replayed, err := s.Receipt("RC1", "B1", "P001", resultSigned, "", t3)
	if err != nil || replayed {
		t.Fatalf("签收回执应成功且非重放: %v replayed=%v", err, replayed)
	}
	if res.Request != "RC1" || res.Batch != "B1" || res.Parcel != "P001" || res.Result != resultSigned ||
		res.Reason != "" || !res.Time.Equal(t3) {
		t.Fatalf("回执结果不符: %+v", res)
	}
	p1, _ := s.Query("P001")
	if p1.Status != statusSigned || p1.Station != "站点A" {
		t.Fatalf("签收后应为已签收且站点保留: %+v", p1)
	}
	e1 := p1.Trail[len(p1.Trail)-1]
	if e1.Op != "回执" || e1.Batch != "B1" || e1.Result != resultSigned || e1.Request != "RC1" || !e1.Time.Equal(t3) {
		t.Fatalf("签收回执轨迹不符: %+v", e1)
	}
	if s.data.Batches["B1"].Done() {
		t.Fatal("尚有成员未回执，批次不应完成")
	}

	// 失败：实物回到出发站，恢复在站，轨迹带原因。
	t4 := tClock(2026, 10, 5, 12, 0)
	res2, _, err := s.Receipt("RC2", "B1", "P002", resultFailed, "收件人不在", t4)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Reason != "收件人不在" {
		t.Fatalf("失败回执应保留原因: %+v", res2)
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusInStation || p2.Station != "站点A" {
		t.Fatalf("失败回执后应恢复在站并回到出发站: %+v", p2)
	}
	e2 := p2.Trail[len(p2.Trail)-1]
	if e2.Op != "回执" || e2.Result != resultFailed || e2.Reason != "收件人不在" || e2.Request != "RC2" {
		t.Fatalf("失败回执轨迹不符: %+v", e2)
	}

	// 全部成员回执后批次自动完成。
	if !s.data.Batches["B1"].Done() {
		t.Fatal("全部成员回执后批次应自动完成")
	}

	// 失败包裹可加入新的配送批次。
	mustDispatch(t, s, "B2", "站点A", "李四", []string{"P002"}, tClock(2026, 10, 5, 13, 0))
	if p, _ := s.Query("P002"); p.Status != statusDelivering {
		t.Fatalf("失败包裹重新出站后应配送中: %+v", p)
	}
}

func TestReceiptFailuresAtomic(t *testing.T) {
	s, path := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name           string
		req, batch, id string
	}{
		{"批次不存在", "RC1", "NOPE", "P001"},
		{"包裹不属于批次", "RC2", "B1", "P003"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Receipt(c.req, c.batch, c.id, resultSigned, "", tClock(2026, 10, 5, 11, 0)); err == nil {
				t.Fatal("条件不满足时回执必须失败")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("失败回执不得写入数据文件")
			}
			if _, ok := s.data.Receipts[c.req]; ok {
				t.Fatalf("失败的首次回执不得占用请求号 %q", c.req)
			}
		})
	}

	// 包裹不在该批次配送中（P002 尚未出站进 B2 却拿 B2 回执）。
	mustDispatch(t, s, "B2", "站点A", "李四", []string{"P003"}, tClock(2026, 10, 5, 11, 30))
	if _, _, err := s.Receipt("RC3", "B2", "P003", resultSigned, "", tClock(2026, 10, 5, 12, 0)); err != nil {
		t.Fatal(err)
	}
	// P003 已签收，换请求号在同一批次再回执：已回执，拒绝。
	if _, _, err := s.Receipt("RC4", "B2", "P003", resultSigned, "", tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatal("同一批次已回执的包裹换请求号再回执必须拒绝")
	}
	if _, ok := s.data.Receipts["RC4"]; ok {
		t.Fatal("被拒绝的回执不得占用请求号")
	}
}

func TestReceiptReplayAndConflict(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustDispatch(t, s, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))

	first, _, err := s.Receipt("RC1", "B1", "P002", resultFailed, "收件人不在", tClock(2026, 10, 5, 11, 0))
	if err != nil {
		t.Fatal(err)
	}
	// 失败包裹进入新批次后，同内容重放仍返回首次结果，不检查当前状态、不改变现状。
	mustDispatch(t, s, "B2", "站点A", "李四", []string{"P002"}, tClock(2026, 10, 5, 12, 0))
	again, replayed, err := s.Receipt("RC1", "B1", "P002", resultFailed, "收件人不在", tClock(2026, 10, 5, 13, 0))
	if err != nil || !replayed || again != first {
		t.Fatalf("同内容重复回执应重放: err=%v replayed=%v", err, replayed)
	}
	p2, _ := s.Query("P002")
	if p2.Status != statusDelivering || len(p2.Trail) != 4 {
		t.Fatalf("重放不得改变状态或追加轨迹: %+v", p2)
	}
	if len(s.data.Batches["B1"].Receipts) != 1 || len(s.data.Batches["B2"].Receipts) != 0 {
		t.Fatal("重放不得改变批次进度")
	}

	// 同请求号换内容：冲突。
	conflicts := []struct {
		batch, id, result, reason string
	}{
		{"B2", "P002", resultFailed, "收件人不在"},
		{"B1", "P001", resultFailed, "收件人不在"},
		{"B1", "P002", resultSigned, ""},
		{"B1", "P002", resultFailed, "别的原因"},
	}
	for _, c := range conflicts {
		if _, _, err := s.Receipt("RC1", c.batch, c.id, c.result, c.reason, tClock(2026, 10, 5, 14, 0)); err == nil {
			t.Fatalf("请求号相同但内容不同（%+v）必须报冲突", c)
		}
	}
	saved := s.data.Receipts["RC1"]
	if saved != first || saved.Batch != "B1" || saved.Reason != "收件人不在" {
		t.Fatalf("冲突提交不得改动已有回执结果: %+v", saved)
	}
}

func TestReceiptRequestScopeIndependent(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustRegister(t, s, "P002", "站点A", t1)
	mustRegister(t, s, "P003", "站点A", t1)
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001", "P002", "P003"}, tClock(2026, 10, 5, 10, 0))
	mustDispatch(t, s, "B1", "站点B", "张三", []string{"P001", "P002", "P003"}, tClock(2026, 10, 5, 11, 0))
	// 回执请求号单独去重，可与批次号、包裹号、交接请求号同名。
	for i, req := range []string{"B1", "P001", "R1"} {
		parcel := []string{"P001", "P002", "P003"}[i]
		if _, _, err := s.Receipt(req, "B1", parcel, resultSigned, "", tClock(2026, 10, 5, 12, 0)); err != nil {
			t.Fatalf("回执请求号 %q 与其他标识同名应可用: %v", req, err)
		}
	}
	// 同名交接请求的重放不受回执影响。
	if _, replayed, err := s.Handoff("R1", "站点A", "站点B", []string{"P001", "P002", "P003"}, tClock(2026, 10, 5, 13, 0)); err != nil || !replayed {
		t.Fatalf("同名回执不得影响交接去重: %v replayed=%v", err, replayed)
	}
}

func TestDispatchCountsAsNewFlowForReturnAndHandoff(t *testing.T) {
	s, _ := openTempStore(t)
	t1 := tClock(2026, 10, 5, 9, 0)
	mustRegister(t, s, "P001", "站点A", t1)
	mustHandoff(t, s, "R1", "站点A", "站点B", []string{"P001"}, tClock(2026, 10, 5, 10, 0))
	mustDispatch(t, s, "B1", "站点B", "张三", []string{"P001"}, tClock(2026, 10, 5, 11, 0))

	// 配送出站算新流转：不能退回此前旧交接。
	if _, _, err := s.Return("RT1", "R1", "错发", tClock(2026, 10, 5, 12, 0)); err == nil {
		t.Fatal("出站后不得退回此前旧交接")
	}
	// 配送中的包裹不能交接。
	if _, _, err := s.Handoff("R2", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 12, 30)); err == nil {
		t.Fatal("配送中的包裹不得交接")
	}
	// 回执失败恢复在站后也不能退回旧交接（回执同样算新流转）。
	if _, _, err := s.Receipt("RC1", "B1", "P001", resultFailed, "破损", tClock(2026, 10, 5, 13, 0)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Return("RT2", "R1", "错发", tClock(2026, 10, 5, 14, 0)); err == nil {
		t.Fatal("回执后不得退回此前旧交接")
	}
	// 但恢复在站后可以正常交接。
	mustHandoff(t, s, "R3", "站点B", "站点C", []string{"P001"}, tClock(2026, 10, 5, 15, 0))
}

func TestDispatchReceiptPersistenceAcrossRestart(t *testing.T) {
	_, path := openTempStore(t)
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s1, "P001", "站点A", tClock(2026, 10, 5, 9, 0))
	mustRegister(t, s1, "P002", "站点A", tClock(2026, 10, 5, 9, 0))
	mustDispatch(t, s1, "B1", "站点A", "张三", []string{"P001", "P002"}, tClock(2026, 10, 5, 10, 0))
	if _, _, err := s1.Receipt("RC1", "B1", "P001", resultFailed, "收件人不在", tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	p, _ := s2.Query("P001")
	if p.Status != statusInStation || len(p.Trail) != 3 || p.Trail[2].Op != "回执" || p.Trail[2].Batch != "B1" {
		t.Fatalf("重启后回执轨迹不符: %+v", p)
	}
	b, err := s2.BatchQuery("B1")
	if err != nil || b.Courier != "张三" || b.Station != "站点A" || len(b.Parcels) != 2 || b.Done() {
		t.Fatalf("重启后批次进度不符: %+v err=%v", b, err)
	}
	if e := b.Receipts["P001"]; e == nil || e.Request != "RC1" || e.Result != resultFailed || e.Reason != "收件人不在" {
		t.Fatalf("重启后批次内回执记录不符: %+v", e)
	}
	// 重启后出站重放与回执重放仍成立。
	if _, replayed, err := s2.Dispatch("B1", "站点A", "张三", []string{"P002", "P001"}, tClock(2026, 10, 5, 12, 0)); err != nil || !replayed {
		t.Fatalf("重启后出站重放失败: %v replayed=%v", err, replayed)
	}
	if _, replayed, err := s2.Receipt("RC1", "B1", "P001", resultFailed, "收件人不在", tClock(2026, 10, 5, 12, 30)); err != nil || !replayed {
		t.Fatalf("重启后回执重放失败: %v replayed=%v", err, replayed)
	}
	// 重启后同批次已回执包裹仍拒绝再次回执。
	if _, _, err := s2.Receipt("RC9", "B1", "P001", resultSigned, "", tClock(2026, 10, 5, 13, 0)); err == nil {
		t.Fatal("重启后已回执包裹仍须拒绝再次回执")
	}
}

func TestCorruptBatchReceiptDataRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")

	parcel := `"id":"P1","station":"A","status":"在站","registered":"2026-10-05T09:00:00Z"`
	trail := `"trail":[{"op":"收件","station":"A","time":"2026-10-05T09:00:00Z"}]`
	base := `"parcels":{"P1":{` + parcel + `,` + trail + `}},"handoffs":{}`
	batch := `"batches":{"B1":{"batch":"B1","station":"A","courier":"张三","parcels":["P1"],"time":"2026-10-05T10:00:00Z","receipts":{}}}`
	cases := map[string]string{
		"已保存退回结果为null": `{"version":1,` + base + `,"returns":{"RT1":null}}`,
		"已保存批次为null":   `{"version":1,` + base + `,` + `"batches":{"B1":null}}`,
		"已保存回执为null":   `{"version":1,` + base + `,` + batch + `,"receipts":{"RC1":null}}`,
		"批次成员为空": `{"version":1,` + base + `,` +
			`"batches":{"B1":{"batch":"B1","station":"A","courier":"张三","parcels":[],"time":"2026-10-05T10:00:00Z","receipts":{}}}}`,
		"批次缺少配送员": `{"version":1,` + base + `,` +
			`"batches":{"B1":{"batch":"B1","station":"A","courier":"","parcels":["P1"],"time":"2026-10-05T10:00:00Z","receipts":{}}}}`,
		"回执引用不存在的批次": `{"version":1,` + base + `,` +
			`"receipts":{"RC1":{"request":"RC1","batch":"NOPE","parcel":"P1","result":"签收","time":"2026-10-05T11:00:00Z"}}}`,
		"失败回执缺少原因": `{"version":1,` + base + `,` + batch + `,` +
			`"receipts":{"RC1":{"request":"RC1","batch":"B1","parcel":"P1","result":"失败","time":"2026-10-05T11:00:00Z"}}}`,
		"批次与回执记录不一致": `{"version":1,` + base + `,` + batch + `,` +
			`"receipts":{"RC1":{"request":"RC1","batch":"B1","parcel":"P1","result":"签收","time":"2026-10-05T11:00:00Z"}}}`,
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

func TestLegacyLedgerWithoutBatchesLoads(t *testing.T) {
	// 只有 register/handoff/return 数据的旧台账应可直接使用新功能。
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
	mustDispatch(t, s, "B1", "A", "张三", []string{"P1"}, tClock(2026, 10, 5, 10, 0))
	if _, _, err := s.Receipt("RC1", "B1", "P1", resultSigned, "", tClock(2026, 10, 5, 11, 0)); err != nil {
		t.Fatalf("旧台账升级后应可回执: %v", err)
	}
	b, err := s.BatchQuery("B1")
	if err != nil || !b.Done() {
		t.Fatalf("批次查询不符: %+v err=%v", b, err)
	}
}
