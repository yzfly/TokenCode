package tui

import "testing"

func TestStatusIndicator(t *testing.T) {
	m := model{}
	if got := m.statusIndicator(); got != "" {
		t.Errorf("idle: %q", got)
	}
	m.thinking = true
	if got := m.statusIndicator(); got != "thinking…" {
		t.Errorf("thinking: %q", got)
	}
	m.thinking = false
	m.state = stateRunning
	if got := m.statusIndicator(); got != "working…" {
		t.Errorf("working generic: %q", got)
	}
	m.workingOn = "[explore] bash"
	if got := m.statusIndicator(); got != "working · [explore] bash…" {
		t.Errorf("working tool: %q", got)
	}
	// 后台 turn：state 仍 idle，只看 thinking。
	m.state = stateIdle
	m.workingOn = "bash"
	if got := m.statusIndicator(); got != "" {
		t.Errorf("background tool exec should be silent: %q", got)
	}
}
