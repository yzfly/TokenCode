package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLogAndSummarize 覆盖主链路：Log 落盘到当月文件 → Summarize 按
// 模型/来源/天聚合正确，区间外与全零记录不计入。
func TestLogAndSummarize(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.Local)
	Log(Record{TS: now, Model: "m1", Source: "user", In: 100, Out: 20, CacheRead: 80, CacheWrite: 16})
	Log(Record{TS: now.Add(time.Hour), Model: "m1", Source: "subagent:racer#1", In: 50, Out: 5})
	Log(Record{TS: now.AddDate(0, 0, -1), Model: "m2", Source: "user", In: 7, Out: 3})
	Log(Record{TS: now, Model: "m1", Source: "user"})                                  // 全零：丢弃
	Log(Record{TS: now.AddDate(0, 0, 10), Model: "m1", Source: "user", In: 1, Out: 1}) // 区间外
	Log(Record{TS: now.AddDate(0, -2, 0), Model: "m3", Source: "user", In: 9, Out: 9}) // 上上月，区间外

	from := now.AddDate(0, 0, -2)
	to := now.AddDate(0, 0, 2)
	sum, err := Summarize(from, to)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}

	if sum.Total.Calls != 3 || sum.Total.In != 157 || sum.Total.Out != 28 ||
		sum.Total.CacheRead != 80 || sum.Total.CacheWrite != 16 {
		t.Fatalf("total wrong: %+v", sum.Total)
	}
	if b := sum.ByModel["m1"]; b.Calls != 2 || b.In != 150 || b.Out != 25 {
		t.Fatalf("by_model[m1] wrong: %+v", b)
	}
	if b := sum.ByModel["m2"]; b.Calls != 1 || b.In != 7 {
		t.Fatalf("by_model[m2] wrong: %+v", b)
	}
	if _, ok := sum.ByModel["m3"]; ok {
		t.Fatal("区间外的月份不该被计入")
	}
	if b := sum.BySource["subagent:racer#1"]; b.Calls != 1 || b.In != 50 {
		t.Fatalf("by_source wrong: %+v", b)
	}
	if b := sum.ByDay[now.Format("2006-01-02")]; b.Calls != 2 {
		t.Fatalf("by_day wrong: %+v", b)
	}

	// 文件按月命名。
	if _, err := os.Stat(filepath.Join(Dir(), "2026-06.jsonl")); err != nil {
		t.Fatalf("月度文件缺失: %v", err)
	}
}

// TestSummarizeSkipsCorruptLine 验证半行损坏（崩溃残留）被跳过，
// 其余记录照常聚合。
func TestSummarizeSkipsCorruptLine(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.Local)
	Log(Record{TS: now, Model: "m1", Source: "user", In: 10, Out: 2})

	path := filepath.Join(Dir(), "2026-06.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"ts":"2026-06-11T11:00:00`) // 写一半崩溃的残行
	f.Close()

	sum, err := Summarize(now.AddDate(0, 0, -1), now.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if sum.Total.Calls != 1 || sum.Total.In != 10 {
		t.Fatalf("total wrong: %+v", sum.Total)
	}
}

// TestSummarizeEmptyDir 验证从未记过账（目录不存在）返回零值不报错。
func TestSummarizeEmptyDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	sum, err := Summarize(time.Now().AddDate(0, 0, -7), time.Now())
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if sum.Total.Calls != 0 {
		t.Fatalf("expected zero, got %+v", sum.Total)
	}
}

// TestLogDefaultSource 验证 Source 缺省落 "user"、TS 缺省取当前时间。
func TestLogDefaultSource(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	Log(Record{Model: "m1", In: 1, Out: 1})
	now := time.Now()
	sum, err := Summarize(now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if b := sum.BySource["user"]; b.Calls != 1 {
		t.Fatalf("默认 source 应为 user: %+v", sum.BySource)
	}
}
