package kiwoom

import (
	"testing"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
)

// 집행 방식 sweep 의 조각 = IOC 지정가 (trde_tp 10). 매수는 호가단위로 내리고 매도는 올린다 (지정가와 같은 규칙).
func TestIOCOrderType(t *testing.T) {
	f := newFake()
	f.on(apiBuy, func(map[string]any) any { return map[string]any{"return_code": 0, "ord_no": "b-1"} })
	f.on(apiSell, func(map[string]any) any { return map[string]any{"return_code": 0, "ord_no": "s-1"} })
	b, _ := newBroker(t, f)

	if _, err := b.SubmitBuy(ctx, broker.OrderRequest{Symbol: sym, Qty: 3, RefPrice: 71_234,
		LimitPrice: 71_234, IOC: true}); err != nil {
		t.Fatal(err)
	}
	body := f.bodies(apiBuy)[0]
	if body["trde_tp"] != tradeLimitIOC || body["ord_uv"] != "71200" {
		t.Fatalf("매수 IOC 바디 %v — trde_tp 10 · 71200(내림) 이어야 한다", body)
	}

	if _, err := b.SubmitSell(ctx, broker.OrderRequest{Symbol: sym, Qty: 3, LimitPrice: 71_234, IOC: true}); err != nil {
		t.Fatal(err)
	}
	body = f.bodies(apiSell)[0]
	if body["trde_tp"] != tradeLimitIOC || body["ord_uv"] != "71300" {
		t.Fatalf("매도 IOC 바디 %v — trde_tp 10 · 71300(올림) 이어야 한다", body)
	}

	// 지정가 없는 IOC 는 내지 않는다 (시장가 IOC 는 이 집행의 뜻이 아니다).
	if _, err := b.SubmitSell(ctx, broker.OrderRequest{Symbol: sym, Qty: 3, IOC: true}); err == nil {
		t.Fatal("지정가 없는 IOC 를 냈다")
	}
	if n := len(f.bodies(apiSell)); n != 1 {
		t.Fatalf("매도 주문 %d회 — 거부한 IOC 가 나갔다", n)
	}
	// IOC 가 아니면 예전 그대로 (시장가 3 · 지정가 0).
	if _, err := b.SubmitBuy(ctx, broker.OrderRequest{Symbol: sym, Qty: 1, RefPrice: 1000}); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies(apiBuy)[1]["trde_tp"]; got != tradeMarket {
		t.Fatalf("시장가 trde_tp %v", got)
	}
}

// ka10004 — 부호 붙은 가격을 절대값으로, 잔량은 그대로. 한쪽이 비면 0 (그 값에 살 수 있다고 읽지 않는다).
func TestBookParsesBestQuotes(t *testing.T) {
	f := newFake()
	f.on(apiBook, func(map[string]any) any {
		return map[string]any{"return_code": 0, "bid_req_base_tm": "091503",
			"sel_fpr_bid": "+60850", "sel_fpr_req": "1,234", "buy_fpr_bid": "-60845", "buy_fpr_req": "77"}
	})
	b, _ := newBroker(t, f)
	bk, err := b.Book(ctx, sym)
	if err != nil {
		t.Fatal(err)
	}
	if bk.Ask1 != 60850 || bk.Ask1Qty != 1234 || bk.Bid1 != 60845 || bk.Bid1Qty != 77 {
		t.Fatalf("호가 %+v", bk)
	}
	if bk.AsOf.In(kst).Format("150405") != "091503" {
		t.Fatalf("호가 시각 %v — 호가잔량기준시간이어야 한다", bk.AsOf.In(kst))
	}
	if got := f.bodies(apiBook)[0]["stk_cd"]; got != sym.Code {
		t.Fatalf("종목 %v", got)
	}

	// 상한가 — 매도 호가 없음.
	f.on(apiBook, func(map[string]any) any {
		return map[string]any{"return_code": 0, "sel_fpr_bid": "", "sel_fpr_req": "", "buy_fpr_bid": "+79000", "buy_fpr_req": "9"}
	})
	bk, err = b.Book(ctx, sym)
	if err != nil || bk.Ask1 != 0 || bk.Bid1 != 79000 {
		t.Fatalf("한쪽 빈 호가 %+v %v", bk, err)
	}

	// 양쪽 다 비면 모른다 (오류).
	f.on(apiBook, func(map[string]any) any { return map[string]any{"return_code": 0} })
	if _, err := b.Book(ctx, sym); err == nil {
		t.Fatal("빈 호가를 성공으로 읽었다")
	}
}
