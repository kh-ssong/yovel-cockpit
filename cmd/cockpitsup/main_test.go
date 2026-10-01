package main

import (
	"errors"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/proc"
)

var t0 = time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)

func TestHung(t *testing.T) {
	grace, hang := 2*time.Minute, 3*time.Minute
	ok := proc.Beat{TS: t0.Add(4 * time.Minute), LastTick: t0.Add(4 * time.Minute)}
	if s, _ := hung(t0.Add(time.Minute), t0, proc.Beat{}, errors.New("x"), grace, hang); s {
		t.Fatal("유예 중에 멈춤 판정")
	}
	if s, _ := hung(t0.Add(5*time.Minute), t0, ok, nil, grace, hang); s {
		t.Fatal("건강한데 멈춤")
	}
	// ★ 하트비트(ts)는 계속 가는데 집행 루프만 멈췄다 — 살아 있는데 멈춘 프로세스.
	stale := proc.Beat{TS: t0.Add(10 * time.Minute), LastTick: t0.Add(5 * time.Minute)}
	if s, why := hung(t0.Add(10*time.Minute), t0, stale, nil, grace, hang); !s || why == "" {
		t.Fatal("멈춘 루프를 못 봤다")
	}
	if s, _ := hung(t0.Add(3*time.Minute), t0, proc.Beat{TS: t0.Add(-time.Hour)}, nil, grace, hang); !s {
		t.Fatal("지난 실행의 하트비트를 지금 것으로 믿었다")
	}
}

func TestPolicyBackoffAndCrashLoop(t *testing.T) {
	var p policy
	n := p.onExit(t0, 2*time.Hour) // 오래 돌다 죽음 → 곧바로
	if n.wait != 5*time.Second || n.halt {
		t.Fatalf("%+v", n)
	}
	now := t0
	var last next
	for i := 0; i < 4; i++ { // 금방 죽기를 반복
		now = now.Add(30 * time.Second)
		last = p.onExit(now, 10*time.Second)
	}
	if !last.halt || p.haltedUntil(now).IsZero() {
		t.Fatalf("크래시 루프인데 안 쉰다 %+v", last)
	}
	if !p.haltedUntil(now.Add(31 * time.Minute)).IsZero() {
		t.Fatal("30분 뒤에도 쉰다")
	}
}
