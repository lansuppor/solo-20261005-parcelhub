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

// 包裹状态。
const (
	// statusInStation 表示包裹当前在某站点（已收件、已完成交接或回执失败回到出发站）。
	statusInStation = "在站"
	// statusDelivering 表示包裹已随某个配送批次出站，正在配送中。
	statusDelivering = "配送中"
	// statusSigned 表示包裹已签收（回执结果为签收）。
	statusSigned = "已签收"
	// statusFrozen 表示在站包裹因异常被冻结：站点不变，禁止交接、配送出站与整批退回。
	statusFrozen = "异常冻结"
)

// 回执结果。
const (
	resultSigned = "签收"
	resultFailed = "失败"
)

// Event 是包裹轨迹中的一条记录，按提交顺序追加。
type Event struct {
	Op         string    `json:"op"`                   // 收件 | 交接 | 退回 | 出站 | 回执 | 冻结 | 解除冻结
	Station    string    `json:"station"`              // 与该操作有关的站点（退回时为退回目的站，即原交接源站；冻结/解除冻结为包裹所在站点）
	Time       time.Time `json:"time"`                 // 发生时间
	Request    string    `json:"request,omitempty"`    // 交接/退回/回执请求号（收件、出站记录为空）
	From       string    `json:"from,omitempty"`       // 退回源站（原交接目的站，仅退回记录有）
	Reason     string    `json:"reason,omitempty"`     // 退回原因（仅退回记录有）、回执失败原因（仅失败回执有）或冻结原因（仅冻结记录有）
	RefRequest string    `json:"refRequest,omitempty"` // 被退回的原交接请求号（仅退回记录有）
	Batch      string    `json:"batch,omitempty"`      // 配送批次号（仅出站、回执记录有）
	Courier    string    `json:"courier,omitempty"`    // 配送员（仅出站记录有）
	Result     string    `json:"result,omitempty"`     // 回执结果：签收 | 失败（仅回执记录有）
	// Incident 为冻结/解除冻结记录的异常单号；解除冻结时另外用 Request 存解除请求号、Note 存处理说明。
	Incident string `json:"incident,omitempty"`
	Note     string `json:"note,omitempty"`
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

// ReceiptEntry 是批次内一件包裹的回执记录。
type ReceiptEntry struct {
	Request string    `json:"request"`          // 回执请求号
	Result  string    `json:"result"`           // 签收 | 失败
	Reason  string    `json:"reason,omitempty"` // 失败原因（仅失败回执有）
	Time    time.Time `json:"time"`
}

// BatchResult 记录一次成功的配送批次出站，用于批次号去重、结果重放与批次查询。
// 成员按首次提交时给出的顺序保存，保存后不可修改；批次完成后批次号也不能复用。
type BatchResult struct {
	Batch    string                   `json:"batch"`
	Station  string                   `json:"station"`  // 出发站
	Courier  string                   `json:"courier"`  // 配送员
	Parcels  []string                 `json:"parcels"`  // 批次成员，按首次提交顺序
	Time     time.Time                `json:"time"`     // 出站时间
	Receipts map[string]*ReceiptEntry `json:"receipts"` // 逐件回执，按包裹编号索引（未回执的包裹不在其中）
}

// Done 报告批次是否已完成（全部成员均已回执）。
func (b *BatchResult) Done() bool { return len(b.Receipts) == len(b.Parcels) }

// ReceiptResult 记录一次成功的逐件回执，用于回执请求号去重与结果重放。
type ReceiptResult struct {
	Request string    `json:"request"` // 回执请求号
	Batch   string    `json:"batch"`   // 所属配送批次号
	Parcel  string    `json:"parcel"`  // 包裹编号
	Result  string    `json:"result"`  // 签收 | 失败
	Reason  string    `json:"reason,omitempty"`
	Time    time.Time `json:"time"`
}

// FreezeResult 记录一次成功的异常冻结，用于异常单号去重与结果重放。
// 异常单号标识一次异常，解除后也不能复用；失败的首次冻结不占用异常单号。
type FreezeResult struct {
	Incident string    `json:"incident"` // 异常单号
	Parcel   string    `json:"parcel"`   // 被冻结的包裹编号
	Station  string    `json:"station"`  // 冻结时包裹所在站点（冻结与解除期间均不变）
	Reason   string    `json:"reason"`   // 清理后的冻结原因
	Time     time.Time `json:"time"`     // 冻结时间
	// ReleasedBy 为解除该异常单的解除请求号（未解除为空）；解除后异常单永久保留、不得复用。
	ReleasedBy string `json:"releasedBy,omitempty"`
}

// UnfreezeResult 记录一次成功的解除冻结，用于解除请求号去重与结果重放。
// 解除请求号独立去重，可与异常单号、包裹号及其他业务编号同名；失败的首次解除不占用请求号。
type UnfreezeResult struct {
	Request  string    `json:"request"`  // 解除请求号
	Incident string    `json:"incident"` // 被解除的异常单号
	Parcel   string    `json:"parcel"`   // 对应包裹编号
	Note     string    `json:"note"`     // 清理后的处理说明
	Time     time.Time `json:"time"`     // 解除时间
}

// ledgerFile 是本地数据文件的磁盘结构。
type ledgerFile struct {
	Version   int                        `json:"version"`
	Parcels   map[string]*Parcel         `json:"parcels"`
	Handoffs  map[string]*HandoffResult  `json:"handoffs"`
	Returns   map[string]*ReturnResult   `json:"returns"`
	Batches   map[string]*BatchResult    `json:"batches"`
	Receipts  map[string]*ReceiptResult  `json:"receipts"`
	Freezes   map[string]*FreezeResult   `json:"freezes"`   // 以异常单号为键（含已解除的异常单，永久保留）
	Unfreezes map[string]*UnfreezeResult `json:"unfreezes"` // 以解除请求号为键
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
			s.data = ledgerFile{Version: 1, Parcels: map[string]*Parcel{}, Handoffs: map[string]*HandoffResult{},
				Returns: map[string]*ReturnResult{}, Batches: map[string]*BatchResult{}, Receipts: map[string]*ReceiptResult{},
				Freezes: map[string]*FreezeResult{}, Unfreezes: map[string]*UnfreezeResult{}}
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
	// 早期版本的数据文件没有 returns/batches/receipts/freeze 相关字段：按空表处理，无需手工修改。
	if s.data.Returns == nil {
		s.data.Returns = map[string]*ReturnResult{}
	}
	if s.data.Batches == nil {
		s.data.Batches = map[string]*BatchResult{}
	}
	if s.data.Receipts == nil {
		s.data.Receipts = map[string]*ReceiptResult{}
	}
	if s.data.Freezes == nil {
		s.data.Freezes = map[string]*FreezeResult{}
	}
	if s.data.Unfreezes == nil {
		s.data.Unfreezes = map[string]*UnfreezeResult{}
	}
	for _, b := range s.data.Batches {
		if b != nil && b.Receipts == nil {
			b.Receipts = map[string]*ReceiptEntry{}
		}
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
		switch p.Status {
		case statusInStation, statusDelivering, statusSigned, statusFrozen:
		default:
			return fmt.Errorf("%w: 包裹 %q 的状态 %q 不受支持", ErrCorrupt, id, p.Status)
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
			if e.Op == "出站" {
				if e.Batch == "" || e.Courier == "" {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条出站轨迹缺少批次号或配送员", ErrCorrupt, id, i+1)
				}
				if _, ok := l.Batches[e.Batch]; !ok {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条出站轨迹引用了不存在的批次 %q", ErrCorrupt, id, i+1, e.Batch)
				}
			}
			if e.Op == "回执" {
				if e.Batch == "" || e.Request == "" || (e.Result != resultSigned && e.Result != resultFailed) {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条回执轨迹缺少批次号、请求号或结果无效", ErrCorrupt, id, i+1)
				}
				if e.Result == resultFailed && e.Reason == "" {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条失败回执轨迹缺少原因", ErrCorrupt, id, i+1)
				}
				if _, ok := l.Batches[e.Batch]; !ok {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条回执轨迹引用了不存在的批次 %q", ErrCorrupt, id, i+1, e.Batch)
				}
			}
			if e.Op == "冻结" {
				if e.Incident == "" || e.Reason == "" {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条冻结轨迹缺少异常单号或原因", ErrCorrupt, id, i+1)
				}
				f, ok := l.Freezes[e.Incident]
				if !ok {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条冻结轨迹引用了不存在的异常单 %q", ErrCorrupt, id, i+1, e.Incident)
				}
				if f.Parcel != id || f.Reason != e.Reason || f.Station != e.Station || !f.Time.Equal(e.Time) {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条冻结轨迹与异常单 %q 记录不一致", ErrCorrupt, id, i+1, e.Incident)
				}
			}
			if e.Op == "解除冻结" {
				if e.Incident == "" || e.Request == "" || e.Note == "" {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条解除冻结轨迹缺少异常单号、解除请求号或处理说明", ErrCorrupt, id, i+1)
				}
				f, ok := l.Freezes[e.Incident]
				if !ok {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条解除冻结轨迹引用了不存在的异常单 %q", ErrCorrupt, id, i+1, e.Incident)
				}
				if f.Parcel != id || f.Station != e.Station || f.ReleasedBy != e.Request {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条解除冻结轨迹与异常单 %q 不匹配", ErrCorrupt, id, i+1, e.Incident)
				}
				u, ok := l.Unfreezes[e.Request]
				if !ok || u.Incident != e.Incident || u.Parcel != id || u.Note != e.Note || !u.Time.Equal(e.Time) {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条解除冻结轨迹与解除请求 %q 记录不一致", ErrCorrupt, id, i+1, e.Request)
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
	for id, b := range l.Batches {
		if b == nil || id != b.Batch || b.Station == "" || b.Courier == "" || len(b.Parcels) == 0 || b.Time.IsZero() {
			return fmt.Errorf("%w: 批次号 %q 的出站结果不完整", ErrCorrupt, id)
		}
		seen := make(map[string]bool, len(b.Parcels))
		for _, pid := range b.Parcels {
			if seen[pid] {
				return fmt.Errorf("%w: 批次 %q 的成员 %q 重复", ErrCorrupt, id, pid)
			}
			seen[pid] = true
			if _, ok := l.Parcels[pid]; !ok {
				return fmt.Errorf("%w: 批次 %q 引用了不存在的包裹 %q", ErrCorrupt, id, pid)
			}
		}
		for pid, e := range b.Receipts {
			if e == nil || !seen[pid] || e.Request == "" || e.Time.IsZero() ||
				(e.Result != resultSigned && e.Result != resultFailed) ||
				(e.Result == resultFailed && e.Reason == "") || (e.Result == resultSigned && e.Reason != "") {
				return fmt.Errorf("%w: 批次 %q 中包裹 %q 的回执记录不完整", ErrCorrupt, id, pid)
			}
			rc, ok := l.Receipts[e.Request]
			if !ok || rc.Batch != id || rc.Parcel != pid {
				return fmt.Errorf("%w: 批次 %q 中包裹 %q 的回执请求号 %q 无法对应", ErrCorrupt, id, pid, e.Request)
			}
		}
	}
	for req, rc := range l.Receipts {
		if rc == nil || req != rc.Request || rc.Batch == "" || rc.Parcel == "" || rc.Time.IsZero() ||
			(rc.Result != resultSigned && rc.Result != resultFailed) ||
			(rc.Result == resultFailed && rc.Reason == "") || (rc.Result == resultSigned && rc.Reason != "") {
			return fmt.Errorf("%w: 回执请求号 %q 的回执结果不完整", ErrCorrupt, req)
		}
		b, ok := l.Batches[rc.Batch]
		if !ok {
			return fmt.Errorf("%w: 回执请求号 %q 引用了不存在的批次 %q", ErrCorrupt, req, rc.Batch)
		}
		e, ok := b.Receipts[rc.Parcel]
		if !ok || e.Request != req || e.Result != rc.Result || e.Reason != rc.Reason || !e.Time.Equal(rc.Time) {
			return fmt.Errorf("%w: 回执请求号 %q 与批次 %q 的回执记录不一致", ErrCorrupt, req, rc.Batch)
		}
	}
	// 冻结/解除冻结：先遍历轨迹核对冻结与解除必须成对出现、顺序正确，
	// 再核对两张结果表与包裹当前状态的对应关系。
	openIncident := make(map[string]string) // 包裹 -> 当前未解除的异常单号（由轨迹推导）
	for id, p := range l.Parcels {
		var open string
		for i, e := range p.Trail {
			switch e.Op {
			case "冻结":
				if open != "" {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条冻结时异常单 %q 尚未解除，一件包裹同时只能有一张未解除异常单",
						ErrCorrupt, id, i+1, open)
				}
				open = e.Incident
			case "解除冻结":
				if open != e.Incident {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条解除的异常单 %q 与当前冻结 %q 不匹配",
						ErrCorrupt, id, i+1, e.Incident, open)
				}
				open = ""
			}
		}
		if p.Status == statusFrozen && open == "" {
			return fmt.Errorf("%w: 包裹 %q 当前为异常冻结，但轨迹中没有未解除的冻结记录", ErrCorrupt, id)
		}
		if p.Status != statusFrozen && open != "" {
			return fmt.Errorf("%w: 包裹 %q 当前状态为 %q，但异常单 %q 尚未解除", ErrCorrupt, id, p.Status, open)
		}
		if open != "" {
			openIncident[id] = open
		}
	}
	for no, f := range l.Freezes {
		if f == nil || no != f.Incident || f.Parcel == "" || f.Station == "" || f.Reason == "" || f.Time.IsZero() {
			return fmt.Errorf("%w: 异常单号 %q 的冻结结果不完整", ErrCorrupt, no)
		}
		p, ok := l.Parcels[f.Parcel]
		if !ok {
			return fmt.Errorf("%w: 异常单 %q 引用了不存在的包裹 %q", ErrCorrupt, no, f.Parcel)
		}
		if f.ReleasedBy == "" && p.Station != f.Station {
			return fmt.Errorf("%w: 异常单 %q 记录的站点 %q 与包裹 %q 当前站点 %q 不一致",
				ErrCorrupt, no, f.Station, f.Parcel, p.Station)
		}
		if f.ReleasedBy != "" {
			u, ok := l.Unfreezes[f.ReleasedBy]
			if !ok || u.Incident != no || u.Parcel != f.Parcel {
				return fmt.Errorf("%w: 异常单 %q 标记的解除请求号 %q 无法对应", ErrCorrupt, no, f.ReleasedBy)
			}
		}
	}
	activeByParcel := make(map[string]string)
	for req, u := range l.Unfreezes {
		if u == nil || req != u.Request || u.Incident == "" || u.Parcel == "" || u.Note == "" || u.Time.IsZero() {
			return fmt.Errorf("%w: 解除请求号 %q 的解除结果不完整", ErrCorrupt, req)
		}
		f, ok := l.Freezes[u.Incident]
		if !ok {
			return fmt.Errorf("%w: 解除请求号 %q 引用了不存在的异常单 %q", ErrCorrupt, req, u.Incident)
		}
		if f.Parcel != u.Parcel {
			return fmt.Errorf("%w: 解除请求号 %q 与异常单 %q 对应的包裹不一致", ErrCorrupt, req, u.Incident)
		}
		if f.ReleasedBy != req {
			return fmt.Errorf("%w: 解除请求号 %q 与异常单 %q 的解除标记不一致", ErrCorrupt, req, u.Incident)
		}
	}
	for no, f := range l.Freezes {
		if f.ReleasedBy != "" {
			continue
		}
		if other, dup := activeByParcel[f.Parcel]; dup {
			return fmt.Errorf("%w: 包裹 %q 同时存在两张未解除异常单 %q 与 %q", ErrCorrupt, f.Parcel, other, no)
		}
		activeByParcel[f.Parcel] = no
		if openIncident[f.Parcel] != no {
			return fmt.Errorf("%w: 包裹 %q 当前冻结对应的异常单应为 %q，与未解除异常单 %q 不匹配",
				ErrCorrupt, f.Parcel, openIncident[f.Parcel], no)
		}
		if p := l.Parcels[f.Parcel]; p.Status != statusFrozen {
			return fmt.Errorf("%w: 异常单 %q 未解除，但包裹 %q 当前状态为 %q", ErrCorrupt, no, f.Parcel, p.Status)
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

// ActiveFreeze 返回一件包裹当前未解除异常单的冻结结果；包裹不存在或当前未冻结时返回 nil。
func (s *Store) ActiveFreeze(id string) *FreezeResult {
	p, ok := s.data.Parcels[id]
	if !ok {
		return nil
	}
	if no := activeFreeze(p); no != "" {
		return s.data.Freezes[no]
	}
	return nil
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
		if inc := activeFreeze(p); inc != "" {
			return nil, false, fmt.Errorf("交接失败：包裹 %q 已被异常单 %q 冻结，冻结期间整批交接拒绝，整次交接未执行", id, inc)
		}
		if p.Status != statusInStation {
			return nil, false, fmt.Errorf("交接失败：包裹 %q 当前状态为 %q，不是在站，整次交接未执行", id, p.Status)
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
		if inc := activeFreeze(p); inc != "" {
			return nil, false, fmt.Errorf("退回失败：包裹 %q 已被异常单 %q 冻结，冻结期间整批退回拒绝，整批退回未执行", id, inc)
		}
		if p.Station != h.To || p.Status != statusInStation {
			return nil, false, fmt.Errorf("退回失败：包裹 %q 当前归属 %q、状态 %q，不在原交接目的站 %q 在站，整批退回未执行",
				id, p.Station, p.Status, h.To)
		}
		last := lastFlowEvent(p.Trail)
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

// Dispatch 提交一次配送批次出站。
//
// 首次出站：所有包裹必须已登记、当前归属出发站且状态为在站，否则整批拒绝、不作任何改动；
// 全部满足时整批转为“配送中”（站点仍记出发站），每件追加一条含批次、配送员和时间的
// 出站记录，批次成员按首次提交顺序保存且不可修改。批次号标识一次配送，完成后也不能复用：
// 相同批次号且站点、配送员、包裹集合（与顺序无关）相同，直接返回首次结果，replayed 为 true；
// 批次号相同但内容不同报冲突；失败的首次出站不占用批次号。
func (s *Store) Dispatch(batch, station, courier string, parcels []string, now time.Time) (result *BatchResult, replayed bool, err error) {
	if saved, ok := s.data.Batches[batch]; ok {
		if saved.Station == station && saved.Courier == courier && sameSet(saved.Parcels, parcels) {
			return saved, true, nil
		}
		return nil, false, fmt.Errorf("批次号 %q 已用于一次不同的出站（出发站=%q 配送员=%q），内容冲突", batch, saved.Station, saved.Courier)
	}

	// 首次出站：先做全部校验，任何一件不满足都整批拒绝。
	for _, id := range parcels {
		p, ok := s.data.Parcels[id]
		if !ok {
			return nil, false, fmt.Errorf("出站失败：包裹 %q 未登记，整批出站未执行", id)
		}
		if p.Station != station {
			return nil, false, fmt.Errorf("出站失败：包裹 %q 当前归属 %q，不在出发站 %q，整批出站未执行", id, p.Station, station)
		}
		if inc := activeFreeze(p); inc != "" {
			return nil, false, fmt.Errorf("出站失败：包裹 %q 已被异常单 %q 冻结，冻结期间整批出站拒绝，整批出站未执行", id, inc)
		}
		if p.Status != statusInStation {
			return nil, false, fmt.Errorf("出站失败：包裹 %q 当前状态为 %q，不是在站，整批出站未执行", id, p.Status)
		}
	}

	res := &BatchResult{
		Batch:    batch,
		Station:  station,
		Courier:  courier,
		Parcels:  append([]string(nil), parcels...),
		Time:     now,
		Receipts: map[string]*ReceiptEntry{},
	}
	s.data.Batches[batch] = res

	// 记录旧值，落盘失败时整体回滚。
	prev := make(map[string]struct {
		status string
		trail  []Event
	}, len(parcels))
	for _, id := range parcels {
		p := s.data.Parcels[id]
		prev[id] = struct {
			status string
			trail  []Event
		}{p.Status, append([]Event(nil), p.Trail...)}
		p.Status = statusDelivering // 站点不变，仍记出发站
		p.Trail = append(p.Trail, Event{
			Op:      "出站",
			Station: station,
			Batch:   batch,
			Courier: courier,
			Time:    now,
		})
	}

	if err := s.save(); err != nil {
		delete(s.data.Batches, batch)
		for id, old := range prev {
			p := s.data.Parcels[id]
			p.Status = old.status
			p.Trail = old.trail
		}
		return nil, false, err
	}
	return res, false, nil
}

// Receipt 提交一件包裹在某个配送批次下的回执。
//
// 首次回执：批次必须存在，包裹必须属于该批次、尚未在该批次回执，且当前仍在该批次
// 配送中；任一不满足则拒绝，不作任何改动。签收后状态为“已签收”；失败表示实物已回到
// 出发站，恢复“在站”，可加入新的配送批次。两种结果都保留站点并追加一条含批次、结果、
// 原因（失败时）、请求号和时间的回执记录；全部成员回执后批次自动完成。
// 相同回执请求号且批次、包裹、结果、清理后的原因相同：直接返回首次结果，replayed 为
// true，不重新检查当前状态，即使失败包裹已进入新批次；请求号相同但内容不同报冲突。
// 回执请求号与批次号、包裹编号、交接及退回请求号分属独立去重范围，允许同名；
// 失败的首次回执不占用请求号。
func (s *Store) Receipt(request, batch, parcel, result, reason string, now time.Time) (res *ReceiptResult, replayed bool, err error) {
	if saved, ok := s.data.Receipts[request]; ok {
		if saved.Batch == batch && saved.Parcel == parcel && saved.Result == result && saved.Reason == reason {
			return saved, true, nil
		}
		return nil, false, fmt.Errorf("回执请求号 %q 已用于一次不同的回执（批次=%q 包裹=%q 结果=%q），内容冲突",
			request, saved.Batch, saved.Parcel, saved.Result)
	}

	b, ok := s.data.Batches[batch]
	if !ok {
		return nil, false, fmt.Errorf("回执失败：批次号 %q 不存在", batch)
	}
	member := false
	for _, id := range b.Parcels {
		if id == parcel {
			member = true
			break
		}
	}
	if !member {
		return nil, false, fmt.Errorf("回执失败：包裹 %q 不属于批次 %q", parcel, batch)
	}
	if _, done := b.Receipts[parcel]; done {
		return nil, false, fmt.Errorf("回执失败：包裹 %q 在批次 %q 中已回执，每件在一个批次只能成功回执一次", parcel, batch)
	}
	p := s.data.Parcels[parcel] // 批次成员必已登记（载入校验保证）
	if p.Status != statusDelivering || currentBatch(p) != batch {
		return nil, false, fmt.Errorf("回执失败：包裹 %q 当前状态为 %q，不在批次 %q 配送中", parcel, p.Status, batch)
	}

	res = &ReceiptResult{
		Request: request,
		Batch:   batch,
		Parcel:  parcel,
		Result:  result,
		Reason:  reason,
		Time:    now,
	}
	entry := &ReceiptEntry{Request: request, Result: result, Reason: reason, Time: now}

	// 记录旧值，落盘失败时整体回滚。
	oldStatus, oldStation := p.Status, p.Station
	oldTrail := append([]Event(nil), p.Trail...)

	if result == resultSigned {
		p.Status = statusSigned // 站点保留不变
	} else {
		p.Status = statusInStation // 实物已回到出发站，可加入新的配送批次
		p.Station = b.Station
	}
	p.Trail = append(p.Trail, Event{
		Op:      "回执",
		Station: p.Station,
		Batch:   batch,
		Result:  result,
		Reason:  reason,
		Request: request,
		Time:    now,
	})
	b.Receipts[parcel] = entry
	s.data.Receipts[request] = res

	if err := s.save(); err != nil {
		delete(s.data.Receipts, request)
		delete(b.Receipts, parcel)
		p.Status, p.Station, p.Trail = oldStatus, oldStation, oldTrail
		return nil, false, err
	}
	return res, false, nil
}

// Freeze 对一件在站包裹登记异常冻结。
//
// 首次冻结：包裹必须已登记、当前归属指定站点且状态为在站；配送中、已签收或已冻结的
// 包裹不能冻结。成功后站点不变，状态转为异常冻结，追加一条含异常单号、原因、站点和
// 时间的冻结记录。一件包裹同时只能有一张未解除异常单。
// 冻结只是管理记录，不移动实物、不算新流转。
// 异常单号标识一次异常，解除后也不能复用：相同异常单号且包裹、站点、清理后的原因
// 相同，直接返回首次冻结结果与时间，replayed 为 true，不再次冻结（即使该异常单已解除、
// 包裹已发生新的流转，也不检查当前状态）；异常单号相同但内容不同报冲突；
// 失败的首次冻结不占用异常单号。异常单号与包裹编号及其他业务编号分属独立去重范围。
func (s *Store) Freeze(incident, parcel, station, reason string, now time.Time) (res *FreezeResult, replayed bool, err error) {
	if saved, ok := s.data.Freezes[incident]; ok {
		if saved.Parcel == parcel && saved.Station == station && saved.Reason == reason {
			return saved, true, nil
		}
		return nil, false, fmt.Errorf("异常单号 %q 已用于一次不同的冻结（包裹=%q 站点=%q 原因=%q），内容冲突",
			incident, saved.Parcel, saved.Station, saved.Reason)
	}

	p, ok := s.data.Parcels[parcel]
	if !ok {
		return nil, false, fmt.Errorf("冻结失败：包裹 %q 未登记，冻结未执行", parcel)
	}
	if p.Station != station {
		return nil, false, fmt.Errorf("冻结失败：包裹 %q 当前归属 %q，不在指定站点 %q，冻结未执行", parcel, p.Station, station)
	}
	if p.Status != statusInStation {
		return nil, false, fmt.Errorf("冻结失败：包裹 %q 当前状态为 %q，仅在站包裹可以冻结，冻结未执行", parcel, p.Status)
	}

	res = &FreezeResult{
		Incident: incident,
		Parcel:   parcel,
		Station:  station,
		Reason:   reason,
		Time:     now,
	}
	s.data.Freezes[incident] = res

	// 记录旧值，落盘失败时整体回滚。
	oldStatus := p.Status
	oldTrail := append([]Event(nil), p.Trail...)
	p.Status = statusFrozen // 站点不变
	p.Trail = append(p.Trail, Event{
		Op:       "冻结",
		Station:  station,
		Incident: incident,
		Reason:   reason,
		Time:     now,
	})

	if err := s.save(); err != nil {
		delete(s.data.Freezes, incident)
		p.Status, p.Trail = oldStatus, oldTrail
		return nil, false, err
	}
	return res, false, nil
}

// Unfreeze 解除一件包裹当前的异常冻结。
//
// 首次解除：异常单必须存在且尚未解除，并且仍是该包裹当前冻结对应的异常单；
// 异常单不存在、已解除或对应关系不符均拒绝。成功后包裹在原站恢复在站，原异常单
// 永久标记为已解除（异常单号不得复用），追加一条含异常单号、解除请求号、处理说明及
// 时间的解除记录，原冻结记录不删改；之后可用新异常单再次冻结。
// 解除只是管理记录，不移动实物、不算新流转。
// 解除请求号独立去重，可与异常单号、包裹号及其他业务编号同名：相同解除请求号且异常单、
// 处理说明相同，直接返回首次解除结果与时间，replayed 为 true，不检查当前状态，也不影响
// 后来新建的异常单或后续流转；请求号相同但内容不同报冲突；失败的首次解除不占用请求号。
func (s *Store) Unfreeze(request, incident, note string, now time.Time) (res *UnfreezeResult, replayed bool, err error) {
	if saved, ok := s.data.Unfreezes[request]; ok {
		if saved.Incident == incident && saved.Note == note {
			return saved, true, nil
		}
		return nil, false, fmt.Errorf("解除请求号 %q 已用于一次不同的解除（异常单=%q 处理说明=%q），内容冲突",
			request, saved.Incident, saved.Note)
	}

	f, ok := s.data.Freezes[incident]
	if !ok {
		return nil, false, fmt.Errorf("解除失败：异常单号 %q 不存在，解除未执行", incident)
	}
	if f.ReleasedBy != "" {
		return nil, false, fmt.Errorf("解除失败：异常单 %q 已解除（解除请求号 %q），不能重复解除", incident, f.ReleasedBy)
	}
	p, ok := s.data.Parcels[f.Parcel]
	if !ok {
		return nil, false, fmt.Errorf("解除失败：异常单 %q 对应的包裹 %q 不存在，解除未执行", incident, f.Parcel)
	}
	if p.Status != statusFrozen || activeFreeze(p) != incident {
		return nil, false, fmt.Errorf("解除失败：异常单 %q 不是包裹 %q 当前冻结对应的异常单（当前异常单 %q），解除未执行",
			incident, f.Parcel, activeFreeze(p))
	}

	res = &UnfreezeResult{
		Request:  request,
		Incident: incident,
		Parcel:   f.Parcel,
		Note:     note,
		Time:     now,
	}
	s.data.Unfreezes[request] = res
	f.ReleasedBy = request

	// 记录旧值，落盘失败时整体回滚。
	oldStatus := p.Status
	oldTrail := append([]Event(nil), p.Trail...)
	p.Status = statusInStation // 在原站恢复在站，站点不变
	p.Trail = append(p.Trail, Event{
		Op:       "解除冻结",
		Station:  f.Station,
		Incident: incident,
		Request:  request,
		Note:     note,
		Time:     now,
	})

	if err := s.save(); err != nil {
		delete(s.data.Unfreezes, request)
		f.ReleasedBy = ""
		p.Status, p.Trail = oldStatus, oldTrail
		return nil, false, err
	}
	return res, false, nil
}

// currentBatch 返回包裹当前配送中的批次号（最后一条出站记录的批次）；不在配送中返回空。
func currentBatch(p *Parcel) string {
	for i := len(p.Trail) - 1; i >= 0; i-- {
		if p.Trail[i].Op == "出站" {
			return p.Trail[i].Batch
		}
	}
	return ""
}

// activeFreeze 返回包裹当前未解除冻结对应的异常单号；未冻结返回空。
// 冻结状态与轨迹成对追加，故当前状态为异常冻结时最后一条冻结记录即为当前异常单。
func activeFreeze(p *Parcel) string {
	if p.Status != statusFrozen {
		return ""
	}
	for i := len(p.Trail) - 1; i >= 0; i-- {
		if p.Trail[i].Op == "冻结" {
			return p.Trail[i].Incident
		}
	}
	return ""
}

// lastFlowEvent 返回轨迹中最后一条真实流转记录（交接/退回/出站/回执）。
// 冻结与解除只是管理记录，不算新流转，判定旧交接可否退回时须跳过它们。
func lastFlowEvent(trail []Event) Event {
	for i := len(trail) - 1; i >= 0; i-- {
		switch trail[i].Op {
		case "冻结", "解除冻结":
			continue
		default:
			return trail[i]
		}
	}
	return Event{}
}

// BatchQuery 返回一个配送批次的出站结果与逐件回执进度；批次不存在时报错。
func (s *Store) BatchQuery(batch string) (*BatchResult, error) {
	b, ok := s.data.Batches[batch]
	if !ok {
		return nil, fmt.Errorf("批次 %q 不存在", batch)
	}
	return b, nil
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
