package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
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
	// statusInTransit 表示包裹已随某张运输单离开源站、尚未在目的站被接收：
	// 归属站点暂记源站，禁止交接、退回、配送出站与冻结。
	statusInTransit = "站间在途"
)

// 回执结果。
const (
	resultSigned = "签收"
	resultFailed = "失败"
)

// Event 是包裹轨迹中的一条记录，按提交顺序追加。
type Event struct {
	Op         string    `json:"op"`                   // 收件 | 交接 | 退回 | 出站 | 回执 | 冻结 | 解除冻结 | 收回 | 续接 | 撤销回执 | 发运 | 接收
	Station    string    `json:"station"`              // 与该操作有关的站点（退回时为退回目的站，即原交接源站；冻结/解除冻结为包裹所在站点；收回/续接/撤销回执为批次出发站；发运时为源站，接收时为目的站）
	Time       time.Time `json:"time"`                 // 发生时间
	Request    string    `json:"request,omitempty"`    // 交接/退回/回执/收回/续接/撤销/接收请求号（收件、出站、发运记录为空）
	From       string    `json:"from,omitempty"`       // 退回源站（原交接目的站，仅退回记录有）或发运/接收源站（仅发运、接收记录有）
	To         string    `json:"to,omitempty"`         // 发运/接收目的站（仅发运、接收记录有）
	Shipment   string    `json:"shipment,omitempty"`   // 运输单号（仅发运、接收记录有）
	Reason     string    `json:"reason,omitempty"`     // 退回原因（仅退回记录有）、回执失败原因（仅失败回执有）、冻结原因（仅冻结记录有）、中止原因（仅收回记录有）、续接原因（仅续接记录有）或撤销原因（仅撤销回执记录有）
	RefRequest string    `json:"refRequest,omitempty"` // 被退回的原交接请求号（仅退回记录有）或被撤销的原回执请求号（仅撤销回执记录有）
	Batch      string    `json:"batch,omitempty"`      // 配送批次号（仅出站、回执、收回、续接、撤销回执记录有；续接时为新批次号）
	FromBatch  string    `json:"fromBatch,omitempty"`  // 续接原批次号（仅续接记录有）
	Courier    string    `json:"courier,omitempty"`    // 配送员（仅出站、续接记录有；续接时为新配送员）
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

// ReceiptEntry 是批次内一件包裹的当前回执记录（被新回执取代的已撤销条目由
// 台账级 Receipts 结果表与撤销记录共同保留）。
type ReceiptEntry struct {
	Request string    `json:"request"`          // 回执请求号
	Result  string    `json:"result"`           // 签收 | 失败
	Reason  string    `json:"reason,omitempty"` // 失败原因（仅失败回执有）
	Time    time.Time `json:"time"`
	// RevokedBy 为撤销该回执的撤销请求号（未撤销为空）；撤销后该条不再计入有效回执。
	RevokedBy string `json:"revokedBy,omitempty"`
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
	// AbortedBy 为中止该批次的中止请求号（未中止为空）；中止永久生效，批次不得复用。
	AbortedBy string `json:"abortedBy,omitempty"`
	// TransferredBy 为把该批次关闭为“已转交”的续接请求号（未转交为空）；
	// 转交永久生效，与中止互斥，批次不得复用。
	TransferredBy string `json:"transferredBy,omitempty"`
	// RelayedFrom 与 RelayRequest 仅对续接创建的新批次有效：分别记录来源批次号
	// 与创建本批次的续接请求号（普通出站批次为空）。
	RelayedFrom  string `json:"relayedFrom,omitempty"`
	RelayRequest string `json:"relayRequest,omitempty"`
}

// Effective 返回批次当前有效（未撤销）的回执数量。
func (b *BatchResult) Effective() int {
	n := 0
	for _, e := range b.Receipts {
		if e != nil && e.RevokedBy == "" {
			n++
		}
	}
	return n
}

// Done 报告批次是否已完成（全部成员均有未撤销的有效回执）。
func (b *BatchResult) Done() bool { return b.Effective() == len(b.Parcels) }

// ReceiptResult 记录一次成功的逐件回执，用于回执请求号去重与结果重放。
// 回执结果永久保留：即使之后被撤销，回执请求号也不删除、不释放。
type ReceiptResult struct {
	Request string    `json:"request"` // 回执请求号
	Batch   string    `json:"batch"`   // 所属配送批次号
	Parcel  string    `json:"parcel"`  // 包裹编号
	Result  string    `json:"result"`  // 签收 | 失败
	Reason  string    `json:"reason,omitempty"`
	Time    time.Time `json:"time"`
	// RevokedBy 为撤销该回执的撤销请求号（未撤销为空）；每条回执只能被撤销一次。
	RevokedBy string `json:"revokedBy,omitempty"`
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

// AbortResult 记录一次成功的配送批次中止，用于中止请求号去重与结果重放。
// 中止表示配送员已将该批次全部尚未回执的包裹实物收回出发站：批次永久关闭，
// 原成员顺序、配送员与出站时间保留不变，已回执成员及其回执完全保留。
// 中止请求号独立去重，可与批次号、包裹号及其他业务编号同名；失败的首次中止不占用请求号。
type AbortResult struct {
	Request string    `json:"request"` // 中止请求号
	Batch   string    `json:"batch"`   // 被中止的配送批次号
	Station string    `json:"station"` // 出发站（收回目的站，取自原批次）
	Reason  string    `json:"reason"`  // 清理后的中止原因
	Parcels []string  `json:"parcels"` // 首次收回的未回执包裹，按原批次成员顺序
	Time    time.Time `json:"time"`    // 中止时间
}

// TransferResult 记录一次成功的配送途中整批续接，用于续接请求号去重与结果重放。
// 续接表示原配送员把原批次全部尚未回执的包裹在配送途中交给另一配送员：
// 新批次按原顺序接纳全部未回执件继续配送，包裹保持“配送中”及原站点，
// 原批次永久关闭为“已转交”。续接请求号独立去重，可与批次号、包裹号及其他
// 业务编号同名；失败的首次续接不占用请求号。
type TransferResult struct {
	Request     string    `json:"request"`     // 续接请求号
	FromBatch   string    `json:"fromBatch"`   // 原批次号
	ToBatch     string    `json:"toBatch"`     // 新批次号
	Station     string    `json:"station"`     // 出发站（沿用原批次）
	FromCourier string    `json:"fromCourier"` // 原配送员（取自原批次）
	ToCourier   string    `json:"toCourier"`   // 新配送员
	Reason      string    `json:"reason"`      // 清理后的续接原因
	Parcels     []string  `json:"parcels"`     // 首次转交集合（原批次未回执成员，按原成员顺序）
	Time        time.Time `json:"time"`        // 接手时间
}

// RevokeResult 记录一次成功的误录回执撤销，用于撤销请求号去重与结果重放。
// 撤销只更正台账：经核实包裹实际仍由原配送员配送，撤销错误的签收或失败登记，
// 不收回实物。原回执结果与轨迹永久保留并标记已撤销，回执请求号不删除也不释放。
// 撤销请求号独立去重，可与批次号、包裹号、回执请求号及其他业务编号同名；
// 失败的首次撤销不占用请求号。
type RevokeResult struct {
	Request string    `json:"request"` // 撤销请求号
	Receipt string    `json:"receipt"` // 被撤销的原回执请求号
	Batch   string    `json:"batch"`   // 所属配送批次号（取自原回执，不可另选）
	Parcel  string    `json:"parcel"`  // 包裹编号（取自原回执，不可另选）
	Reason  string    `json:"reason"`  // 清理后的撤销原因
	Time    time.Time `json:"time"`    // 撤销时间
}

// ShipmentResult 记录一次成功的站间发运，用于运输单号去重、结果重放与运输单查询。
// 发运表示整单包裹已离开源站、尚未到达目的站：整单转为“站间在途”，归属站点暂记
// 源站，成员按首次提交顺序保存，保存后不可修改。运输单号独立于已有各类编号，
// 接收后也不释放；失败的首次发运不占用运输单号。
type ShipmentResult struct {
	Shipment string    `json:"shipment"` // 运输单号
	From     string    `json:"from"`     // 源站
	To       string    `json:"to"`       // 目的站
	Parcels  []string  `json:"parcels"`  // 成员，按首次提交顺序
	Time     time.Time `json:"time"`     // 发运时间
	// ReceivedBy 为使该运输单达到全部接收的接收请求号（尚未全部接收为空）；
	// 全部接收永久生效，运输单号不释放。分批到站时可能经历多次接收，
	// 每次接收记录于接收结果表，是否全部接收按各次接收事实统计。
	ReceivedBy string `json:"receivedBy,omitempty"`
}

// ReceiveResult 记录一次成功的到站接收，用于接收请求号去重与结果重放。
// 接收表示运输单的一批成员已到达目的站：按发运保存顺序将本次成员改归目的站、
// 恢复在站；全部成员到齐后运输单永久标记为已接收。接收请求号与运输单号及已有
// 各类编号分属独立去重范围，允许同名；失败的首次接收不占用请求号。
type ReceiveResult struct {
	Request  string    `json:"request"`  // 接收请求号
	Shipment string    `json:"shipment"` // 被接收的运输单号
	Station  string    `json:"station"`  // 接收站点（即运输单目的站）
	Parcels  []string  `json:"parcels"`  // 本次接收的成员，按发运保存顺序
	Time     time.Time `json:"time"`     // 接收时间
	// Explicit 为 true 表示本次接收显式选择了包裹集合；false 表示不选成员、
	// 接收当时全部尚未接收件（旧版整单接收记录没有该字段，按不选成员处理）。
	Explicit bool `json:"explicit,omitempty"`
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
	Aborts    map[string]*AbortResult    `json:"aborts"`    // 以中止请求号为键
	Transfers map[string]*TransferResult `json:"transfers"` // 以续接请求号为键
	Revokes   map[string]*RevokeResult   `json:"revokes"`   // 以撤销请求号为键
	Shipments map[string]*ShipmentResult `json:"shipments"` // 以运输单号为键（含已接收的运输单，永久保留）
	Receives  map[string]*ReceiveResult  `json:"receives"`  // 以接收请求号为键
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
				Freezes: map[string]*FreezeResult{}, Unfreezes: map[string]*UnfreezeResult{}, Aborts: map[string]*AbortResult{},
				Transfers: map[string]*TransferResult{}, Revokes: map[string]*RevokeResult{}, Shipments: map[string]*ShipmentResult{},
				Receives: map[string]*ReceiveResult{}}
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
	// 早期版本的数据文件没有 returns/batches/receipts/freeze/abort/revoke 相关字段：按空表处理，无需手工修改。
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
	if s.data.Aborts == nil {
		s.data.Aborts = map[string]*AbortResult{}
	}
	if s.data.Transfers == nil {
		s.data.Transfers = map[string]*TransferResult{}
	}
	if s.data.Revokes == nil {
		s.data.Revokes = map[string]*RevokeResult{}
	}
	// 早期版本的数据文件没有 shipments/receives 字段：按空表处理，无需手工修改。
	if s.data.Shipments == nil {
		s.data.Shipments = map[string]*ShipmentResult{}
	}
	if s.data.Receives == nil {
		s.data.Receives = map[string]*ReceiveResult{}
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
		case statusInStation, statusDelivering, statusSigned, statusFrozen, statusInTransit:
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
			if e.Op == "撤销回执" {
				if e.Batch == "" || e.Request == "" || e.RefRequest == "" || e.Reason == "" {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条撤销回执轨迹缺少批次号、撤销请求号、原回执请求号或原因", ErrCorrupt, id, i+1)
				}
				b, ok := l.Batches[e.Batch]
				if !ok || b == nil {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条撤销回执轨迹引用了不存在的批次 %q", ErrCorrupt, id, i+1, e.Batch)
				}
				if e.Station != b.Station {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条撤销回执轨迹的站点 %q 与批次 %q 出发站 %q 不一致",
						ErrCorrupt, id, i+1, e.Station, e.Batch, b.Station)
				}
				rv, ok := l.Revokes[e.Request]
				if !ok || rv == nil {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条撤销回执轨迹引用了不存在的撤销请求 %q", ErrCorrupt, id, i+1, e.Request)
				}
				if rv.Receipt != e.RefRequest || rv.Batch != e.Batch || rv.Parcel != id || rv.Reason != e.Reason || !rv.Time.Equal(e.Time) {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条撤销回执轨迹与撤销请求 %q 记录不一致", ErrCorrupt, id, i+1, e.Request)
				}
			}
			if e.Op == "收回" {
				if e.Batch == "" || e.Request == "" || e.Reason == "" {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条收回轨迹缺少批次号、中止请求号或原因", ErrCorrupt, id, i+1)
				}
				a, ok := l.Aborts[e.Request]
				if !ok || a == nil {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条收回轨迹引用了不存在的中止请求 %q", ErrCorrupt, id, i+1, e.Request)
				}
				if a.Batch != e.Batch || a.Station != e.Station || a.Reason != e.Reason || !a.Time.Equal(e.Time) {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条收回轨迹与中止请求 %q 记录不一致", ErrCorrupt, id, i+1, e.Request)
				}
				inAbort := false
				for _, pid := range a.Parcels {
					if pid == id {
						inAbort = true
						break
					}
				}
				if !inAbort {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条收回轨迹不在中止请求 %q 的收回集合中", ErrCorrupt, id, i+1, e.Request)
				}
			}
			if e.Op == "续接" {
				if e.Batch == "" || e.FromBatch == "" || e.Courier == "" || e.Request == "" || e.Reason == "" {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条续接轨迹缺少新批次号、原批次号、新配送员、续接请求号或原因", ErrCorrupt, id, i+1)
				}
				t, ok := l.Transfers[e.Request]
				if !ok || t == nil {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条续接轨迹引用了不存在的续接请求 %q", ErrCorrupt, id, i+1, e.Request)
				}
				if t.FromBatch != e.FromBatch || t.ToBatch != e.Batch || t.ToCourier != e.Courier ||
					t.Station != e.Station || t.Reason != e.Reason || !t.Time.Equal(e.Time) {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条续接轨迹与续接请求 %q 记录不一致", ErrCorrupt, id, i+1, e.Request)
				}
				inTransfer := false
				for _, pid := range t.Parcels {
					if pid == id {
						inTransfer = true
						break
					}
				}
				if !inTransfer {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条续接轨迹不在续接请求 %q 的转交集合中", ErrCorrupt, id, i+1, e.Request)
				}
			}
			if e.Op == "发运" {
				if e.Shipment == "" || e.From == "" || e.To == "" {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条发运轨迹缺少运输单号、源站或目的站", ErrCorrupt, id, i+1)
				}
				sh, ok := l.Shipments[e.Shipment]
				if !ok || sh == nil {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条发运轨迹引用了不存在的运输单 %q", ErrCorrupt, id, i+1, e.Shipment)
				}
				if sh.From != e.From || sh.To != e.To || e.Station != e.From || !sh.Time.Equal(e.Time) {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条发运轨迹与运输单 %q 记录不一致", ErrCorrupt, id, i+1, e.Shipment)
				}
				inShipment := false
				for _, pid := range sh.Parcels {
					if pid == id {
						inShipment = true
						break
					}
				}
				if !inShipment {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条发运轨迹不在运输单 %q 的成员集合中", ErrCorrupt, id, i+1, e.Shipment)
				}
			}
			if e.Op == "接收" {
				if e.Shipment == "" || e.Request == "" || e.From == "" || e.To == "" {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条接收轨迹缺少运输单号、接收请求号、源站或目的站", ErrCorrupt, id, i+1)
				}
				rv, ok := l.Receives[e.Request]
				if !ok || rv == nil {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条接收轨迹引用了不存在的接收请求 %q", ErrCorrupt, id, i+1, e.Request)
				}
				if rv.Shipment != e.Shipment || rv.Station != e.Station || e.Station != e.To || !rv.Time.Equal(e.Time) {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条接收轨迹与接收请求 %q 记录不一致", ErrCorrupt, id, i+1, e.Request)
				}
				sh, ok := l.Shipments[e.Shipment]
				if !ok || sh == nil {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条接收轨迹引用了不存在的运输单 %q", ErrCorrupt, id, i+1, e.Shipment)
				}
				if sh.From != e.From || sh.To != e.To {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条接收轨迹的源站或目的站与运输单 %q 记录（%q -> %q）不一致",
						ErrCorrupt, id, i+1, e.Shipment, sh.From, sh.To)
				}
				inReceive := false
				for _, pid := range rv.Parcels {
					if pid == id {
						inReceive = true
						break
					}
				}
				if !inReceive {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条接收轨迹不在接收请求 %q 的成员集合中", ErrCorrupt, id, i+1, e.Request)
				}
			}
			if e.Op == "冻结" {
				if e.Incident == "" || e.Reason == "" {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条冻结轨迹缺少异常单号或原因", ErrCorrupt, id, i+1)
				}
				f, ok := l.Freezes[e.Incident]
				if !ok || f == nil {
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
				if !ok || f == nil {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条解除冻结轨迹引用了不存在的异常单 %q", ErrCorrupt, id, i+1, e.Incident)
				}
				if f.Parcel != id || f.Station != e.Station || f.ReleasedBy != e.Request {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条解除冻结轨迹与异常单 %q 不匹配", ErrCorrupt, id, i+1, e.Incident)
				}
				u, ok := l.Unfreezes[e.Request]
				if !ok || u == nil || u.Incident != e.Incident || u.Parcel != id || u.Note != e.Note || !u.Time.Equal(e.Time) {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条解除冻结轨迹与解除请求 %q 记录不一致", ErrCorrupt, id, i+1, e.Request)
				}
			}
		}
	}
	// 站间运输生命周期：按每件轨迹的保存顺序（不按发生时间排序）核对发运与接收的
	// 配对关系。每张运输单的成员恰有一次对应发运；已接收时恰有一次对应接收且位于
	// 发运之后；发运后到接收前不得出现其他作业轨迹；未接收时不得有接收记录，
	// 发运必须是最后一条轨迹。
	for id, p := range l.Parcels {
		shipped := make(map[string]bool) // 该包裹轨迹中已出现发运的运输单号
		var open string                  // 已发运、尚未在轨迹中接收的在途运输单号
		for i, e := range p.Trail {
			if open != "" && e.Op != "接收" {
				return fmt.Errorf("%w: 包裹 %q 第 %d 条轨迹（%s）出现在运输单 %q 发运之后、接收之前，在途期间不得有其他作业轨迹",
					ErrCorrupt, id, i+1, e.Op, open)
			}
			switch e.Op {
			case "发运":
				if shipped[e.Shipment] {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条轨迹重复发运运输单 %q，每张运输单的成员恰有一次对应发运",
						ErrCorrupt, id, i+1, e.Shipment)
				}
				shipped[e.Shipment] = true
				open = e.Shipment
			case "接收":
				if open == "" {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条接收轨迹（运输单 %q）位于对应发运之前或缺少对应发运",
						ErrCorrupt, id, i+1, e.Shipment)
				}
				if e.Shipment != open {
					return fmt.Errorf("%w: 包裹 %q 第 %d 条接收轨迹的运输单 %q 与当前在途运输单 %q 不匹配",
						ErrCorrupt, id, i+1, e.Shipment, open)
				}
				open = ""
			}
		}
		if open != "" {
			sh := l.Shipments[open] // 发运轨迹校验已保证运输单存在且非空
			if sh.ReceivedBy != "" {
				return fmt.Errorf("%w: 包裹 %q 的运输单 %q 已接收（接收请求号 %q），但轨迹缺少对应的接收记录",
					ErrCorrupt, id, open, sh.ReceivedBy)
			}
		}
	}
	// 当前归属与状态必须由轨迹记录形成：按保存顺序的最后一条轨迹推导应有的归属
	// 站点与状态，与台账当前值不一致即存在无对应记录的站点或状态改变。接收后的
	// 合法后续流转（交接、再次发运、回执及其撤销、收回、续接、冻结与解除）会
	// 产生新的末条轨迹，按新轨迹校验，不因历史接收而把成员固定在目的站。
	for id, p := range l.Parcels {
		last := p.Trail[len(p.Trail)-1]
		var want string
		switch last.Op {
		case "收件", "交接", "退回", "收回", "接收", "解除冻结":
			want = statusInStation
		case "出站", "续接", "撤销回执":
			want = statusDelivering
		case "回执":
			if last.Result == resultSigned {
				want = statusSigned
			} else {
				want = statusInStation
			}
		case "冻结":
			want = statusFrozen
		case "发运":
			want = statusInTransit
		default:
			continue // 未知操作不参与推导（逐条校验已兜底）
		}
		if p.Station != last.Station || p.Status != want {
			return fmt.Errorf("%w: 包裹 %q 当前归属 %q、状态 %q 与最后一条轨迹（%s，站点 %q）应形成的归属与状态 %q、%q 不一致",
				ErrCorrupt, id, p.Station, p.Status, last.Op, last.Station, last.Station, want)
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
			if !ok || r == nil || r.Handoff != req {
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
		if !ok || h == nil {
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
			if !ok || rc == nil || rc.Batch != id || rc.Parcel != pid {
				return fmt.Errorf("%w: 批次 %q 中包裹 %q 的回执请求号 %q 无法对应", ErrCorrupt, id, pid, e.Request)
			}
			if e.RevokedBy != rc.RevokedBy {
				return fmt.Errorf("%w: 批次 %q 中包裹 %q 的回执条目与回执请求号 %q 的撤销标记不一致", ErrCorrupt, id, pid, e.Request)
			}
		}
		if b.AbortedBy != "" {
			a, ok := l.Aborts[b.AbortedBy]
			if !ok || a == nil || a.Batch != id {
				return fmt.Errorf("%w: 批次 %q 标记的中止请求号 %q 无法对应", ErrCorrupt, id, b.AbortedBy)
			}
		}
		if b.TransferredBy != "" {
			if b.AbortedBy != "" {
				return fmt.Errorf("%w: 批次 %q 不能同时标记中止与转交", ErrCorrupt, id)
			}
			t, ok := l.Transfers[b.TransferredBy]
			if !ok || t == nil || t.FromBatch != id {
				return fmt.Errorf("%w: 批次 %q 标记的续接请求号 %q 无法对应", ErrCorrupt, id, b.TransferredBy)
			}
		}
		if b.RelayedFrom != "" {
			t, ok := l.Transfers[b.RelayRequest]
			if !ok || t == nil || t.ToBatch != id || t.FromBatch != b.RelayedFrom {
				return fmt.Errorf("%w: 批次 %q 标记的续接来源（原批次=%q 续接请求号=%q）无法对应",
					ErrCorrupt, id, b.RelayedFrom, b.RelayRequest)
			}
		} else if b.RelayRequest != "" {
			return fmt.Errorf("%w: 批次 %q 标记了续接请求号 %q 但缺少来源批次", ErrCorrupt, id, b.RelayRequest)
		}
	}
	for req, rc := range l.Receipts {
		if rc == nil || req != rc.Request || rc.Batch == "" || rc.Parcel == "" || rc.Time.IsZero() ||
			(rc.Result != resultSigned && rc.Result != resultFailed) ||
			(rc.Result == resultFailed && rc.Reason == "") || (rc.Result == resultSigned && rc.Reason != "") {
			return fmt.Errorf("%w: 回执请求号 %q 的回执结果不完整", ErrCorrupt, req)
		}
		b, ok := l.Batches[rc.Batch]
		if !ok || b == nil {
			return fmt.Errorf("%w: 回执请求号 %q 引用了不存在的批次 %q", ErrCorrupt, req, rc.Batch)
		}
		// 每件包裹的当前回执条目一旦建立只会被新回执取代、不会删除，
		// 因此该件在批次内必须始终有一条回执条目。
		e, cur := b.Receipts[rc.Parcel]
		if !cur {
			return fmt.Errorf("%w: 回执请求号 %q 在批次 %q 中缺少包裹 %q 的回执条目", ErrCorrupt, req, rc.Batch, rc.Parcel)
		}
		if rc.RevokedBy == "" {
			// 未撤销回执必须仍是该件在该批次的当前有效回执。
			if e.Request != req || e.Result != rc.Result || e.Reason != rc.Reason || !e.Time.Equal(rc.Time) || e.RevokedBy != "" {
				return fmt.Errorf("%w: 回执请求号 %q 与批次 %q 的回执记录不一致", ErrCorrupt, req, rc.Batch)
			}
			continue
		}
		// 已撤销回执：撤销关联必须存在且回指该回执。
		rv, ok := l.Revokes[rc.RevokedBy]
		if !ok || rv == nil || rv.Receipt != req {
			return fmt.Errorf("%w: 回执请求号 %q 标记的撤销请求号 %q 无法对应", ErrCorrupt, req, rc.RevokedBy)
		}
		if e.Request == req {
			// 该件之后未重新回执：批次内条目必须同内容并同样标记撤销。
			if e.Result != rc.Result || e.Reason != rc.Reason || !e.Time.Equal(rc.Time) || e.RevokedBy != rc.RevokedBy {
				return fmt.Errorf("%w: 回执请求号 %q 与批次 %q 的回执记录不一致", ErrCorrupt, req, rc.Batch)
			}
		}
		// 该件之后已用新请求号重新回执：当前条目属于新回执，由其自身校验。
	}
	// 撤销：每条撤销必须对应一条已撤销回执，批次、包裹一致，且包裹轨迹中有
	// 与该撤销请求一致的撤销回执记录；撤销关联缺失或不一致即损坏。
	for req, rv := range l.Revokes {
		if rv == nil || req != rv.Request || rv.Receipt == "" || rv.Batch == "" || rv.Parcel == "" ||
			rv.Reason == "" || rv.Time.IsZero() {
			return fmt.Errorf("%w: 撤销请求号 %q 的撤销结果不完整", ErrCorrupt, req)
		}
		rc, ok := l.Receipts[rv.Receipt]
		if !ok || rc == nil {
			return fmt.Errorf("%w: 撤销请求号 %q 引用了不存在的原回执 %q", ErrCorrupt, req, rv.Receipt)
		}
		if rc.RevokedBy != req {
			return fmt.Errorf("%w: 撤销请求号 %q 与原回执 %q 的撤销标记不一致", ErrCorrupt, req, rv.Receipt)
		}
		if rc.Batch != rv.Batch || rc.Parcel != rv.Parcel {
			return fmt.Errorf("%w: 撤销请求号 %q 的批次或包裹与原回执 %q 不一致", ErrCorrupt, req, rv.Receipt)
		}
		p, ok := l.Parcels[rv.Parcel]
		if !ok {
			return fmt.Errorf("%w: 撤销请求号 %q 引用了不存在的包裹 %q", ErrCorrupt, req, rv.Parcel)
		}
		found := false
		for _, e := range p.Trail {
			if e.Op == "撤销回执" && e.Request == req {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: 撤销请求号 %q 的包裹 %q 缺少对应的撤销回执轨迹", ErrCorrupt, req, rv.Parcel)
		}
	}
	// 中止：收回集合必须恰为原批次的未回执成员（按原成员顺序，已撤销回执不计入
	// 有效回执），且每件收回包裹的轨迹中都有与该中止请求一致的收回记录；
	// 中止记录与收回轨迹不一致即损坏。
	for req, a := range l.Aborts {
		if a == nil || req != a.Request || a.Batch == "" || a.Station == "" || a.Reason == "" ||
			len(a.Parcels) == 0 || a.Time.IsZero() {
			return fmt.Errorf("%w: 中止请求号 %q 的中止结果不完整", ErrCorrupt, req)
		}
		b, ok := l.Batches[a.Batch]
		if !ok || b == nil {
			return fmt.Errorf("%w: 中止请求号 %q 引用了不存在的批次 %q", ErrCorrupt, req, a.Batch)
		}
		if b.AbortedBy != req {
			return fmt.Errorf("%w: 中止请求号 %q 与批次 %q 的中止标记不一致", ErrCorrupt, req, a.Batch)
		}
		if a.Station != b.Station {
			return fmt.Errorf("%w: 中止请求号 %q 的站点 %q 与批次 %q 出发站 %q 不一致", ErrCorrupt, req, a.Station, a.Batch, b.Station)
		}
		expect := make([]string, 0, len(b.Parcels))
		for _, pid := range b.Parcels {
			if e, done := b.Receipts[pid]; !done || e.RevokedBy != "" {
				expect = append(expect, pid)
			}
		}
		if len(expect) != len(a.Parcels) {
			return fmt.Errorf("%w: 中止请求号 %q 的收回集合与批次 %q 的未回执成员不一致", ErrCorrupt, req, a.Batch)
		}
		for i := range expect {
			if expect[i] != a.Parcels[i] {
				return fmt.Errorf("%w: 中止请求号 %q 的收回集合与批次 %q 的未回执成员不一致", ErrCorrupt, req, a.Batch)
			}
		}
		for _, pid := range a.Parcels {
			p, ok := l.Parcels[pid]
			if !ok {
				return fmt.Errorf("%w: 中止请求号 %q 引用了不存在的包裹 %q", ErrCorrupt, req, pid)
			}
			found := false
			for _, e := range p.Trail {
				if e.Op == "收回" && e.Request == req {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%w: 中止请求号 %q 的收回包裹 %q 缺少对应的收回轨迹", ErrCorrupt, req, pid)
			}
		}
	}
	// 续接：转交集合必须恰为原批次的未回执成员（按原成员顺序），新批次必须由该续接
	// 创建且成员、站点、配送员、接手时间与之一致；每件转交包裹的轨迹中都有与该
	// 续接请求一致的续接记录；续接记录与批次及轨迹不一致即损坏。
	for req, t := range l.Transfers {
		if t == nil || req != t.Request || t.FromBatch == "" || t.ToBatch == "" || t.Station == "" ||
			t.FromCourier == "" || t.ToCourier == "" || t.FromCourier == t.ToCourier ||
			t.Reason == "" || len(t.Parcels) == 0 || t.Time.IsZero() {
			return fmt.Errorf("%w: 续接请求号 %q 的续接结果不完整", ErrCorrupt, req)
		}
		fb, ok := l.Batches[t.FromBatch]
		if !ok || fb == nil {
			return fmt.Errorf("%w: 续接请求号 %q 引用了不存在的原批次 %q", ErrCorrupt, req, t.FromBatch)
		}
		if fb.TransferredBy != req {
			return fmt.Errorf("%w: 续接请求号 %q 与原批次 %q 的转交标记不一致", ErrCorrupt, req, t.FromBatch)
		}
		if fb.Station != t.Station || fb.Courier != t.FromCourier {
			return fmt.Errorf("%w: 续接请求号 %q 的站点或原配送员与原批次 %q 不一致", ErrCorrupt, req, t.FromBatch)
		}
		nb, ok := l.Batches[t.ToBatch]
		if !ok || nb == nil {
			return fmt.Errorf("%w: 续接请求号 %q 引用了不存在的新批次 %q", ErrCorrupt, req, t.ToBatch)
		}
		if nb.RelayedFrom != t.FromBatch || nb.RelayRequest != req {
			return fmt.Errorf("%w: 续接请求号 %q 与新批次 %q 的来源标记不一致", ErrCorrupt, req, t.ToBatch)
		}
		if nb.Station != t.Station || nb.Courier != t.ToCourier || !nb.Time.Equal(t.Time) {
			return fmt.Errorf("%w: 续接请求号 %q 与新批次 %q 的站点、配送员或接手时间不一致", ErrCorrupt, req, t.ToBatch)
		}
		expect := make([]string, 0, len(fb.Parcels))
		for _, pid := range fb.Parcels {
			if e, done := fb.Receipts[pid]; !done || e.RevokedBy != "" {
				expect = append(expect, pid)
			}
		}
		if !sameOrder(expect, t.Parcels) || !sameOrder(t.Parcels, nb.Parcels) {
			return fmt.Errorf("%w: 续接请求号 %q 的转交集合与原批次 %q 的未回执成员或新批次 %q 的成员不一致",
				ErrCorrupt, req, t.FromBatch, t.ToBatch)
		}
		for _, pid := range t.Parcels {
			p, ok := l.Parcels[pid]
			if !ok {
				return fmt.Errorf("%w: 续接请求号 %q 引用了不存在的包裹 %q", ErrCorrupt, req, pid)
			}
			found := false
			for _, e := range p.Trail {
				if e.Op == "续接" && e.Request == req {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%w: 续接请求号 %q 的转交包裹 %q 缺少对应的续接轨迹", ErrCorrupt, req, pid)
			}
		}
	}
	// 发运：成员必须非空、不重复且都已登记，每件成员的轨迹中都有与该运输单一致的
	// 发运记录；尚未接收的成员必须仍归该单在途（状态为站间在途、归属源站），已接收
	// 成员的现状不限（其后续流转不影响接收事实）；一件包裹同时只能属于一张运输单的
	// 未接收成员。全部接收标记必须与各次接收事实一致：全部成员均已接收时恰有
	// 完成接收的请求号，未全部接收时不得标记。关联缺失或矛盾即损坏。
	receivedByShipment := make(map[string]map[string]bool) // 运输单号 -> 已接收成员集合
	for _, r := range l.Receives {
		if r == nil {
			continue // 空关联由接收结果校验统一报错
		}
		set := receivedByShipment[r.Shipment]
		if set == nil {
			set = make(map[string]bool)
			receivedByShipment[r.Shipment] = set
		}
		for _, pid := range r.Parcels {
			set[pid] = true
		}
	}
	inTransitCount := make(map[string]int) // 包裹 -> 以它为未接收成员的运输单数
	for no, sh := range l.Shipments {
		if sh == nil || no != sh.Shipment || sh.From == "" || sh.To == "" || sh.From == sh.To ||
			len(sh.Parcels) == 0 || sh.Time.IsZero() {
			return fmt.Errorf("%w: 运输单号 %q 的发运结果不完整", ErrCorrupt, no)
		}
		seen := make(map[string]bool, len(sh.Parcels))
		for _, pid := range sh.Parcels {
			if seen[pid] {
				return fmt.Errorf("%w: 运输单 %q 的成员 %q 重复", ErrCorrupt, no, pid)
			}
			seen[pid] = true
			p, ok := l.Parcels[pid]
			if !ok {
				return fmt.Errorf("%w: 运输单 %q 引用了不存在的包裹 %q", ErrCorrupt, no, pid)
			}
			found := false
			for _, e := range p.Trail {
				if e.Op == "发运" && e.Shipment == no {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%w: 运输单 %q 的成员包裹 %q 缺少对应的发运轨迹", ErrCorrupt, no, pid)
			}
		}
		received := receivedByShipment[no]
		receivedCount := 0
		for _, pid := range sh.Parcels {
			if received[pid] {
				receivedCount++
			}
		}
		if sh.ReceivedBy != "" {
			r, ok := l.Receives[sh.ReceivedBy]
			if !ok || r == nil || r.Shipment != no {
				return fmt.Errorf("%w: 运输单 %q 标记的接收请求号 %q 无法对应", ErrCorrupt, no, sh.ReceivedBy)
			}
			if receivedCount != len(sh.Parcels) {
				return fmt.Errorf("%w: 运输单 %q 标记已全部接收（接收请求号 %q），但按接收事实仍有成员未接收",
					ErrCorrupt, no, sh.ReceivedBy)
			}
		} else if receivedCount == len(sh.Parcels) {
			return fmt.Errorf("%w: 运输单 %q 的全部成员均已接收，但缺少全部接收标记", ErrCorrupt, no)
		}
		for _, pid := range sh.Parcels {
			if received[pid] {
				continue // 已收件的现状不限：后续交接、冻结、配送或再次发运不妨碍余件接收
			}
			p := l.Parcels[pid]
			inTransitCount[pid]++
			if p.Status != statusInTransit || p.Station != sh.From {
				return fmt.Errorf("%w: 运输单 %q 的成员包裹 %q 尚未接收，但当前归属 %q、状态 %q，不在该单在途",
					ErrCorrupt, no, pid, p.Station, p.Status)
			}
		}
	}
	for id, p := range l.Parcels {
		if p.Status == statusInTransit && inTransitCount[id] != 1 {
			return fmt.Errorf("%w: 包裹 %q 当前为站间在途，但未接收运输单数为 %d，应恰好属于一张运输单的未接收成员",
				ErrCorrupt, id, inTransitCount[id])
		}
	}
	// 接收：接收站点必须等于运输单目的站，成员必须非空、不重复、均属原单且按发运
	// 保存顺序排列；同一运输单各次接收的成员不得重叠（同单同件只能接收一次）；
	// 每件成员的轨迹中都有与该接收请求一致的接收记录。关联缺失、同单同件重复
	// 接收或与轨迹矛盾即损坏。
	receiveSeen := make(map[string]map[string]string) // 运输单号 -> 包裹 -> 接收请求号
	for req, r := range l.Receives {
		if r == nil || req != r.Request || r.Shipment == "" || r.Station == "" || len(r.Parcels) == 0 || r.Time.IsZero() {
			return fmt.Errorf("%w: 接收请求号 %q 的接收结果不完整", ErrCorrupt, req)
		}
		sh, ok := l.Shipments[r.Shipment]
		if !ok || sh == nil {
			return fmt.Errorf("%w: 接收请求号 %q 引用了不存在的运输单 %q", ErrCorrupt, req, r.Shipment)
		}
		if r.Station != sh.To {
			return fmt.Errorf("%w: 接收请求号 %q 的接收站点 %q 与运输单 %q 目的站 %q 不一致", ErrCorrupt, req, r.Station, r.Shipment, sh.To)
		}
		member := make(map[string]bool, len(sh.Parcels))
		for _, pid := range sh.Parcels {
			member[pid] = true
		}
		seen := make(map[string]bool, len(r.Parcels))
		pos := 0 // 成员必须按发运保存顺序排列：在 sh.Parcels 中的位置严格递增
		for _, pid := range r.Parcels {
			if !member[pid] {
				return fmt.Errorf("%w: 接收请求号 %q 的包裹 %q 不属于运输单 %q", ErrCorrupt, req, pid, r.Shipment)
			}
			if seen[pid] {
				return fmt.Errorf("%w: 接收请求号 %q 的成员 %q 重复", ErrCorrupt, req, pid)
			}
			seen[pid] = true
			at := -1
			for i := pos; i < len(sh.Parcels); i++ {
				if sh.Parcels[i] == pid {
					at = i
					break
				}
			}
			if at < 0 {
				return fmt.Errorf("%w: 接收请求号 %q 的成员顺序与运输单 %q 的发运保存顺序不一致", ErrCorrupt, req, r.Shipment)
			}
			pos = at + 1
			if prev, dup := receiveSeen[r.Shipment][pid]; dup {
				return fmt.Errorf("%w: 包裹 %q 在运输单 %q 下被接收请求 %q 与 %q 重复接收，同单同件只能接收一次",
					ErrCorrupt, pid, r.Shipment, prev, req)
			}
			if receiveSeen[r.Shipment] == nil {
				receiveSeen[r.Shipment] = make(map[string]string)
			}
			receiveSeen[r.Shipment][pid] = req
		}
		for _, pid := range r.Parcels {
			p, ok := l.Parcels[pid]
			if !ok {
				return fmt.Errorf("%w: 接收请求号 %q 引用了不存在的包裹 %q", ErrCorrupt, req, pid)
			}
			found := false
			for _, e := range p.Trail {
				if e.Op == "接收" && e.Request == req {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%w: 接收请求号 %q 的成员包裹 %q 缺少对应的接收轨迹", ErrCorrupt, req, pid)
			}
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
			if !ok || u == nil || u.Incident != no || u.Parcel != f.Parcel {
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
		if !ok || f == nil {
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

// coordinationPath 返回数据文件对应的协调文件（锁文件）路径。
// 相对、绝对及含 .、.. 的写法先归一到同一绝对路径，已存在的符号链接也被解析，
// 确保同一台账的不同路径写法共用同一把锁；首次创建不存在的台账同样适用。
func coordinationPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析数据文件路径失败: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	} else if dir, derr := filepath.EvalSymlinks(filepath.Dir(abs)); derr == nil {
		abs = filepath.Join(dir, filepath.Base(abs))
	}
	return abs + ".lock", nil
}

// OpenForUpdate 在进程间排他协调下打开台账，供会改写台账的命令使用：
// 先取得该台账的锁（其他终端正在作业时等待其完成），再在锁内读取最新已提交
// 数据；调用方随后完成的受理、去重与整次保存都在最新数据上进行，并行效果
// 等同于某个逐次执行顺序。返回的 release 必须在本次命令结束时调用（无论成败）。
// 锁由操作系统随文件描述符管理，持有者进程被强制终止时自动释放，
// 无需人工删除协调文件即可继续作业。
func OpenForUpdate(path string) (s *Store, release func(), err error) {
	lockPath, err := coordinationPath(path)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, nil, fmt.Errorf("准备协调文件失败: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("打开协调文件失败: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, nil, fmt.Errorf("等待台账协调锁失败: %w", err)
	}
	release = func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}
	s, err = Open(path)
	if err != nil {
		release()
		return nil, nil, err
	}
	return s, release, nil
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
		if saved.RelayedFrom != "" {
			return nil, false, fmt.Errorf("批次号 %q 已由续接 %q 创建（来源批次 %q），不能用于出站，内容冲突",
				batch, saved.RelayRequest, saved.RelayedFrom)
		}
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
// 首次回执：批次必须存在，包裹必须属于该批次、在该批次没有未撤销的回执，且当前仍在该批次
// 配送中；任一不满足则拒绝，不作任何改动。签收后状态为“已签收”；失败表示实物已回到
// 出发站，恢复“在站”，可加入新的配送批次。两种结果都保留站点并追加一条含批次、结果、
// 原因（失败时）、请求号和时间的回执记录；全部成员均有未撤销回执后批次自动完成。
// 已撤销的回执不再占用该件的回执名额，可用新请求号重新回执（见 RevokeReceipt）。
// 相同回执请求号且批次、包裹、结果、清理后的原因相同：直接返回首次结果，replayed 为
// true，不重新检查当前状态，即使失败包裹已进入新批次或该回执已被撤销（重放不恢复回执，
// 结果中可辨认已撤销）；请求号相同但内容不同报冲突。
// 回执请求号与批次号、包裹编号、交接及退回请求号分属独立去重范围，允许同名；
// 失败的首次回执不占用请求号。
func (s *Store) Receipt(request, batch, parcel, result, reason string, now time.Time) (res *ReceiptResult, replayed bool, err error) {
	res, replayed, undo, err := s.applyReceipt(request, batch, parcel, result, reason, now)
	if err != nil || replayed {
		return res, replayed, err
	}
	if err := s.save(); err != nil {
		undo() // 落盘失败：回滚内存变更，保留原有有效数据
		return nil, false, err
	}
	return res, false, nil
}

// applyReceipt 在内存台账上校验并应用一条回执（不落盘），是 Receipt 与
// ImportReceipts 共用的核心。全部校验通过后才会改动数据；返回的 undo 可在
// 整体保存失败时撤销本次改动（重放或校验失败时 undo 为 nil）。
func (s *Store) applyReceipt(request, batch, parcel, result, reason string, now time.Time) (res *ReceiptResult, replayed bool, undo func(), err error) {
	if saved, ok := s.data.Receipts[request]; ok {
		if saved.Batch == batch && saved.Parcel == parcel && saved.Result == result && saved.Reason == reason {
			return saved, true, nil, nil
		}
		return nil, false, nil, fmt.Errorf("回执请求号 %q 已用于一次不同的回执（批次=%q 包裹=%q 结果=%q），内容冲突",
			request, saved.Batch, saved.Parcel, saved.Result)
	}

	b, ok := s.data.Batches[batch]
	if !ok {
		return nil, false, nil, fmt.Errorf("回执失败：批次号 %q 不存在", batch)
	}
	member := false
	for _, id := range b.Parcels {
		if id == parcel {
			member = true
			break
		}
	}
	if !member {
		return nil, false, nil, fmt.Errorf("回执失败：包裹 %q 不属于批次 %q", parcel, batch)
	}
	if e, done := b.Receipts[parcel]; done && e.RevokedBy == "" {
		return nil, false, nil, fmt.Errorf("回执失败：包裹 %q 在批次 %q 中已回执，每件在一个批次只能有一条未撤销回执", parcel, batch)
	}
	p := s.data.Parcels[parcel] // 批次成员必已登记（载入校验保证）
	if p.Status != statusDelivering || currentBatch(p) != batch {
		return nil, false, nil, fmt.Errorf("回执失败：包裹 %q 当前状态为 %q，不在批次 %q 配送中", parcel, p.Status, batch)
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

	// 记录旧值，undo 在整体落盘失败时回滚。
	oldStatus, oldStation := p.Status, p.Station
	oldTrailLen := len(p.Trail)
	oldEntry, hadEntry := b.Receipts[parcel] // 被取代的已撤销条目（若有）

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

	undo = func() {
		delete(s.data.Receipts, request)
		if hadEntry {
			b.Receipts[parcel] = oldEntry
		} else {
			delete(b.Receipts, parcel)
		}
		p.Status, p.Station = oldStatus, oldStation
		p.Trail = p.Trail[:oldTrailLen]
	}
	return res, false, undo, nil
}

// ReceiptImportRecord 是回执整批导入文件中的一条记录（已清洗）。
type ReceiptImportRecord struct {
	Request string // 回执请求号（与逐件 receipt 共用去重范围）
	Batch   string // 配送批次号
	Parcel  string // 包裹编号
	Result  string // 签收 | 失败
	Reason  string // 失败原因（失败时非空，签收时为空）
}

// ReceiptImportItem 是整批导入中一条记录的处理结果，按文件顺序排列。
type ReceiptImportItem struct {
	Record   ReceiptImportRecord // 清洗后的记录内容
	Time     time.Time           // 发生时间（重放时为首次保存的时间）
	Replayed bool                // true 表示同内容历史重放，未产生新变更
	Revoked  bool                // true 表示该回执已被撤销（仅历史重放可能；重放不恢复回执）
}

// ImportReceipts 按文件顺序整批应用回执记录。
//
// 逐条处理：与逐件 Receipt 共用同一回执请求号去重范围，同请求号且批次、包裹、
// 结果、清理后的原因相同的记录返回首次结果与时间（重放），不检查当前状态、不追加
// 轨迹、不改变批次进度；换内容报冲突。首次回执要求批次存在、包裹属于该批次、
// 尚未在该批次回执且当前仍在该批次配送中（含本文件前面记录已产生的回执）。
// 全部记录可接受时作为一个整体原子落盘；任一记录无效、冲突或受理条件不符，
// 整份拒绝并提示记录位置，台账保持导入前状态，新请求号均不占用。
// 全部为历史重放时不改写台账。
func (s *Store) ImportReceipts(records []ReceiptImportRecord, now time.Time) ([]ReceiptImportItem, error) {
	// 在台账副本上按文件顺序试算：任一记录失败直接放弃副本，原台账保持不变。
	work, err := s.fork()
	if err != nil {
		return nil, err
	}
	items := make([]ReceiptImportItem, 0, len(records))
	hasNew := false
	for i, rec := range records {
		res, replayed, _, err := work.applyReceipt(rec.Request, rec.Batch, rec.Parcel, rec.Result, rec.Reason, now)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条记录（请求号 %q）：%w", i+1, rec.Request, err)
		}
		if !replayed {
			hasNew = true
		}
		items = append(items, ReceiptImportItem{Record: rec, Time: res.Time, Replayed: replayed, Revoked: res.RevokedBy != ""})
	}
	if !hasNew {
		return items, nil // 全部为历史重放：不改写台账
	}

	// 整体提交：副本替换当前台账后一次原子落盘，失败时恢复导入前状态。
	orig := s.data
	s.data = work.data
	if err := s.save(); err != nil {
		s.data = orig
		return nil, err
	}
	return items, nil
}

// fork 返回当前台账的深拷贝副本，供整批导入试算；副本上的任何改动都不影响原台账。
func (s *Store) fork() (*Store, error) {
	buf, err := json.Marshal(s.data)
	if err != nil {
		return nil, fmt.Errorf("准备回执整批导入失败: %w", err)
	}
	var cp ledgerFile
	if err := json.Unmarshal(buf, &cp); err != nil {
		return nil, fmt.Errorf("准备回执整批导入失败: %w", err)
	}
	return &Store{path: s.path, data: cp}, nil
}

// RevokeReceipt 撤销一条误录的配送回执：经核实包裹实际仍由原配送员配送，
// 撤销错误的签收或失败登记，不收回实物。包裹与批次取自原回执，不可另选。
//
// 首次撤销：原回执必须存在、未撤销且仍为该件在该批次的有效回执；所属批次
// 未中止、未转交；原回执必须是该包裹最后一条轨迹（其后发生过流转、冻结或
// 解除均拒绝，其他成员的后续操作不阻止撤销），包裹仍在原出发站且状态与回执
// 结果一致。任一不满足则拒绝，不作任何改动。成功后该件恢复原批次配送中，
// 站点、配送员不变，该回执不再计入有效回执（原本已完成的批次恢复配送中，
// 原成员顺序与出站或接手时间保留，其他成员及回执不变）；原回执结果与轨迹
// 永久保留并标记已撤销，追加一条撤销回执记录，回执请求号不删除也不释放。
// 每件同批次最多一条未撤销回执，撤销后可用新回执请求号再次提交签收或失败，
// 也可随原批次中止或续接。撤销不恢复配送前旧交接的退回资格。
// 撤销请求号独立去重，可与其他业务编号同名：相同请求号且原回执、清理后的
// 原因相同，直接返回首次撤销信息和时间，replayed 为 true，不检查现状、不
// 再次改变进度或改写台账（后续重新回执、中止或续接后仍成立）；请求号相同
// 但内容不同报冲突。每条原回执只能撤销一次，换撤销号再次撤销它拒绝；
// 失败的首次撤销不占用请求号。
func (s *Store) RevokeReceipt(request, receiptReq, reason string, now time.Time) (res *RevokeResult, replayed bool, err error) {
	if saved, ok := s.data.Revokes[request]; ok {
		if saved.Receipt == receiptReq && saved.Reason == reason {
			return saved, true, nil
		}
		return nil, false, fmt.Errorf("撤销请求号 %q 已用于一次不同的撤销（原回执=%q 原因=%q），内容冲突",
			request, saved.Receipt, saved.Reason)
	}

	rc, ok := s.data.Receipts[receiptReq]
	if !ok {
		return nil, false, fmt.Errorf("撤销失败：原回执请求号 %q 不存在", receiptReq)
	}
	if rc.RevokedBy != "" {
		return nil, false, fmt.Errorf("撤销失败：原回执 %q 已撤销（撤销请求号 %q），每条原回执只能撤销一次", receiptReq, rc.RevokedBy)
	}
	b := s.data.Batches[rc.Batch] // 回执所属批次必存在（载入校验保证）
	if b.AbortedBy != "" {
		return nil, false, fmt.Errorf("撤销失败：批次 %q 已中止（中止请求号 %q），不能撤销其中的回执", rc.Batch, b.AbortedBy)
	}
	if b.TransferredBy != "" {
		return nil, false, fmt.Errorf("撤销失败：批次 %q 已转交（续接请求号 %q），不能撤销其中的回执", rc.Batch, b.TransferredBy)
	}
	entry := b.Receipts[rc.Parcel]
	if entry == nil || entry.Request != receiptReq {
		return nil, false, fmt.Errorf("撤销失败：回执 %q 已不是包裹 %q 在批次 %q 的有效回执", receiptReq, rc.Parcel, rc.Batch)
	}
	p := s.data.Parcels[rc.Parcel] // 批次成员必已登记（载入校验保证）
	last := p.Trail[len(p.Trail)-1]
	if last.Op != "回执" || last.Request != receiptReq {
		return nil, false, fmt.Errorf("撤销失败：包裹 %q 在原回执 %q 之后又发生流转、冻结或解除，不能撤销该回执", rc.Parcel, receiptReq)
	}
	if p.Station != b.Station {
		return nil, false, fmt.Errorf("撤销失败：包裹 %q 当前归属 %q，不在原出发站 %q，不能撤销该回执", rc.Parcel, p.Station, b.Station)
	}
	want := statusSigned
	if rc.Result == resultFailed {
		want = statusInStation
	}
	if p.Status != want {
		return nil, false, fmt.Errorf("撤销失败：包裹 %q 当前状态为 %q，与原回执 %q 的结果 %q 不一致，不能撤销",
			rc.Parcel, p.Status, receiptReq, rc.Result)
	}

	res = &RevokeResult{
		Request: request,
		Receipt: receiptReq,
		Batch:   rc.Batch,
		Parcel:  rc.Parcel,
		Reason:  reason,
		Time:    now,
	}
	s.data.Revokes[request] = res
	rc.RevokedBy = request
	entry.RevokedBy = request

	// 记录旧值，落盘失败时整体回滚。
	oldStatus := p.Status
	oldTrailLen := len(p.Trail)
	p.Status = statusDelivering // 恢复原批次配送中，站点与配送员不变
	p.Trail = append(p.Trail, Event{
		Op:         "撤销回执",
		Station:    b.Station,
		Batch:      rc.Batch,
		Request:    request,
		RefRequest: receiptReq,
		Reason:     reason,
		Time:       now,
	})

	if err := s.save(); err != nil {
		delete(s.data.Revokes, request)
		rc.RevokedBy = ""
		entry.RevokedBy = ""
		p.Status = oldStatus
		p.Trail = p.Trail[:oldTrailLen]
		return nil, false, err
	}
	return res, false, nil
}

// ReceiptOf 按回执请求号返回回执结果；不存在时返回 nil。
func (s *Store) ReceiptOf(request string) *ReceiptResult {
	return s.data.Receipts[request]
}

// RevokeOf 按撤销请求号返回撤销结果；不存在时返回 nil。
func (s *Store) RevokeOf(request string) *RevokeResult {
	return s.data.Revokes[request]
}

// BatchRevokes 返回一个配送批次相关的全部撤销记录，按撤销时间升序。
func (s *Store) BatchRevokes(batch string) []*RevokeResult {
	var out []*RevokeResult
	for _, rv := range s.data.Revokes {
		if rv.Batch == batch {
			out = append(out, rv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
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

// Abort 中止一个配送批次：配送员已将该批次全部尚未回执的包裹实物收回出发站。
// 成员与站点取自原批次结果，不允许另选成员或站点。
//
// 首次中止：批次必须存在、尚未完成且未中止，且至少有一件未回执成员；这些成员
// 必须仍在原批次配送中、归属出发站，任一不符整批拒绝、不作任何改动。已回执成员
// 及其回执完全保留，即使它们已进入其他批次或被冻结也不阻止中止，不检查也不改变
// 其当前状态。全部满足时按原成员顺序将全部未回执件恢复“在站”（站点为出发站），
// 各追加一条含批次、中止请求号、原因、站点和时间的收回记录；收回不是失败回执，
// 批次回执记录不变。中止永久关闭原批次，原成员顺序、配送员与出站时间保留不变。
// 收回算新的流转，收回后不能退回出站前的旧交接；收回件可交接、冻结或再次出站，
// 但不能首次回执旧批次（receipt 与 receipt-import 同样遵守）。
// 中止请求号独立去重，可与批次号、包裹号及其他业务编号同名：相同请求号且批次、
// 清理后的原因相同，直接返回首次收回集合与时间，replayed 为 true，不重新计算成员、
// 不检查当前状态、不改写台账；请求号相同但批次或原因不同报冲突。
// 每批只能成功中止一次：换请求号再中止或中止已正常完成的批次均拒绝；
// 失败的首次中止不占用请求号。
func (s *Store) Abort(request, batch, reason string, now time.Time) (res *AbortResult, replayed bool, err error) {
	if saved, ok := s.data.Aborts[request]; ok {
		if saved.Batch == batch && saved.Reason == reason {
			return saved, true, nil
		}
		return nil, false, fmt.Errorf("中止请求号 %q 已用于一次不同的中止（批次=%q 原因=%q），内容冲突",
			request, saved.Batch, saved.Reason)
	}

	b, ok := s.data.Batches[batch]
	if !ok {
		return nil, false, fmt.Errorf("中止失败：批次号 %q 不存在", batch)
	}
	if b.AbortedBy != "" {
		return nil, false, fmt.Errorf("中止失败：批次 %q 已中止（中止请求号 %q），每批只能成功中止一次", batch, b.AbortedBy)
	}
	if b.TransferredBy != "" {
		return nil, false, fmt.Errorf("中止失败：批次 %q 已转交（续接请求号 %q），不能中止", batch, b.TransferredBy)
	}
	if b.Done() {
		return nil, false, fmt.Errorf("中止失败：批次 %q 已全部回执完成，不能中止", batch)
	}

	// 按原成员顺序挑出全部未回执成员（批次未完成，必至少有一件）。
	recalled := make([]string, 0, len(b.Parcels)-len(b.Receipts))
	for _, pid := range b.Parcels {
		if e, done := b.Receipts[pid]; !done || e.RevokedBy != "" {
			recalled = append(recalled, pid)
		}
	}
	if len(recalled) == 0 {
		return nil, false, fmt.Errorf("中止失败：批次 %q 没有未回执成员，不能中止", batch)
	}

	// 先做全部校验：未回执成员必须仍在原批次配送中且归属出发站，任一不符整批拒绝。
	for _, pid := range recalled {
		p := s.data.Parcels[pid] // 批次成员必已登记（载入校验保证）
		if p.Status != statusDelivering || currentBatch(p) != batch || p.Station != b.Station {
			return nil, false, fmt.Errorf("中止失败：包裹 %q 当前归属 %q、状态 %q，不在批次 %q 从出发站 %q 配送中，整批中止未执行",
				pid, p.Station, p.Status, batch, b.Station)
		}
	}

	res = &AbortResult{
		Request: request,
		Batch:   batch,
		Station: b.Station,
		Reason:  reason,
		Parcels: recalled,
		Time:    now,
	}
	s.data.Aborts[request] = res
	b.AbortedBy = request

	// 记录旧值，落盘失败时整体回滚。
	prev := make(map[string]struct {
		status string
		trail  []Event
	}, len(recalled))
	for _, pid := range recalled {
		p := s.data.Parcels[pid]
		prev[pid] = struct {
			status string
			trail  []Event
		}{p.Status, append([]Event(nil), p.Trail...)}
		p.Status = statusInStation // 实物已收回出发站，站点仍记出发站
		p.Trail = append(p.Trail, Event{
			Op:      "收回",
			Station: b.Station,
			Batch:   batch,
			Request: request,
			Reason:  reason,
			Time:    now,
		})
	}

	if err := s.save(); err != nil {
		delete(s.data.Aborts, request)
		b.AbortedBy = ""
		for pid, old := range prev {
			p := s.data.Parcels[pid]
			p.Status = old.status
			p.Trail = old.trail
		}
		return nil, false, err
	}
	return res, false, nil
}

// BatchAbort 返回一个配送批次的中止结果；批次不存在或未中止时返回 nil。
func (s *Store) BatchAbort(batch string) *AbortResult {
	b, ok := s.data.Batches[batch]
	if !ok || b.AbortedBy == "" {
		return nil
	}
	return s.data.Aborts[b.AbortedBy]
}

// Transfer 把原批次全部尚未回执的包裹在配送途中整批交给另一配送员：
// 新建批次继续配送，无需回站。成员与站点取自原批次，不允许另选成员或站点。
//
// 首次续接：原批次必须存在、仍开放（未中止、未转交）且有未回执成员；新批次号
// 必须从未被出站或续接使用；新配送员必须与原配送员不同。未回执成员必须全部仍在
// 原批次配送中且归属出发站，任一不符整批拒绝、不作任何改动。已回执成员不检查也
// 不变更，其后续流转或冻结不阻止续接。全部满足时新批次按原成员顺序接纳全部未
// 回执件，沿用出发站，记录新配送员及接手时间；包裹保持“配送中”及原站点，当前
// 配送归属切换到新批次，各追加一条含请求号、前后批次、新配送员、站点、原因和
// 时间的续接记录。原批次永久关闭为“已转交”，原成员、配送员、出站时间与真实
// 回执保留不变；转交不算回执、收回或再次出站。续接算新的流转，续接后不能退回
// 配送前的旧交接。
// 续接请求号独立去重，可与批次号、包裹号及其他业务编号同名：相同请求号且原批次、
// 新批次、新配送员、清理后的原因相同，直接返回首次转交集合、续接信息及时间，
// replayed 为 true，不检查现状、不重算成员、不改写台账；请求号相同但内容不同报
// 冲突；失败的首次续接不占用请求号。
func (s *Store) Transfer(request, fromBatch, toBatch, courier, reason string, now time.Time) (res *TransferResult, replayed bool, err error) {
	if saved, ok := s.data.Transfers[request]; ok {
		if saved.FromBatch == fromBatch && saved.ToBatch == toBatch && saved.ToCourier == courier && saved.Reason == reason {
			return saved, true, nil
		}
		return nil, false, fmt.Errorf("续接请求号 %q 已用于一次不同的续接（原批次=%q 新批次=%q 新配送员=%q），内容冲突",
			request, saved.FromBatch, saved.ToBatch, saved.ToCourier)
	}

	b, ok := s.data.Batches[fromBatch]
	if !ok {
		return nil, false, fmt.Errorf("续接失败：原批次号 %q 不存在", fromBatch)
	}
	if b.AbortedBy != "" {
		return nil, false, fmt.Errorf("续接失败：批次 %q 已中止（中止请求号 %q），不能续接", fromBatch, b.AbortedBy)
	}
	if b.TransferredBy != "" {
		return nil, false, fmt.Errorf("续接失败：批次 %q 已转交（续接请求号 %q），不能再次续接", fromBatch, b.TransferredBy)
	}
	if _, used := s.data.Batches[toBatch]; used {
		return nil, false, fmt.Errorf("续接失败：新批次号 %q 已被出站或续接使用，不能复用", toBatch)
	}
	if courier == b.Courier {
		return nil, false, fmt.Errorf("续接失败：新配送员 %q 不能与原批次 %q 的配送员相同", courier, fromBatch)
	}

	// 按原成员顺序挑出全部未回执成员（批次仍开放，必至少有一件）。
	moved := make([]string, 0, len(b.Parcels)-len(b.Receipts))
	for _, pid := range b.Parcels {
		if e, done := b.Receipts[pid]; !done || e.RevokedBy != "" {
			moved = append(moved, pid)
		}
	}
	if len(moved) == 0 {
		return nil, false, fmt.Errorf("续接失败：批次 %q 没有未回执成员，不能续接", fromBatch)
	}

	// 先做全部校验：未回执成员必须仍在原批次配送中且归属出发站，任一不符整批拒绝。
	for _, pid := range moved {
		p := s.data.Parcels[pid] // 批次成员必已登记（载入校验保证）
		if p.Status != statusDelivering || currentBatch(p) != fromBatch || p.Station != b.Station {
			return nil, false, fmt.Errorf("续接失败：包裹 %q 当前归属 %q、状态 %q，不在批次 %q 从出发站 %q 配送中，整批续接未执行",
				pid, p.Station, p.Status, fromBatch, b.Station)
		}
	}

	res = &TransferResult{
		Request:     request,
		FromBatch:   fromBatch,
		ToBatch:     toBatch,
		Station:     b.Station,
		FromCourier: b.Courier,
		ToCourier:   courier,
		Reason:      reason,
		Parcels:     append([]string(nil), moved...),
		Time:        now,
	}
	s.data.Transfers[request] = res
	s.data.Batches[toBatch] = &BatchResult{
		Batch:        toBatch,
		Station:      b.Station,
		Courier:      courier,
		Parcels:      append([]string(nil), moved...),
		Time:         now,
		Receipts:     map[string]*ReceiptEntry{},
		RelayedFrom:  fromBatch,
		RelayRequest: request,
	}
	b.TransferredBy = request

	// 记录旧值，落盘失败时整体回滚。包裹保持“配送中”及原站点，只追加续接轨迹。
	prev := make(map[string][]Event, len(moved))
	for _, pid := range moved {
		p := s.data.Parcels[pid]
		prev[pid] = append([]Event(nil), p.Trail...)
		p.Trail = append(p.Trail, Event{
			Op:        "续接",
			Station:   b.Station,
			Batch:     toBatch,
			FromBatch: fromBatch,
			Courier:   courier,
			Request:   request,
			Reason:    reason,
			Time:      now,
		})
	}

	if err := s.save(); err != nil {
		delete(s.data.Transfers, request)
		delete(s.data.Batches, toBatch)
		b.TransferredBy = ""
		for pid, old := range prev {
			s.data.Parcels[pid].Trail = old
		}
		return nil, false, err
	}
	return res, false, nil
}

// BatchTransferSource 返回以该批次为原批次的续接结果；批次不存在或未转交时返回 nil。
func (s *Store) BatchTransferSource(batch string) *TransferResult {
	b, ok := s.data.Batches[batch]
	if !ok || b.TransferredBy == "" {
		return nil
	}
	return s.data.Transfers[b.TransferredBy]
}

// TransferOf 按续接请求号返回续接结果；不存在时返回 nil。
func (s *Store) TransferOf(request string) *TransferResult {
	return s.data.Transfers[request]
}

// Ship 提交一次站间发运：整单包裹离开源站、尚未到达目的站。
//
// 首次发运：每件包裹必须已登记、当前归属源站且状态为在站；任一不满足则整单拒绝，
// 不作任何改动（在途、配送中、已签收或已冻结的包裹均不满足）。全部满足时整单转为
// “站间在途”（归属站点暂记源站），每件追加一条含运输单号、两站和时间的发运记录；
// 成员按首次提交顺序保存，保存后不可修改。每件包裹同时最多属于一张未接收运输单。
// 发运算新的流转：发运后不能退回此前的旧交接；运输单不是交接，不能作为退回的原交接。
// 运输单号独立于已有各类编号，接收后也不释放：相同运输单号且源站、目的站、包裹集合
// 相同（集合顺序无关），直接返回首次成员顺序与发运时间，replayed 为 true，不检查现状、
// 不追加轨迹、不改写台账；运输单号相同但内容不同报冲突；失败的首次发运不占用运输单号。
func (s *Store) Ship(shipment, from, to string, parcels []string, now time.Time) (res *ShipmentResult, replayed bool, err error) {
	if saved, ok := s.data.Shipments[shipment]; ok {
		if saved.From == from && saved.To == to && sameSet(saved.Parcels, parcels) {
			return saved, true, nil
		}
		return nil, false, fmt.Errorf("运输单号 %q 已用于一次不同的发运（源站=%q 目的站=%q），内容冲突", shipment, saved.From, saved.To)
	}

	// 首次发运：先做全部校验，任何一件不满足都整单拒绝。
	for _, id := range parcels {
		p, ok := s.data.Parcels[id]
		if !ok {
			return nil, false, fmt.Errorf("发运失败：包裹 %q 未登记，整单发运未执行", id)
		}
		if p.Station != from {
			return nil, false, fmt.Errorf("发运失败：包裹 %q 当前归属 %q，不在源站 %q，整单发运未执行", id, p.Station, from)
		}
		if p.Status != statusInStation {
			return nil, false, fmt.Errorf("发运失败：包裹 %q 当前状态为 %q，不是在站，整单发运未执行", id, p.Status)
		}
	}

	res = &ShipmentResult{
		Shipment: shipment,
		From:     from,
		To:       to,
		Parcels:  append([]string(nil), parcels...),
		Time:     now,
	}
	s.data.Shipments[shipment] = res

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
		p.Status = statusInTransit // 归属站点暂记源站，整单转为站间在途
		p.Trail = append(p.Trail, Event{
			Op:       "发运",
			Station:  from,
			From:     from,
			To:       to,
			Shipment: shipment,
			Time:     now,
		})
	}

	if err := s.save(); err != nil {
		delete(s.data.Shipments, shipment)
		for id, old := range prev {
			p := s.data.Parcels[id]
			p.Status = old.status
			p.Trail = old.trail
		}
		return nil, false, err
	}
	return res, false, nil
}

// Receive 提交一次到站接收：运输单的部分或全部未到成员到达目的站。
// parcels 为 nil 表示不选成员，接收当时全部尚未接收件；非 nil 为显式选择的
// 非空、不重复集合（由调用方清洗）。
//
// 首次接收：运输单必须存在，接收站点必须等于运输单目的站，本次至少接收一件。
// 显式集合的每件选中件都必须属于原单、尚未接收，且仍在源站归该单在途；
// 不选成员时本次集合为当时的全部尚未接收件。任一不满足则整次拒绝，不作任何改动。
// 已收件不再检查接收条件也不变更，其后续交接、冻结、配送或再次发运不妨碍余件接收。
// 成功时按发运保存顺序将本次成员改归目的站、恢复“在站”，各追加一条含运输单号、
// 两站、接收请求号和时间的接收记录；全部成员到齐后运输单永久标记为已接收，
// 随后可继续在站作业。接收算新的流转：接收后不能退回此前的旧交接。
// 两种选择方式共用接收请求号去重范围，与运输单号及已有各类编号独立，允许同名：
// 相同接收请求号且运输单、接收站点、选择方式相同（显式集合换序无关），直接返回
// 首次接收集合与时间，replayed 为 true，不检查现状、不重算余件、不追加轨迹、
// 不改写台账（全部接收及后续流转后仍成立）；请求号相同但运输单、接收站点、
// 选择方式或显式集合不同报冲突；全部接收后新请求拒绝；失败的首次接收不占用请求号。
func (s *Store) Receive(request, shipment, station string, parcels []string, now time.Time) (res *ReceiveResult, replayed bool, err error) {
	explicit := parcels != nil
	if saved, ok := s.data.Receives[request]; ok {
		if saved.Shipment == shipment && saved.Station == station && saved.Explicit == explicit &&
			(!explicit || sameSet(saved.Parcels, parcels)) {
			return saved, true, nil
		}
		return nil, false, fmt.Errorf("接收请求号 %q 已用于一次不同的接收（运输单=%q 接收站点=%q），内容冲突",
			request, saved.Shipment, saved.Station)
	}

	sh, ok := s.data.Shipments[shipment]
	if !ok {
		return nil, false, fmt.Errorf("接收失败：运输单号 %q 不存在", shipment)
	}
	if station != sh.To {
		return nil, false, fmt.Errorf("接收失败：接收站点 %q 不是运输单 %q 的目的站 %q，本次接收未执行", station, shipment, sh.To)
	}

	received := s.receivedUnder(shipment)
	var members []string // 本次接收集合，按发运保存顺序
	if explicit {
		if len(parcels) == 0 {
			return nil, false, fmt.Errorf("接收失败：显式选择的包裹集合不可为空，本次接收未执行")
		}
		want := make(map[string]bool, len(parcels))
		for _, pid := range parcels {
			want[pid] = true
		}
		// 先做全部校验：选中件均属原单、尚未接收且仍归该单在途，任一不符整次拒绝。
		for _, pid := range parcels {
			member := false
			for _, mid := range sh.Parcels {
				if mid == pid {
					member = true
					break
				}
			}
			if !member {
				return nil, false, fmt.Errorf("接收失败：包裹 %q 不属于运输单 %q，本次接收未执行", pid, shipment)
			}
			if received[pid] {
				return nil, false, fmt.Errorf("接收失败：包裹 %q 已在运输单 %q 下接收，不能重复接收，本次接收未执行", pid, shipment)
			}
			p := s.data.Parcels[pid] // 运输单成员必已登记（载入校验保证）
			if p.Status != statusInTransit || p.Station != sh.From {
				return nil, false, fmt.Errorf("接收失败：包裹 %q 当前归属 %q、状态 %q，不在运输单 %q 从源站 %q 在途，本次接收未执行",
					pid, p.Station, p.Status, shipment, sh.From)
			}
		}
		for _, mid := range sh.Parcels {
			if want[mid] {
				members = append(members, mid)
			}
		}
	} else {
		// 不选成员：本次集合为当时的全部尚未接收件，按发运保存顺序。
		for _, mid := range sh.Parcels {
			if !received[mid] {
				members = append(members, mid)
			}
		}
		if len(members) == 0 {
			return nil, false, fmt.Errorf("接收失败：运输单 %q 已全部接收（接收请求号 %q），没有尚未接收的成员，不能再次接收",
				shipment, sh.ReceivedBy)
		}
	}

	res = &ReceiveResult{
		Request:  request,
		Shipment: shipment,
		Station:  station,
		Parcels:  members,
		Time:     now,
		Explicit: explicit,
	}
	s.data.Receives[request] = res
	oldReceivedBy := sh.ReceivedBy
	if len(received)+len(members) == len(sh.Parcels) {
		sh.ReceivedBy = request // 全部成员到齐：运输单永久标记为已接收
	}

	// 记录旧值，落盘失败时整体回滚。
	prev := make(map[string]struct {
		station string
		status  string
		trail   []Event
	}, len(members))
	for _, pid := range members {
		p := s.data.Parcels[pid]
		prev[pid] = struct {
			station string
			status  string
			trail   []Event
		}{p.Station, p.Status, append([]Event(nil), p.Trail...)}
		p.Station = sh.To
		p.Status = statusInStation
		p.Trail = append(p.Trail, Event{
			Op:       "接收",
			Station:  sh.To,
			From:     sh.From,
			To:       sh.To,
			Shipment: shipment,
			Request:  request,
			Time:     now,
		})
	}

	if err := s.save(); err != nil {
		delete(s.data.Receives, request)
		sh.ReceivedBy = oldReceivedBy
		for pid, old := range prev {
			p := s.data.Parcels[pid]
			p.Station, p.Status = old.station, old.status
			p.Trail = old.trail
		}
		return nil, false, err
	}
	return res, false, nil
}

// ShipmentQuery 按运输单号返回发运结果；运输单不存在时报错。
func (s *Store) ShipmentQuery(shipment string) (*ShipmentResult, error) {
	sh, ok := s.data.Shipments[shipment]
	if !ok {
		return nil, fmt.Errorf("运输单 %q 不存在", shipment)
	}
	return sh, nil
}

// ReceiveOf 按接收请求号返回接收结果；不存在时返回 nil。
func (s *Store) ReceiveOf(request string) *ReceiveResult {
	return s.data.Receives[request]
}

// receivedUnder 返回一张运输单已接收成员的集合（按接收结果表统计）。
func (s *Store) receivedUnder(shipment string) map[string]bool {
	got := make(map[string]bool)
	for _, r := range s.data.Receives {
		if r == nil || r.Shipment != shipment {
			continue
		}
		for _, pid := range r.Parcels {
			got[pid] = true
		}
	}
	return got
}

// ShipmentReceiveOf 返回一件包裹在指定运输单下的接收结果；该件尚未接收时返回 nil。
func (s *Store) ShipmentReceiveOf(shipment, parcel string) *ReceiveResult {
	for _, r := range s.data.Receives {
		if r == nil || r.Shipment != shipment {
			continue
		}
		for _, pid := range r.Parcels {
			if pid == parcel {
				return r
			}
		}
	}
	return nil
}

// ActiveShipment 返回包裹当前所属的运输单（该包裹是其中尚未接收的成员）；
// 包裹不存在或不归某单在途时返回 nil。已收件即使原单未收齐也不再归属原单。
func (s *Store) ActiveShipment(id string) *ShipmentResult {
	p, ok := s.data.Parcels[id]
	if !ok || p.Status != statusInTransit {
		return nil
	}
	for _, sh := range s.data.Shipments {
		if sh == nil {
			continue
		}
		member := false
		for _, pid := range sh.Parcels {
			if pid == id {
				member = true
				break
			}
		}
		if member && s.ShipmentReceiveOf(sh.Shipment, id) == nil {
			return sh
		}
	}
	return nil
}

// currentBatch 返回包裹当前配送归属的批次号（最后一条出站或续接记录的批次）；
// 不在配送中返回空。续接不新增出站记录，但会把当前配送归属切换到新批次。
func currentBatch(p *Parcel) string {
	for i := len(p.Trail) - 1; i >= 0; i-- {
		if p.Trail[i].Op == "出站" || p.Trail[i].Op == "续接" {
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

// lastFlowEvent 返回轨迹中最后一条真实流转记录（交接/退回/出站/回执/收回/续接/撤销回执/发运/接收）。
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

// sameOrder 判断两个列表长度与逐元素顺序完全一致。
func sameOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
