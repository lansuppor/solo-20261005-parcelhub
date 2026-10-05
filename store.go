package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// statusInStation 表示包裹当前在某站点（已收件或已完成交接）。
const statusInStation = "在站"

// Event 是包裹轨迹中的一条记录，按提交顺序追加。
type Event struct {
	Op         string    `json:"op"`                   // 收件 | 交接 | 退回
	Station    string    `json:"station"`              // 与该操作有关的站点（退回时为退回目的站，即原交接源站）
	Time       time.Time `json:"time"`                 // 发生时间
	Request    string    `json:"request,omitempty"`    // 交接/退回请求号（收件记录为空）
	From       string    `json:"from,omitempty"`       // 退回源站（原交接目的站，仅退回记录有）
	Reason     string    `json:"reason,omitempty"`     // 退回原因（仅退回记录有）
	RefRequest string    `json:"refRequest,omitempty"` // 被退回的原交接请求号（仅退回记录有）
}

// Parcel 是一件包裹的台账信息。
type Parcel struct {
	ID         string    `json:"id"`
	Station    string    `json:"station"`    // 当前归属站点
	Status     string    `json:"status"`     // 当前状态
	Registered time.Time `json:"registered"` // 收件时间
	Trail      []Event   `json:"trail"`      // 完整轨迹，按提交顺序排列
}

