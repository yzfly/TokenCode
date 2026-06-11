// Package usage 是 token 用量记账层：每次模型调用追加一行 JSONL，
// 按月分文件存于 $XDG_DATA_HOME/tokencode/usage/YYYY-MM.jsonl
// （未设 XDG 时 ~/.local/share/...，目录惯例与 internal/session 对齐）。
// append-only、损坏行读取时跳过；写入廉价且失败静默——记账绝不影响对话。
// WebUI 大盘与 /usage 命令共用 Summarize 聚合。
package usage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record 是一次模型调用的用量记录（JSONL 一行）。
// Source 标识调用方："user"、"subagent:<label>"、"race:judge"、
// "heartbeat"、"dream"、"headless"、"serve" 等。
type Record struct {
	TS         time.Time `json:"ts"`
	Model      string    `json:"model"`
	Source     string    `json:"source"`
	In         int       `json:"in"`
	Out        int       `json:"out"`
	CacheRead  int       `json:"cache_read,omitempty"`
	CacheWrite int       `json:"cache_write,omitempty"`
}

// Dir 返回记账根目录（与 session 的数据目录惯例一致）。
func Dir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "tokencode", "usage")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "tokencode", "usage")
}

var (
	mu     sync.Mutex
	warned bool // 首个写失败在 stderr 警告一次，之后静默
)

// Log 追加一条用量记录。全零用量的记录直接丢弃——fake/不报用量的端点
// 没有可记的信息，也避免单测里的假模型污染真实账本。
// 写失败静默（首个错误在 stderr 警告一次）：记账绝不打断对话。
func Log(rec Record) {
	if rec.In == 0 && rec.Out == 0 && rec.CacheRead == 0 && rec.CacheWrite == 0 {
		return
	}
	if rec.TS.IsZero() {
		rec.TS = time.Now()
	}
	if rec.Source == "" {
		rec.Source = "user"
	}

	mu.Lock()
	defer mu.Unlock()
	if err := appendLine(rec); err != nil && !warned {
		warned = true
		fmt.Fprintf(os.Stderr, "warn: 用量记账不可用: %v\n", err)
	}
}

// appendLine 把记录序列化后追加到当月文件（open-append-close，
// 频率是每次模型调用一次，开销可忽略且天然适配按月滚动）。
func appendLine(rec Record) error {
	root := Dir()
	if root == "" {
		return fmt.Errorf("无法定位数据目录")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	path := filepath.Join(root, rec.TS.Format("2006-01")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// Bucket 是一个聚合桶：in/out/cache 合计与调用次数。
type Bucket struct {
	In         int `json:"in"`
	Out        int `json:"out"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
	Calls      int `json:"calls"`
}

func (b *Bucket) add(r Record) {
	b.In += r.In
	b.Out += r.Out
	b.CacheRead += r.CacheRead
	b.CacheWrite += r.CacheWrite
	b.Calls++
}

// Summary 是一段时间内的用量汇总：总计 + 按模型 / 按来源 / 按天三个维度。
type Summary struct {
	Total    Bucket            `json:"total"`
	ByModel  map[string]Bucket `json:"by_model"`
	BySource map[string]Bucket `json:"by_source"`
	ByDay    map[string]Bucket `json:"by_day"` // 键为 "2006-01-02"（本地时区）
}

// Summarize 聚合 [from, to) 区间内的全部记录。只读涉及的月度文件，
// 损坏行跳过；目录不存在视为零用量（不报错）。
func Summarize(from, to time.Time) (Summary, error) {
	sum := Summary{
		ByModel:  map[string]Bucket{},
		BySource: map[string]Bucket{},
		ByDay:    map[string]Bucket{},
	}
	root := Dir()
	if root == "" {
		return sum, fmt.Errorf("usage: 无法定位数据目录")
	}

	// 逐月扫过区间涉及的文件（含 from、to 所在月）。
	for m := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, from.Location()); !m.After(to); m = m.AddDate(0, 1, 0) {
		path := filepath.Join(root, m.Format("2006-01")+".jsonl")
		if err := scanFile(path, from, to, &sum); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// scanFile 读一个月度文件，把落在 [from, to) 内的记录累进 sum。
// 文件不存在不算错；半行损坏（崩溃残留）跳过。
func scanFile(path string, from, to time.Time, sum *Summary) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue // 损坏行跳过
		}
		if r.TS.Before(from) || !r.TS.Before(to) {
			continue
		}
		sum.Total.add(r)
		bump(sum.ByModel, r.Model, r)
		bump(sum.BySource, r.Source, r)
		bump(sum.ByDay, r.TS.Local().Format("2006-01-02"), r)
	}
	return sc.Err()
}

func bump(m map[string]Bucket, key string, r Record) {
	b := m[key]
	b.add(r)
	m[key] = b
}
