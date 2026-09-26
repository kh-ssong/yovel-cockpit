package main

import (
	"strings"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
)

// ★ reflex 2026-08-04 재현: 예수금은 멀쩡해 보이는데 해외 원화배정으로 현금 한도(L1)가 빠졌다.
// Deposit 으로 비교하면 통과해 버린다 — Seed 로 봐야 걸린다.
func TestBudgetUsesSeedNotDeposit(t *testing.T) {
	ok, msg := budgetVerdict(16_000_000, 0, broker.Cash{Deposit: 18_000_000, Orderable: 4_000_000, Seed: 4_400_000})
	if ok || !strings.Contains(msg, "28%") {
		t.Fatalf("잡지 못했다: %v %q", ok, msg)
	}
}

// ★ 보유 증거금이 Orderable 을 깎아도 Seed 가 충분하면 통과다 (reflex 시드 −49% 오판 재현 금지).
func TestBudgetIgnoresOrderableShrink(t *testing.T) {
	if ok, _ := budgetVerdict(1_400_000, 0, broker.Cash{Orderable: 94_352, Seed: 1_427_620}); !ok {
		t.Fatal("Orderable 로 판정했다")
	}
}

// 이미 산 자리는 현금이 아니라 주식으로 있다 — 장중 재시작에서 오경보를 내지 않는다.
func TestBudgetSubtractsOpenPositions(t *testing.T) {
	if ok, msg := budgetVerdict(16_000_000, 12_000_000, broker.Cash{Seed: 4_500_000}); !ok {
		t.Fatalf("보유분을 빼지 않았다: %s", msg)
	}
}

func TestBudgetUnknownSeedIsNotAlarm(t *testing.T) {
	ok, msg := budgetVerdict(1, 0, broker.Cash{})
	if !ok || msg == "" {
		t.Fatalf("%v %q", ok, msg)
	}
}

func TestNextBudgetCheck(t *testing.T) {
	loc := time.FixedZone("KST", 9*3600)
	cases := []struct{ now, want time.Time }{
		{time.Date(2026, 9, 26, 7, 0, 0, 0, loc), time.Date(2026, 9, 26, 8, 50, 0, 0, loc)},
		{time.Date(2026, 9, 26, 8, 50, 0, 0, loc), time.Date(2026, 9, 27, 8, 50, 0, 0, loc)},
		{time.Date(2026, 9, 26, 15, 0, 0, 0, loc), time.Date(2026, 9, 27, 8, 50, 0, 0, loc)},
	}
	for _, c := range cases {
		if got := nextBudgetCheck(c.now); !got.Equal(c.want) {
			t.Errorf("%v → %v, 기대 %v", c.now, got, c.want)
		}
	}
}
