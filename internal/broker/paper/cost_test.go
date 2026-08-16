package paper

import (
	"context"
	"math"
	"testing"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

func newBroker(buyBp, sellBp, slipBp float64) *Broker {
	return New(Config{
		Cash: 10_000_000, Lot: 1,
		FeeBpBuy: buyBp, FeeBpSell: sellBp, SlipBp: slipBp,
		Price: func(protocol.Symbol) (float64, bool) { return 70000, true },
	})
}

// ★★ 국내 구조 = 매수엔 위탁수수료만, 매도엔 거래세가 얹힌다. 하나의 상수로 뭉개면
// 매수 체결에 **존재하지 않는 비용**이 붙는다 (옛 대칭 15bp 가 정확히 그랬다).
func TestFeeIsAsymmetric(t *testing.T) {
	ctx := context.Background()
	b := newBroker(0, 20, 0)
	s := protocol.Symbol{Exchange: "KRX", Code: "005930"}

	buy, err := b.Buy(ctx, broker.OrderRequest{Symbol: s, Qty: 10, RefPrice: 70000})
	if err != nil {
		t.Fatal(err)
	}
	if buy.FeeKRW != 0 {
		t.Fatalf("매수 수수료 = %v, 기대 0 (매수엔 거래세가 없다)", buy.FeeKRW)
	}

	sell, err := b.Sell(ctx, broker.OrderRequest{Symbol: s, Qty: 10, RefPrice: 71000})
	if err != nil {
		t.Fatal(err)
	}
	want := 71000 * 10 * 20.0 / 10000 // 20bp
	if math.Abs(sell.FeeKRW-want) > 1e-6 {
		t.Fatalf("매도 수수료 = %v, 기대 %v", sell.FeeKRW, want)
	}
}

// ★ 슬리피지는 0 으로 두지 않는다 — 비용 0 시뮬은 손익분기 근처 전략의 판정을 뒤집는다.
func TestSlippageIsChargedBothWays(t *testing.T) {
	ctx := context.Background()
	b := newBroker(0, 0, 10)
	s := protocol.Symbol{Exchange: "KRX", Code: "005930"}

	buy, err := b.Buy(ctx, broker.OrderRequest{Symbol: s, Qty: 1, RefPrice: 70000})
	if err != nil {
		t.Fatal(err)
	}
	if buy.Price <= 70000 {
		t.Fatalf("매수 체결가 %v — 기준가보다 비싸야 한다", buy.Price)
	}
	sell, err := b.Sell(ctx, broker.OrderRequest{Symbol: s, Qty: 1, RefPrice: 70000})
	if err != nil {
		t.Fatal(err)
	}
	if sell.Price >= 70000 {
		t.Fatalf("매도 체결가 %v — 기준가보다 싸야 한다", sell.Price)
	}
}

// ★★ paper 가 TP 지정가를 실제로 체결시키는가 — 이게 없으면 승자가 전부 시간청산까지
// 끌려가서 손익 분포가 라이브와 구조적으로 달라진다 (reflex 페이퍼엔 없던 문제).
func TestSettleLimitsFillsWhenPriceReaches(t *testing.T) {
	ctx := context.Background()
	px := 70000.0
	b := New(Config{
		Cash: 10_000_000, Lot: 1, FeeBpBuy: 0, FeeBpSell: 20, SlipBp: 0,
		Price: func(protocol.Symbol) (float64, bool) { return px, true },
	})
	s := protocol.Symbol{Exchange: "KRX", Code: "005930"}

	if _, err := b.Buy(ctx, broker.OrderRequest{Symbol: s, Qty: 10, RefPrice: 70000}); err != nil {
		t.Fatal(err)
	}
	id, err := b.PlaceTP(ctx, s, 10, 72100)
	if err != nil {
		t.Fatal(err)
	}

	// 아직 안 닿았다 → 체결 없음
	fills, err := b.SettleLimits(ctx)
	if err != nil || len(fills) != 0 {
		t.Fatalf("가격이 안 닿았는데 체결됐다: %v %v", fills, err)
	}

	px = 72500 // 지정가를 넘어섰다
	fills, err = b.SettleLimits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 1 {
		t.Fatalf("체결 %d건, 기대 1건", len(fills))
	}
	f := fills[0]
	// ★ 체결가는 **지정가 그대로**다. 넘어선 만큼 더 받지 않는다 = 우리에게 불리한 쪽.
	if f.Price != 72100 {
		t.Fatalf("체결가 %v, 기대 72100 (지정가)", f.Price)
	}
	if f.OrderID != id || f.Symbol.Code != "005930" {
		t.Fatalf("체결 귀속이 틀렸다: %+v", f)
	}
	if f.SlippageBp != 0 {
		t.Fatalf("지정가 슬리피지 %v, 기대 0", f.SlippageBp)
	}
	want := 72100 * 10 * 20.0 / 10000
	if math.Abs(f.FeeKRW-want) > 1e-6 {
		t.Fatalf("수수료 %v, 기대 %v", f.FeeKRW, want)
	}

	// 포지션이 사라지고 현금이 늘었다 = "계좌가 불어나는지" 가 보인다
	pos, _ := b.Positions(ctx)
	if len(pos) != 0 {
		t.Fatalf("체결 후에도 포지션이 남았다: %+v", pos)
	}
	cash, _ := b.Cash(ctx)
	if cash.Deposit <= 10_000_000 {
		t.Fatalf("현금 %v — 익절했는데 안 늘었다", cash.Deposit)
	}
	if b.OpenTPOrders() != 0 {
		t.Fatal("체결된 지정가가 주문서에 남아 있다")
	}

	// 두 번 체결되지 않는다 (멱등)
	again, _ := b.SettleLimits(ctx)
	if len(again) != 0 {
		t.Fatalf("같은 지정가가 두 번 체결됐다: %+v", again)
	}
}

// ★ 시세원이 없으면 체결 판정 자체가 불가 — 조용히 아무것도 안 한다 (낙관 금지).
func TestSettleLimitsNoPriceSourceDoesNothing(t *testing.T) {
	b := New(Config{Cash: 1_000_000, Lot: 1})
	fills, err := b.SettleLimits(context.Background())
	if err != nil || len(fills) != 0 {
		t.Fatalf("시세원 없이 체결됐다: %v %v", fills, err)
	}
}
