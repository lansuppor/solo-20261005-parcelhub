package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
)

// 包裹状态：当前业务中包裹完成登记或交接后均处于在站状态。
const statusAtSite = "在站"

// Event 是包裹轨迹中的一条记录。
type Event struct {
	Op        string `json:"op"`                  // "收件" 或 "交接"
	Site      string `json:"site,omitempty"`      // 收件时的收件站点
	From      string `json:"from,omitempty"`      // 交接源站点
	To        string `json:"to,omitempty"`        // 交接目的站点
	RequestID string `json:"requestId,omitempty"` // 交接请求号
	Time      string `json:"time"`                // RFC3339 时间
}

// Parcel 是一件包裹的当前归属与完整轨迹。
type Parcel struct {
	ID     string  `json:"id"`
	Site   string  `json:"site"`
	Status string  `json:"status"`
	Events []Event `json:"events"`
}

// HandoverResult 是一次成功交接的保存结果，用于去重重放。
type HandoverResult struct {
	RequestID string   `json:"requestId"`
	From      string   `json:"from"`
	To        string   `json:"to"`
	Parcels   []string `json:"parcels"`
}

// HandoverRecord 按请求号保存的交接结果及其规范化判定键。
type HandoverRecord struct {
	Key    string         `json:"key"`
	Result HandoverResult `json:"result"`
}

// Ledger 是台账：包裹表 + 交接请求去重表。
type Ledger struct {
	Parcels  map[string]*Parcel         `json:"parcels"`
	Requests map[string]*HandoverRecord `json:"requests"`
}

func newLedger() *Ledger {
	return &Ledger{
		Parcels:  map[string]*Parcel{},
		Requests: map[string]*HandoverRecord{},
	}
}

// handoverKey 生成交接内容的规范化判定键，包裹集合顺序不影响结果。
func handoverKey(from, to string, parcels []string) string {
	sorted := make([]string, len(parcels))
	copy(sorted, parcels)
	sort.Strings(sorted)
	parts := append([]string{from, to}, sorted...)
	return strings.Join(parts, "\x1f")
}

// loadLedger 读取数据文件。文件不存在时返回空台账；内容损坏时明确报错。
func loadLedger(path string) (*Ledger, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return newLedger(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取数据文件失败: %w", err)
	}
	var l Ledger
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("数据文件内容损坏，拒绝读写: %s", path)
	}
	if l.Parcels == nil {
		l.Parcels = map[string]*Parcel{}
	}
	if l.Requests == nil {
		l.Requests = map[string]*HandoverRecord{}
	}
	for id, p := range l.Parcels {
		if p == nil || p.ID != id || p.Events == nil {
			return nil, fmt.Errorf("数据文件内容损坏，拒绝读写: %s", path)
		}
	}
	for id, r := range l.Requests {
		if r == nil || r.Result.RequestID != id {
			return nil, fmt.Errorf("数据文件内容损坏，拒绝读写: %s", path)
		}
	}
	return &l, nil
}

// saveLedger 将台账整体原子写入：先写临时文件再重命名，
// 保证一次成功交接的状态、轨迹和请求结果作为整体落盘。
func saveLedger(path string, l *Ledger) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化台账失败: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("保存数据文件失败: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("保存数据文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("保存数据文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("保存数据文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("保存数据文件失败: %w", err)
	}
	return nil
}
