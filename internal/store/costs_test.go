package store

import (
	"math"
	"testing"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

func fill(t *testing.T, s *Store, id, side string, qty, price, fee float64, mode protocol.Mode) {
	t.Helper()
	err := s.RecordOrder(ctx, Order{
		ID: id, IntentID: "i-" + id, Phase: "filled", Symbol: sym("005930"),
		Side: side, Qty: qty, Price: price, FeeKRW: fee, Mode: mode, Source: SourceBot,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestObservedCostIsEmptyWithoutLiveFills(t *testing.T) {
	s := open(t)
	obs, err := s.ObservedCost(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// ★ "관측 없음" 과 "0bp" 는 다른 사실이다. 합치면 수수료가 공짜인 것처럼 보인다.
	if obs.HasBuy() || obs.HasSell() {
		t.Fatalf("체결이 없는데 관측이 있다고 한다: %+v", obs)
	}
}

func TestObservedCostIgnoresPaper(t *testing.T) {
	// ★★ paper 의 fee 는 **우리가 가정한 요율로 계산한 값**이다. 그걸로 요율을 도출하면
	//    가정이 자기 자신을 확인하는 순환이 되고, 틀려도 언제나 일관돼 보인다.
	s := open(t)
	fill(t, s, "p1", "buy", 10, 70000, 1050, protocol.ModePaper) // 15bp 짜리 가짜
	obs, err := s.ObservedCost(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if obs.HasBuy() {
		t.Fatalf("paper 체결로 요율을 도출했다: %+v", obs)
	}
}

func TestObservedCostDerivesAsymmetricRates(t *testing.T) {
	s := open(t)
	// 매수 700,000원에 fee 105원 = 1.5bp (위탁수수료만)
	fill(t, s, "b1", "buy", 10, 70000, 105, protocol.ModeLive)
	// 매도 710,000원에 fee 1,491원 = 21bp (수수료 + 거래세)
	fill(t, s, "s1", "sell", 10, 71000, 1491, protocol.ModeLive)

	obs, err := s.ObservedCost(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.HasBuy() || !obs.HasSell() {
		t.Fatalf("관측이 비었다: %+v", obs)
	}
	if math.Abs(obs.BuyBp-1.5) > 1e-6 {
		t.Fatalf("매수 bp = %v, 기대 1.5", obs.BuyBp)
	}
	if math.Abs(obs.SellBp-21.0) > 1e-6 {
		t.Fatalf("매도 bp = %v, 기대 21", obs.SellBp)
	}
	// ★ 이 비대칭이 요점이다 — 하나의 상수로 뭉개면 매수에 없는 비용이 붙는다.
	if obs.BuyBp >= obs.SellBp {
		t.Fatal("국내 구조상 매도 비용이 매수보다 커야 한다 (거래세)")
	}
}

func TestObservedCostWeightsByNotional(t *testing.T) {
	// ★ 건수 평균이 아니라 **금액 가중**이어야 한다. 작은 체결의 반올림이 요율을 흔들면
	//   관측치가 설정과 계속 어긋나 경고만 울리는 늑대소년이 된다.
	s := open(t)
	fill(t, s, "b1", "buy", 1, 1000, 1, protocol.ModeLive)     // 10bp, 1,000원
	fill(t, s, "b2", "buy", 100, 1000, 100, protocol.ModeLive) // 1bp, 100,000원
	obs, err := s.ObservedCost(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := (1.0 + 100.0) / (1000.0 + 100000.0) * 10000 // ≈ 1.0bp
	if math.Abs(obs.BuyBp-want) > 1e-6 {
		t.Fatalf("매수 bp = %v, 기대 %v (금액 가중)", obs.BuyBp, want)
	}
	if obs.BuyFills != 2 {
		t.Fatalf("체결 수 = %d, 기대 2", obs.BuyFills)
	}
}
