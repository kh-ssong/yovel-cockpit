package kiwoom

import (
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

var etf = protocol.Symbol{Exchange: "KRX", Code: "069500"}

func bookResp(ask, bid string) func(map[string]any) any {
	return func(map[string]any) any {
		return map[string]any{"return_code": 0, "bid_req_base_tm": "100000",
			"sel_fpr_bid": ask, "sel_fpr_req": "100", "buy_fpr_bid": bid, "buy_fpr_req": "100"}
	}
}

func onOrders(f *fakeKiwoom) {
	f.on(apiBuy, func(map[string]any) any { return map[string]any{"return_code": 0, "ord_no": "b"} })
	f.on(apiSell, func(map[string]any) any { return map[string]any{"return_code": 0, "ord_no": "s"} })
}

// 주식 호가는 언제나 주식 단위 위에 있다 (경계값에서 아래 대역 단위를 써도 그 단위가 약수라 격자 위).
// 그러니 주식 단위 밖 가격 = ETP 의 증거다.
func TestOffStockGrid(t *testing.T) {
	for _, p := range []float64{0, 999, 1999, 2000, 4995, 5000, 9990, 49950, 50000, 83600, 199900, 200000, 500000, 501000} {
		if offStockGrid(p) {
			t.Errorf("%v 는 주식 호가로 나올 수 있다", p)
		}
	}
	for _, p := range []float64{5005, 9985, 20005, 49995, 83615, 83605, 200005} {
		if !offStockGrid(p) {
			t.Errorf("%v 는 주식 호가로 나올 수 없다 (ETP 증거)", p)
		}
	}
}

// ★ 2026-11-05~11 실사고 재현: 069500 (ETF) cap 83,615 매수 IOC 가 83,600 으로 깎여 매도1 에 못 닿았다.
// 호가로 ETF 를 알아보면 5원 단위로 낸다 — 매수 내림 · 매도 올림 · TP 올림 전부.
func TestETFLimitUsesFiveWonTick(t *testing.T) {
	f := newFake()
	onOrders(f)
	f.on(apiBook, bookResp("+83615", "+83610"))
	b, _ := newBroker(t, f)

	if _, err := b.SubmitBuy(ctx, broker.OrderRequest{Symbol: etf, Qty: 3, RefPrice: 83_615, LimitPrice: 83_615, IOC: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies(apiBuy)[0]["ord_uv"]; got != "83615" {
		t.Fatalf("ETF 매수 IOC 지정가 %v — 83615 (5원 단위) 여야 한다 (옛 버그 83600)", got)
	}
	if _, err := b.SubmitSell(ctx, broker.OrderRequest{Symbol: etf, Qty: 3, LimitPrice: 83_842, IOC: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies(apiSell)[0]["ord_uv"]; got != "83845" {
		t.Fatalf("ETF 매도 IOC 지정가 %v — 83845 여야 한다 (옛 버그 83900)", got)
	}
	if _, err := b.PlaceTP(ctx, etf, 3, 84_001); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies(apiSell)[1]["ord_uv"]; got != "84005" {
		t.Fatalf("ETF TP %v — 84005 여야 한다 (옛 버그 84100)", got)
	}
	// 호가는 처음 한 번만 읽는다.
	if n := len(f.bodies(apiBook)); n != 1 {
		t.Fatalf("호가 조회 %d회 — 판정은 한 번이면 된다", n)
	}
}

// sweep 은 IOC 전에 이미 호가를 읽는다 — 그 호가로 판정하고 주문 전에 또 읽지 않는다 (호가→주문 간격을 늘리지 않는다).
func TestSweepBookDecidesWithoutExtraQuery(t *testing.T) {
	f := newFake()
	onOrders(f)
	f.on(apiBook, bookResp("83615", "83610"))
	b, _ := newBroker(t, f)

	if _, err := b.Book(ctx, etf); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SubmitBuy(ctx, broker.OrderRequest{Symbol: etf, Qty: 1, RefPrice: 83_615, LimitPrice: 83_615, IOC: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies(apiBuy)[0]["ord_uv"]; got != "83615" {
		t.Fatalf("지정가 %v", got)
	}
	if n := len(f.bodies(apiBook)); n != 1 {
		t.Fatalf("호가 조회 %d회 — sweep 이 읽은 호가로 충분하다", n)
	}

	// 주식도 마찬가지 — 읽은 호가가 주식 격자 위면 주식 단위 그대로, 다시 묻지 않는다.
	f.on(apiBook, bookResp("71300", "71200"))
	if _, err := b.Book(ctx, sym); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SubmitBuy(ctx, broker.OrderRequest{Symbol: sym, Qty: 1, RefPrice: 71_234, LimitPrice: 71_234, IOC: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies(apiBuy)[1]["ord_uv"]; got != "71200" {
		t.Fatalf("주식 지정가 %v — 71200 (100원 내림) 이어야 한다", got)
	}
	if n := len(f.bodies(apiBook)); n != 2 {
		t.Fatalf("호가 조회 %d회", n)
	}
}

// 첫 호가가 애매하면 (스프레드 2틱 · 둘 다 10원 단위) 주식 단위로 낸다 — 거부는 안 난다.
// 뒤에 sweep 이 읽은 호가에 증거가 보이면 그때부터 ETP 다.
func TestAmbiguousBookThenEvidence(t *testing.T) {
	f := newFake()
	onOrders(f)
	cheap := protocol.Symbol{Exchange: "KRX", Code: "122630"}
	f.on(apiBook, bookResp("9990", "9980"))
	b, _ := newBroker(t, f)

	if _, err := b.SubmitBuy(ctx, broker.OrderRequest{Symbol: cheap, Qty: 1, RefPrice: 9_987, LimitPrice: 9_987}); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies(apiBuy)[0]["ord_uv"]; got != "9980" {
		t.Fatalf("애매할 때 지정가 %v — 주식 단위 9980", got)
	}
	f.on(apiBook, bookResp("9990", "9985"))
	if _, err := b.Book(ctx, cheap); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SubmitBuy(ctx, broker.OrderRequest{Symbol: cheap, Qty: 1, RefPrice: 9_987, LimitPrice: 9_987}); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies(apiBuy)[1]["ord_uv"]; got != "9985" {
		t.Fatalf("증거 뒤 지정가 %v — ETP 단위 9985", got)
	}
}

// 호가 조회가 실패하면 주식 단위로 낸다 (예전과 같음 · 거부 없음). 1분 안엔 다시 묻지 않는다.
// 두 표가 같은 가격대 (5,000원 이하) 는 아예 묻지 않는다.
func TestETPProbeFailureAndCheapPrices(t *testing.T) {
	f := newFake()
	onOrders(f)
	b, cl := newBroker(t, f) // 호가 핸들러 없음 → 빈 호가 → 오류

	for i := 0; i < 2; i++ {
		if _, err := b.PlaceTP(ctx, etf, 1, 83_601); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.bodies(apiSell)[1]["ord_uv"]; got != "83700" {
		t.Fatalf("조회 실패 때 TP %v — 주식 단위 83700", got)
	}
	if n := len(f.bodies(apiBook)); n != 1 {
		t.Fatalf("실패 뒤 1분 안 호가 조회 %d회", n)
	}

	cl.advance(etpRetry + time.Second)
	f.on(apiBook, bookResp("83605", "83600"))
	if _, err := b.PlaceTP(ctx, etf, 1, 83_601); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies(apiSell)[2]["ord_uv"]; got != "83605" {
		t.Fatalf("1분 뒤 다시 읽고 TP %v — 83605", got)
	}

	if _, err := b.SubmitBuy(ctx, broker.OrderRequest{Symbol: protocol.Symbol{Exchange: "KRX", Code: "000001"},
		Qty: 1, RefPrice: 4_321, LimitPrice: 4_321}); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies(apiBuy)[0]["ord_uv"]; got != "4320" {
		t.Fatalf("5천원 이하 지정가 %v", got)
	}
	if n := len(f.bodies(apiBook)); n != 2 {
		t.Fatalf("5천원 이하인데 호가를 물었다 (%d회)", n)
	}
}
