// Package checkpoint 实现文件改动检查点：write/edit 写盘前把原内容存入
// 影子目录，/rewind 可按 turn 粒度把文件恢复到任一检查点拍下时的状态。
// 只管文件，不管对话历史；bash 产生的改动拦不到（已知盲区，bash 是任意
// 进程，没有写盘前的静态拦截点）。
package checkpoint

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry 是 manifest（JSONL）里的一行：一次写盘前的快照记录。
type Entry struct {
	Seq     int       `json:"seq"`     // 全局递增序号（恢复按它逆序）
	Turn    int       `json:"turn"`    // 用户 turn 序号（检查点按它分组）
	Tool    string    `json:"tool"`    // 触发快照的工具（write/edit）
	Path    string    `json:"path"`    // 被改文件的绝对路径
	Existed bool      `json:"existed"` // 写之前文件是否存在；false=回滚即删除
	Shadow  string    `json:"shadow"`  // 影子文件名（相对会话目录；Existed=false 时为空）
	Time    time.Time `json:"time"`
}

// Checkpointer 管理一个会话的检查点目录与 manifest。
// 并发安全：子代理可能并行触发快照。
type Checkpointer struct {
	mu   sync.Mutex
	dir  string // <base>/<会话id>，懒创建（没有写操作就不留垃圾目录）
	seq  int
	turn int
}

// manifestName 是会话目录下的 manifest 文件名。
const manifestName = "manifest.jsonl"

// New 建一个 Checkpointer，会话目录为 base/<时间戳-pid>。
// 目录懒创建：第一次 Snapshot 才落盘。
func New(base string) *Checkpointer {
	id := fmt.Sprintf("%s-%d", time.Now().Format("20060102-150405"), os.Getpid())
	return &Checkpointer{dir: filepath.Join(base, id), turn: 1}
}

// Dir 返回会话检查点目录（可能尚未创建）。
func (c *Checkpointer) Dir() string { return c.dir }

// BeginTurn 在每条用户消息发出时调用，递增 turn 序号。
func (c *Checkpointer) BeginTurn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.turn++
}