// HandoffResult 记录一次成功交接，用于请求号去重与结果重放。
type HandoffResult struct {
	Request    string    `json:"request"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Parcels    []string  `json:"parcels"` // 按首次提交时给出的顺序保存
	Time       time.Time `json:"time"`
	ReturnedBy string    `json:"returnedBy,omitempty"` // 已成功退回该交接的退回请求号（未退回为空）
}

// ReturnResult 记录一次成功的整批退回，用于退回请求号去重与结果重放。
type ReturnResult struct {
	Request string    `json:"request"` // 退回请求号
	Handoff string    `json:"handoff"` // 被退回的原交接请求号
	From    string    `json:"from"`    // 退回源站（原交接目的站）
	To      string    `json:"to"`      // 退回目的站（原交接源站）
	Reason  string    `json:"reason"`
	Parcels []string  `json:"parcels"` // 原交接批次，按原交接保存的顺序
	Time    time.Time `json:"time"`
}

// ledgerFile 是本地数据文件的磁盘结构。
type ledgerFile struct {
	Version  int                       `json:"version"`
	Parcels  map[string]*Parcel        `json:"parcels"`
	Handoffs map[string]*HandoffResult `json:"handoffs"`
	Returns  map[string]*ReturnResult  `json:"returns"`
}

// Store 是一个数据文件对应的包裹站点交接台账。
type Store struct {
	path string
	data ledgerFile
}

var (
	// ErrCorrupt 表示数据文件已存在但内容损坏，不得当作空库继续读写。
	ErrCorrupt = errors.New("数据文件已损坏")
	// ErrNotFound 表示包裹编号在台账中不存在。
	ErrNotFound = errors.New("包裹不存在")
)

// Open 打开（或在文件不存在时建立）一个本地台账。
// 文件不存在时仅初始化内存结构，真正建文件发生在首次成功保存时。
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.data = ledgerFile{Version: 1, Parcels: map[string]*Parcel{}, Handoffs: map[string]*HandoffResult{}, Returns: map[string]*ReturnResult{}}
			return s, nil
		}
		return nil, fmt.Errorf("读取数据文件失败: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%w: 文件为空", ErrCorrupt)
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if s.data.Version != 1 || s.data.Parcels == nil || s.data.Handoffs == nil {
		return nil, fmt.Errorf("%w: 缺少必要字段或版本不受支持", ErrCorrupt)
	}
	// 早期版本的数据文件没有 returns 字段：按空退回表处理，无需手工修改。
	if s.data.Returns == nil {
		s.data.Returns = map[string]*ReturnResult{}
	}
	if err := s.data.validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// validate 校验载入内容内部自洽，防止损坏数据被当作有效台账继续使用。
func (l *ledgerFile) validate() error {
	for id, p := range l.Parcels {
		if p == nil || id != p.ID || p.Station == "" || p.Status == "" || len(p.Trail) == 0 {
			return fmt.Errorf("%w: 包裹 %q 记录不完整", ErrCorrupt, id)
		}
		for i, e := range p.Trail {
			if e.Op == "" || e.Station == "" || e.Time.IsZero() {
				return fmt.Errorf("%w: 包裹 %q 第 %d 条轨迹缺少操作、站点或发生时间", ErrCorrupt, id, i+1)
			}
			if e.Op == "退回" {
				if e.Request == "" || e.Reason == "" || e.From == "" {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条退回轨迹缺少退回请求号、源站或原因", ErrCorrupt, id, i+1)
				}
				if _, ok := l.Handoffs[e.RefRequest]; !ok {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条退回轨迹引用了不存在的原交接 %q", ErrCorrupt, id, i+1, e.RefRequest)
				}
			}
		}
	}
	for req, h := range l.Handoffs {
		if h == nil || req != h.Request || h.From == "" || h.To == "" || len(h.Parcels) == 0 {
			return fmt.Errorf("%w: 请求号 %q 的交接结果不完整", ErrCorrupt, req)
		}
		for _, id := range h.Parcels {
			if _, ok := l.Parcels[id]; !ok {
				return fmt.Errorf("%w: 请求号 %q 引用了不存在的包裹 %q", ErrCorrupt, req, id)
			}
		}
		if h.ReturnedBy != "" {
			r, ok := l.Returns[h.ReturnedBy]
			if !ok || r.Handoff != req {
				return fmt.Errorf("%w: 交接 %q 标记的退回请求号 %q 无法对应", ErrCorrupt, req, h.ReturnedBy)
			}
		}
	}
	for req, r := range l.Returns {
		if r == nil || req != r.Request || r.Handoff == "" || r.From == "" || r.To == "" ||
			r.Reason == "" || len(r.Parcels) == 0 || r.Time.IsZero() {
			return fmt.Errorf("%w: 退回请求号 %q 的退回结果不完整", ErrCorrupt, req)
		}
		h, ok := l.Handoffs[r.Handoff]
		if !ok {
			return fmt.Errorf("%w: 退回请求号 %q 引用了不存在的原交接 %q", ErrCorrupt, req, r.Handoff)
		}
		if h.ReturnedBy != req || r.From != h.To || r.To != h.From || !sameSet(r.Parcels, h.Parcels) {
			return fmt.Errorf("%w: 退回请求号 %q 与原交接 %q 的记录不一致", ErrCorrupt, req, r.Handoff)
		}
		for _, id := range r.Parcels {
			if _, ok := l.Parcels[id]; !ok {
				return fmt.Errorf("%w: 退回请求号 %q 引用了不存在的包裹 %q", ErrCorrupt, req, id)
			}
		}
	}
	return nil
}

// Register 执行单件收件登记并将结果整体落盘。
func (s *Store) Register(id, station string, now time.Time) (*Parcel, error) {
	if _, ok := s.data.Parcels[id]; ok {
		return nil, fmt.Errorf("包裹编号 %q 已登记，不能重复登记", id)
	}
	p := &Parcel{
		ID:         id,
		Station:    station,
		Status:     statusInStation,
		Registered: now,
		Trail: []Event{{
			Op:      "收件",
			Station: station,
			Time:    now,
		}},
	}
	s.data.Parcels[id] = p
	if err := s.save(); err != nil {
		delete(s.data.Parcels, id) // 落盘失败：回滚内存变更，保留原有有效数据
		return nil, err
	}
	return p, nil
}

// Query 返回一件包裹的当前归属、状态和完整轨迹；不存在时返回 ErrNotFound。
func (s *Store) Query(id string) (*Parcel, error) {
	p, ok := s.data.Parcels[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return p, nil
}

// Handoff 提交一次多件包裹的站点交接。
//
// 首次提交：所有包裹必须已登记且当前归属源站点，否则整次失败、不作任何改动；
// 全部满足时一起改归目的站点并各追加一条交接记录，作为一个整体保存。
// 相同请求号且业务内容（源、目的、包裹集合，与顺序无关）相同：直接返回首次结果，
// 返回值 replayed 为 true；相同请求号但业务内容不同：报冲突，已有结果不变。
func (s *Store) Handoff(request, from, to string, parcels []string, now time.Time) (result *HandoffResult, replayed bool, err error) {
	if saved, ok := s.data.Handoffs[request]; ok {
		if saved.From == from && saved.To == to && sameSet(saved.Parcels, parcels) {
			return saved, true, nil
		}
		return nil, false, fmt.Errorf("请求号 %q 已用于一次不同的交接（源=%q 目的=%q），内容冲突", request, saved.From, saved.To)
	}

	// 首次提交：先做全部校验，任何一件不满足都整体失败。
	for _, id := range parcels {
		p, ok := s.data.Parcels[id]
		if !ok {
			return nil, false, fmt.Errorf("交接失败：包裹 %q 未登记，整次交接未执行", id)
		}
		if p.Station != from {
			return nil, false, fmt.Errorf("交接失败：包裹 %q 当前归属 %q，不属于源站点 %q，整次交接未执行", id, p.Station, from)
		}
	}

	res := &HandoffResult{
		Request: request,
		From:    from,
		To:      to,
		Parcels: append([]string(nil), parcels...),
		Time:    now,
	}
	s.data.Handoffs[request] = res

	// 记录旧值，落盘失败时整体回滚。
	prev := make(map[string]struct {
		station string
		trail   []Event
	}, len(parcels))
	for _, id := range parcels {
		p := s.data.Parcels[id]
		prev[id] = struct {
			station string
			trail   []Event
		}{p.Station, append([]Event(nil), p.Trail...)}
		p.Station = to
		p.Status = statusInStation
		p.Trail = append(p.Trail, Event{
			Op:      "交接",
			Station: to,
			Request: request,
			Time:    now,
		})
	}

	if err := s.save(); err != nil {
		delete(s.data.Handoffs, request)
		for id, old := range prev {
			p := s.data.Parcels[id]
			p.Station = old.station
			p.Trail = old.trail
		}
		return nil, false, err
	}
	return res, false, nil
}

// Return 按原交接整批退回：实物送回该交接的源站点，批次与站点取自原交接结果。
//
// 首次退回：原交接必须存在且尚未成功退回；批次中每件包裹必须仍归属原交接目的站、
// 状态为在站，且最后一条流转记录就是该原交接（请求重放不产生新流转，不影响判定）。
// 任一条件不满足则整批拒绝，不作任何改动。全部满足时整批改归原交接源站（仍为在站），
// 每件按批次顺序追加一条退回记录，原收件、交接轨迹保留不变。
// 相同退回请求号且原交接、原因相同：直接返回首次结果，replayed 为 true，
// 即使包裹之后又被交接也不重新检查；请求号相同但原交接或原因不同：报冲突。
// 退回请求号与交接请求号分属独立去重范围，互不影响；失败的首次退回不占用请求号。
func (s *Store) Return(request, handoffReq, reason string, now time.Time) (result *ReturnResult, replayed bool, err error) {
	if saved, ok := s.data.Returns[request]; ok {
		if saved.Handoff == handoffReq && saved.Reason == reason {
			return saved, true, nil
		}
		return nil, false, fmt.Errorf("退回请求号 %q 已用于一次不同的退回（原交接=%q 原因=%q），内容冲突",
			request, saved.Handoff, saved.Reason)
	}

	h, ok := s.data.Handoffs[handoffReq]
	if !ok {
		return nil, false, fmt.Errorf("退回失败：原交接请求号 %q 不存在", handoffReq)
	}
	if h.ReturnedBy != "" {
		return nil, false, fmt.Errorf("退回失败：原交接 %q 已成功退回（退回请求号 %q），不能再次退回", handoffReq, h.ReturnedBy)
	}

	// 先做全部校验，任何一件不满足都整批拒绝。
	for _, id := range h.Parcels {
		p, ok := s.data.Parcels[id]
		if !ok {
			return nil, false, fmt.Errorf("退回失败：包裹 %q 未登记，整批退回未执行", id)
		}
		if p.Station != h.To || p.Status != statusInStation {
			return nil, false, fmt.Errorf("退回失败：包裹 %q 当前归属 %q、状态 %q，不在原交接目的站 %q 在站，整批退回未执行",
				id, p.Station, p.Status, h.To)
		}
		last := p.Trail[len(p.Trail)-1]
		if last.Op != "交接" || last.Request != handoffReq {
			return nil, false, fmt.Errorf("退回失败：包裹 %q 在原交接 %q 之后又有新的流转，不能退回该交接，整批退回未执行",
				id, handoffReq)
		}
	}

	res := &ReturnResult{
		Request: request,
		Handoff: handoffReq,
		From:    h.To,
		To:      h.From,
		Reason:  reason,
		Parcels: append([]string(nil), h.Parcels...),
		Time:    now,
	}
	s.data.Returns[request] = res
	h.ReturnedBy = request

	// 记录旧值，落盘失败时整体回滚。
	prev := make(map[string]struct {
		station string
		trail   []Event
	}, len(h.Parcels))
	for _, id := range h.Parcels {
		p := s.data.Parcels[id]
		prev[id] = struct {
			station string
			trail   []Event
		}{p.Station, append([]Event(nil), p.Trail...)}
		p.Station = h.From
		p.Status = statusInStation
		p.Trail = append(p.Trail, Event{
			Op:         "退回",
			Station:    h.From,
			From:       h.To,
			Request:    request,
			RefRequest: handoffReq,
			Reason:     reason,
			Time:       now,
		})
	}

	if err := s.save(); err != nil {
		delete(s.data.Returns, request)
		h.ReturnedBy = ""
		for id, old := range prev {
			p := s.data.Parcels[id]
			p.Station = old.station
			p.Trail = old.trail
		}
		return nil, false, err
	}
	return res, false, nil
}

// sameSet 判断两个包裹列表作为集合是否相同（顺序不影响判定）。
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// save 将当前台账作为一个整体原子写入数据文件：先写临时文件并同步，
// 校验可重新解析后再原子替换，最后目录同步，避免保存失败留下部分业务变更。
func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("保存数据文件失败: %w", err)
	}
	buf, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("保存数据文件失败: %w", err)
	}
	buf = append(buf, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".parcelhub-*.tmp")
	if err != nil {
		return fmt.Errorf("保存数据文件失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(buf); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("保存数据文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("保存数据文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("保存数据文件失败: %w", err)
	}

	// 替换前确认临时文件自身可解析，杜绝把损坏内容写到正式位置。
	var check ledgerFile
	if err := json.Unmarshal(buf, &check); err != nil {
		cleanup()
		return fmt.Errorf("保存数据文件失败: 写入内容无效: %w", err)
	}

	if err := os.Rename(tmpName, s.path); err != nil {
		cleanup()
		return fmt.Errorf("保存数据文件失败: %w", err)
	}
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
