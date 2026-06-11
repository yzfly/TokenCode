package tui

import (
	"encoding/json"
	"testing"

	"github.com/yzfly/tokencode/internal/permrules"
)

func TestPermsDecide(t *testing.T) {
	p := newPerms(modeReview)

	// read 在任何模式都放行。
	for _, m := range []permMode{modePlan, modeReview, modeYolo} {
		p.setMode(m)
		if p.decide("read") != permAllow {
			t.Fatalf("read should be allowed in mode %v", m)
		}
	}

	// review：写类工具需确认；'a' 记住后放行。
	p.setMode(modeReview)
	if p.decide("write") != permConfirm {
		t.Fatal("review: write should need confirm")
	}
	p.rememberAlways("write")
	if p.decide("write") != permAllow {
		t.Fatal("review: write should be allowed after rememberAlways")
	}
	if p.decide("bash") != permConfirm {
		t.Fatal("review: bash still needs confirm (only write remembered)")
	}

	// yolo：全放行。
	p.setMode(modeYolo)
	if p.decide("bash") != permAllow {
		t.Fatal("yolo: bash should be allowed")
	}

	// plan：read 之外一律拒绝。
	p.setMode(modePlan)
	if p.decide("write") != permReject {
		t.Fatal("plan: write should be rejected")
	}
	if p.decide("bash") != permReject {
		t.Fatal("plan: bash should be rejected")
	}
}

func TestPermsCycle(t *testing.T) {
	p := newPerms(modePlan)
	if got := p.cycle(); got != modeReview {
		t.Fatalf("plan→review, got %v", got)
	}
	if got := p.cycle(); got != modeAuto {
		t.Fatalf("review→auto, got %v", got)
	}
	if got := p.cycle(); got != modeYolo {
		t.Fatalf("auto→yolo, got %v", got)
	}
	if got := p.cycle(); got != modePlan {
		t.Fatalf("yolo→plan (wrap), got %v", got)
	}
}

func TestPermsAutoDecide(t *testing.T) {
	// auto 与 review 同样返回 permConfirm（bridge 决定走小模型还是人工）。
	p := newPerms(modeAuto)
	if p.decide("write") != permConfirm {
		t.Fatal("auto: write should map to permConfirm")
	}
	if p.decide("read") != permAllow {
		t.Fatal("auto: read should be allowed")
	}
}

func TestResolveGate(t *testing.T) {
	pr := permrules.NoMatch
	cases := []struct {
		name string
		rd   permrules.Decision
		pd   permDecision
		want gateAction
	}{
		// deny 永远拒，任何模式裁决都翻不了。
		{"deny beats yolo/allow", permrules.Deny, permAllow, gateReject},
		{"deny beats confirm", permrules.Deny, permConfirm, gateReject},
		{"deny beats plan reject", permrules.Deny, permReject, gateReject},
		// plan 只读铁律：规则 allow/ask 都突破不了。
		{"plan iron rule beats rule allow", permrules.Allow, permReject, gateReject},
		{"plan iron rule beats rule ask", permrules.Ask, permReject, gateReject},
		// ask 强制人工确认：yolo/记住放行（permAllow）也要问，auto 不许代答。
		{"ask forces human under yolo", permrules.Ask, permAllow, gateConfirmHuman},
		{"ask forces human under review/auto", permrules.Ask, permConfirm, gateConfirmHuman},
		// allow 跳过模式确认直接放行。
		{"rule allow skips confirm", permrules.Allow, permConfirm, gateAllow},
		{"rule allow under yolo", permrules.Allow, permAllow, gateAllow},
		// 不命中：完全回落模式默认。
		{"no-match falls back to mode allow", pr, permAllow, gateAllow},
		{"no-match falls back to mode confirm", pr, permConfirm, gateConfirmMode},
		{"no-match falls back to plan reject", pr, permReject, gateReject},
	}
	for _, c := range cases {
		if got := resolveGate(c.rd, c.pd); got != c.want {
			t.Errorf("%s: resolveGate(%v, %v) = %v, want %v", c.name, c.rd, c.pd, got, c.want)
		}
	}
}

func TestResolveGateWithRealRules(t *testing.T) {
	// 端到端：真实规则集 + 真实 perms，验证 gateTool 用的两个输入合成正确。
	rules, warns := permrules.Compile(permrules.Lists{
		Allow: []string{"bash(go test*)"},
		Ask:   []string{"bash(git push *)"},
		Deny:  []string{"bash(rm -rf *)"},
	})
	if len(warns) != 0 {
		t.Fatalf("unexpected warns: %v", warns)
	}
	in := func(cmd string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"command": cmd})
		return b
	}
	p := newPerms(modeReview)
	// review：allow 规则跳过确认；deny 直接拒；ask 强制人工；不命中走确认。
	if got := resolveGate(rules.Evaluate("bash", in("go test ./...")), p.decide("bash")); got != gateAllow {
		t.Fatalf("review + rule allow = %v, want gateAllow", got)
	}
	if got := resolveGate(rules.Evaluate("bash", in("rm -rf /")), p.decide("bash")); got != gateReject {
		t.Fatalf("review + rule deny = %v, want gateReject", got)
	}
	if got := resolveGate(rules.Evaluate("bash", in("git push origin")), p.decide("bash")); got != gateConfirmHuman {
		t.Fatalf("review + rule ask = %v, want gateConfirmHuman", got)
	}
	if got := resolveGate(rules.Evaluate("bash", in("make build")), p.decide("bash")); got != gateConfirmMode {
		t.Fatalf("review + no-match = %v, want gateConfirmMode", got)
	}
	// yolo：deny 仍拒、ask 仍问。
	p.setMode(modeYolo)
	if got := resolveGate(rules.Evaluate("bash", in("rm -rf /")), p.decide("bash")); got != gateReject {
		t.Fatalf("yolo + rule deny = %v, want gateReject", got)
	}
	if got := resolveGate(rules.Evaluate("bash", in("git push origin")), p.decide("bash")); got != gateConfirmHuman {
		t.Fatalf("yolo + rule ask = %v, want gateConfirmHuman", got)
	}
	// plan：规则 allow 也突破不了只读铁律。
	p.setMode(modePlan)
	if got := resolveGate(rules.Evaluate("bash", in("go test ./...")), p.decide("bash")); got != gateReject {
		t.Fatalf("plan + rule allow = %v, want gateReject (只读铁律)", got)
	}
}
