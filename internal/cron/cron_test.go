package cron

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCreateValidation(t *testing.T) {
	m := NewManager(func(string, string) {})
	defer m.Close()

	if err := m.Create("", time.Hour, "p"); err == nil {
		t.Error("empty name must fail")
	}
	if err := m.Create("a", time.Second, "p"); err == nil {
		t.Error("interval below MinEvery must fail")
	}
	if err := m.Create("a", time.Hour, ""); err == nil {
		t.Error("empty prompt must fail")
	}
	if err := m.Create("a", time.Hour, "p"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.Create("a", time.Hour, "p"); err == nil {
		t.Error("duplicate name must fail")
	}
}

func TestFireAndDelete(t *testing.T) {
	old := MinEvery
	MinEvery = 10 * time.Millisecond
	defer func() { MinEvery = old }()

	fired := make(chan string, 4)
	m := NewManager(func(name, prompt string) { fired <- name + ":" + prompt })
	defer m.Close()

	if err := m.Create("tick", 20*time.Millisecond, "check"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-fired:
		if got != "tick:check" {
			t.Errorf("fired = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timer never fired")
	}

	// 周期任务会再次触发。
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("timer did not reschedule")
	}

	if err := m.Delete("tick"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("tick"); err == nil {
		t.Error("double delete must fail")
	}
	if got := len(m.List()); got != 0 {
		t.Errorf("entries after delete = %d", got)
	}
}

func TestToolsRoundTrip(t *testing.T) {
	m := NewManager(func(string, string) {})
	defer m.Close()
	ts := Tools(m)
	byName := map[string]interface {
		Execute(context.Context, json.RawMessage) (string, error)
	}{}
	for _, tool := range ts {
		byName[tool.Name()] = tool
	}

	in, _ := json.Marshal(map[string]any{"name": "watch", "every": "5m", "prompt": "look around"})
	if _, err := byName["cron_create"].Execute(context.Background(), in); err != nil {
		t.Fatalf("cron_create: %v", err)
	}

	out, err := byName["cron_list"].Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("cron_list: %v", err)
	}
	if !strings.Contains(out, "watch") || !strings.Contains(out, "5m") {
		t.Errorf("cron_list output:\n%s", out)
	}

	in, _ = json.Marshal(map[string]any{"name": "watch"})
	if _, err := byName["cron_delete"].Execute(context.Background(), in); err != nil {
		t.Fatalf("cron_delete: %v", err)
	}
	out, _ = byName["cron_list"].Execute(context.Background(), nil)
	if !strings.Contains(out, "没有定时任务") {
		t.Errorf("after delete:\n%s", out)
	}
}