// Snapshot 在工具写 path 之前调用：把原内容（或「不存在」标记）记入检查点。
// 尽力而为——快照失败绝不阻塞工具写盘，错误静默吞掉。
func (c *Checkpointer) Snapshot(tool, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return
	}
	c.seq++
	e := Entry{Seq: c.seq, Turn: c.turn, Tool: tool, Path: path, Time: time.Now()}
	if data, err := os.ReadFile(path); err == nil {
		h := sha256.Sum256([]byte(path))
		e.Existed = true
		e.Shadow = fmt.Sprintf("%d-%s", c.seq, hex.EncodeToString(h[:6]))
		if err := os.WriteFile(filepath.Join(c.dir, e.Shadow), data, 0o644); err != nil {
			c.seq-- // 影子没写成，这条快照作废
			return
		}
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(c.dir, manifestName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// load 从 manifest 读回全部记录，损坏的行直接跳过（manifest 是 append-only
// JSONL，个别行坏掉不该让整个 /rewind 不可用）。文件不存在返回空。
func (c *Checkpointer) load() []Entry {
	f, err := os.Open(filepath.Join(c.dir, manifestName))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Seq == 0 || e.Path == "" {
			continue // 损坏行跳过
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// Point 是 /rewind 列表里的一个检查点：一个有写操作的 turn 为一组，
// 代表「该 turn 改动之前」的文件状态。
type Point struct {
	N     int       // 列表序号（1 起，按时间正序）
	Turn  int       // 对应的用户 turn
	Time  time.Time // 该 turn 第一次写盘的时间
	Tools []string  // 涉及的工具（去重）
	Files int       // 涉及的文件数（去重）
}

// List 返回全部检查点（按 turn 分组，时间正序）。
func (c *Checkpointer) List() []Point {
	c.mu.Lock()
	defer c.mu.Unlock()
	return groupPoints(c.load())
}

// groupPoints 把 manifest 记录按 turn 分组成检查点列表。
func groupPoints(entries []Entry) []Point {
	var pts []Point
	idx := map[int]int{} // turn → pts 下标
	for _, e := range entries {
		i, ok := idx[e.Turn]
		if !ok {
			pts = append(pts, Point{N: len(pts) + 1, Turn: e.Turn, Time: e.Time})
			i = len(pts) - 1
			idx[e.Turn] = i
		}
		if !containsStr(pts[i].Tools, e.Tool) {
			pts[i].Tools = append(pts[i].Tools, e.Tool)
		}
	}
	// 文件数去重：同一 turn 内同一路径多次写只算一个。
	files := map[int]map[string]bool{}
	for _, e := range entries {
		if files[e.Turn] == nil {
			files[e.Turn] = map[string]bool{}
		}
		files[e.Turn][e.Path] = true
	}
	for i := range pts {
		pts[i].Files = len(files[pts[i].Turn])
	}
	return pts
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// Rewind 回滚到第 n 个检查点（List 序号）：把该检查点对应 turn 及之后的
// 全部改动按 seq 逆序撤销——原文件恢复影子内容、当时不存在的文件删除。
// 撤销过的记录从 manifest 移除（影子文件一并清理），保证可连续回滚。
// 返回恢复的文件列表（去重，恢复顺序）。只回滚文件，不回滚对话。
func (c *Checkpointer) Rewind(n int) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := c.load()
	pts := groupPoints(entries)
	if n < 1 || n > len(pts) {
		return nil, fmt.Errorf("没有第 %d 个检查点（当前共 %d 个，/rewind 查看列表）", n, len(pts))
	}
	fromTurn := pts[n-1].Turn

	var undo, keep []Entry
	for _, e := range entries {
		if e.Turn >= fromTurn {
			undo = append(undo, e)
		} else {
			keep = append(keep, e)
		}
	}

	var restored []string
	seen := map[string]bool{}
	var firstErr error
	for i := len(undo) - 1; i >= 0; i-- {
		e := undo[i]
		var err error
		if e.Existed {
			var data []byte
			if data, err = os.ReadFile(filepath.Join(c.dir, e.Shadow)); err == nil {
				err = os.WriteFile(e.Path, data, 0o644)
			}
		} else {
			err = os.Remove(e.Path)
			if os.IsNotExist(err) {
				err = nil // 已经不在了，目标达成
			}
		}
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("恢复 %s: %w", e.Path, err)
		}
		if err == nil && !seen[e.Path] {
			seen[e.Path] = true
			restored = append(restored, e.Path)
		}
	}

	// 重写 manifest 只留未撤销的记录，撤销过的影子文件顺手清掉。
	if err := c.rewriteManifest(keep); err != nil && firstErr == nil {
		firstErr = err
	}
	for _, e := range undo {
		if e.Shadow != "" {
			_ = os.Remove(filepath.Join(c.dir, e.Shadow))
		}
	}
	return restored, firstErr
}

// rewriteManifest 用给定记录整体重写 manifest（先写临时文件再原子替换）。
func (c *Checkpointer) rewriteManifest(entries []Entry) error {
	path := filepath.Join(c.dir, manifestName)
	if len(entries) == 0 {
		return os.Remove(path)
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Clear 清空本会话的全部检查点（影子文件 + manifest）。
func (c *Checkpointer) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.RemoveAll(c.dir); err != nil {
		return err
	}
	c.seq = 0
	return nil
}

// CleanOld 删除 base 下修改时间早于 maxAge 的旧会话目录（启动时调用，
// 尽力而为）。进程退出不删当次目录——留给用户翻旧账。
func CleanOld(base string, maxAge time.Duration) {
	des, err := os.ReadDir(base)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		info, err := de.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(base, de.Name()))
		}
	}
}
