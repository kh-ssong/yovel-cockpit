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
