package tui

import "testing"

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
	if got := p.cycle(); got != modeYolo {
		t.Fatalf("review→yolo, got %v", got)
	}
	if got := p.cycle(); got != modePlan {
		t.Fatalf("yolo→plan (wrap), got %v", got)
	}
}
