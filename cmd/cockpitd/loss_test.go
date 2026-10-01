package main

import "testing"

func TestDecideLoss(t *testing.T) {
	// 한도 넘음 → 건다
	if d := decideLoss(500_000, -512_000, "2026-10-01", false, ""); !d.trip {
		t.Fatalf("%+v", d)
	}
	// 아직 → 아무것도 안 함
	if d := decideLoss(500_000, -100_000, "2026-10-01", false, ""); d.trip || d.reset {
		t.Fatalf("%+v", d)
	}
	reason := lossPrefix + "2026-10-01: ..."
	// 같은 날 → 유지
	if d := decideLoss(500_000, 0, "2026-10-01", true, reason); d.trip || d.reset {
		t.Fatalf("%+v", d)
	}
	// 다음 날 → 푼다
	if d := decideLoss(500_000, 0, "2026-10-02", true, reason); !d.reset {
		t.Fatalf("%+v", d)
	}
	// ★ 다른 사유(de-risk)로 걸린 건 건드리지 않는다
	if d := decideLoss(500_000, 0, "2026-10-02", true, "operator derisk"); d.trip || d.reset {
		t.Fatalf("%+v", d)
	}
	// 한도를 끄면 내가 건 건 푼다
	if d := decideLoss(0, -9e9, "2026-10-01", true, reason); !d.reset {
		t.Fatalf("%+v", d)
	}
}
