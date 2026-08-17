package paper

import (
	"context"
	"testing"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// ★★ 시세원 없이 뜬 paper 에서도 **청산은 나가야 한다** (2026-08-17).
//
// `docs/flat6.md §5` 가 처방한 검증 배치가 정확히 이 상태(paper + 시세원 없음)인데, 옛 코드는
// 매도에서 `모르는 종목` 으로 매 틱 실패했다. 진입에는 `ref_price` 우회로가 있어 통과하는데
// 청산에는 없어서 **비대칭이 거꾸로 걸려 있었다** — 진입 실패는 기회 상실(유한)이고 청산
// 실패는 손실 노출(무한)인데 막힌 쪽이 청산이었다.
func newQuoteless() *Broker {
	return New(Config{Cash: 10_000_000, Lot: 1, Price: nil}) // ★ 시세원 없음
}

var sym = protocol.Symbol{Exchange: "KRX", Code: "005930"}

func TestSellClosesWithoutQuoteSource(t *testing.T) {
	ctx := context.Background()
	b := newQuoteless()

	// 진입은 ref_price 로 통과한다 (그게 휴장에도 배선 검증이 되는 이유다)
	if _, err := b.Buy(ctx, broker.OrderRequest{Symbol: sym, Qty: 10, RefPrice: 70_000}); err != nil {
		t.Fatalf("ref_price 를 실었는데 진입이 막혔다: %v", err)
	}

	fill, err := b.Sell(ctx, broker.OrderRequest{Symbol: sym, Qty: 10})
	if err != nil {
		t.Fatalf("★ 시세가 없다고 청산이 막혔다 — 무한 손실 쪽이 막히면 안 된다: %v", err)
	}
	if fill.Price <= 0 {
		t.Fatalf("체결가가 없다: %+v", fill)
	}
	if fill.Detail == "" {
		t.Fatal("★ 근사로 닫았으면 그 사실이 원장에 남아야 한다 (조용한 근사 금지)")
	}
	if len(b.holdings) != 0 {
		t.Fatalf("포지션이 안 닫혔다: %+v", b.holdings)
	}
}

// ★ 시세가 **있으면** 그걸 쓴다 — 평단 폴백은 어디까지나 마지막 수단이다.
func TestSellPrefersQuoteOverAvgPrice(t *testing.T) {
	ctx := context.Background()
	b := New(Config{
		Cash: 10_000_000, Lot: 1,
		Price: func(protocol.Symbol) (float64, bool) { return 77_000, true },
	})
	if _, err := b.Buy(ctx, broker.OrderRequest{Symbol: sym, Qty: 10, RefPrice: 70_000}); err != nil {
		t.Fatal(err)
	}
	fill, err := b.Sell(ctx, broker.OrderRequest{Symbol: sym, Qty: 10})
	if err != nil {
		t.Fatal(err)
	}
	if fill.Price != 77_000 {
		t.Fatalf("시세가 있는데 평단으로 닫았다: %.0f", fill.Price)
	}
	if fill.Detail != "" {
		t.Fatalf("정상 경로인데 단서가 붙었다: %q", fill.Detail)
	}
}

// ★ 보유가 없으면 여전히 에러다 — 폴백이 "모르는 종목" 을 삼켜선 안 된다.
func TestSellStillFailsForUnknownSymbol(t *testing.T) {
	b := newQuoteless()
	if _, err := b.Sell(context.Background(),
		broker.OrderRequest{Symbol: protocol.Symbol{Exchange: "KRX", Code: "999999"}, Qty: 1}); err == nil {
		t.Fatal("보유도 시세도 없는데 청산이 성공했다")
	}
}
