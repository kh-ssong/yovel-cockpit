package paper

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
)

// ★ 코인은 소수 수량이다. paper 가 "1주 단위" 만 알면 사이징이 0 이 되어 영영 못 산다.
func TestCoinPaperRoundTrip(t *testing.T) {
	c := context.Background()
	btc := protocol.Symbol{Exchange: "UPBIT", Code: "KRW-BTC"}
	b := New(Config{
		Cash: 1_000_000, Lot: 1, Now: func() time.Time { return time.Unix(0, 0) },
		Price:    func(protocol.Symbol) (float64, bool) { return 114_438_000, true },
		FeeBpBuy: 1.5, FeeBpSell: 21.5, // 주식 요율
		Rules: map[string]Rule{"UPBIT": {Lot: 1e-8, MinOrderValue: 5000, FeeBpBuy: 5, FeeBpSell: 5}},
	})
	if b.LotSize(btc) != 1e-8 || b.MinOrderValue(btc) != 5000 {
		t.Fatalf("규칙 %v %v", b.LotSize(btc), b.MinOrderValue(btc))
	}
	if b.LotSize(protocol.Symbol{Exchange: "KRX", Code: "005930"}) != 1 {
		t.Fatal("주식 규칙이 바뀌었다")
	}

	res := sizing.Shares(0.1, 1_000_000, 114_438_000,
		sizing.Market{LotSize: b.LotSize(btc), MinOrderValue: b.MinOrderValue(btc)})
	if res.Qty <= 0 || res.Qty >= 1 {
		t.Fatalf("소수 수량이 안 나왔다: %+v", res)
	}

	fill, err := b.Buy(c, broker.OrderRequest{Symbol: btc, Qty: res.Qty, RefPrice: 114_438_000})
	if err != nil {
		t.Fatal(err)
	}
	sell, err := b.Sell(c, broker.OrderRequest{Symbol: btc, Qty: fill.Qty})
	if err != nil {
		t.Fatal(err)
	}
	// ★ 코인 매도에 주식 매도세가 붙으면 안 된다.
	if want := sell.Qty * sell.Price * 5 / 10000; sell.FeeKRW < want-1e-6 || sell.FeeKRW > want+1e-6 {
		t.Fatalf("코인 매도 수수료 %v, 기대 %v (5bp)", sell.FeeKRW, want)
	}
	if hs, _ := b.Positions(c); len(hs) != 0 {
		t.Fatalf("다 팔았는데 먼지가 남았다: %+v", hs)
	}
}

// ★ 같은 코인을 여러 장부가 같은 수량으로 사고 따로 팔면, 보유(로트의 합)에서 로트를 빼는 사이
// float 오차가 쌓여 마지막 로트가 보유보다 4e-11 커진다. 그걸 «매도가능 부족» 으로 거부하면
// 그 장부는 영영 청산되지 않는다 (2026-10-17 맥 paper KRW-ICX 실측 수치 그대로).
func TestSharedCoinLastLotStillSells(t *testing.T) {
	c := context.Background()
	icx := protocol.Symbol{Exchange: "UPBIT", Code: "KRW-ICX"}
	b := New(Config{
		Cash: 10_000_000, Lot: 1, Now: func() time.Time { return time.Unix(0, 0) },
		Rules: map[string]Rule{"UPBIT": {Lot: 1e-8, MinOrderValue: 5000, FeeBpBuy: 5, FeeBpSell: 5}},
	})
	const lot = 69444.44444444
	for i := 0; i < 3; i++ {
		if _, err := b.Buy(c, broker.OrderRequest{Symbol: icx, Qty: lot, RefPrice: 14.4}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := b.Sell(c, broker.OrderRequest{Symbol: icx, Qty: lot, RefPrice: 14.6}); err != nil {
			t.Fatalf("%d번째 매도: %v", i+1, err)
		}
	}
	if hs, _ := b.Positions(c); len(hs) != 0 {
		t.Fatalf("다 팔았는데 먼지가 남았다: %+v", hs)
	}
	// 반 단위 넘게 모자라면 여전히 거부한다.
	if _, err := b.Buy(c, broker.OrderRequest{Symbol: icx, Qty: lot, RefPrice: 14.4}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Sell(c, broker.OrderRequest{Symbol: icx, Qty: lot + 1e-8, RefPrice: 14.6}); !errors.Is(err, broker.ErrNotEnoughShare) {
		t.Fatalf("한 단위 더 팔았는데 거부 안 됨: %v", err)
	}
}
