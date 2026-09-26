package upbit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

var (
	ctx = context.Background()
	btc = protocol.Symbol{Exchange: Exchange, Code: "KRW-BTC"}
)

const secret = "test-secret"

// fake — 업비트 흉내. 요청마다 JWT 서명과 query_hash 를 **실제로 검증**한다.
type fake struct {
	t     *testing.T
	mu    sync.Mutex
	calls []call
	// route — (method path) → 핸들러. 없으면 404.
	route map[string]func(c call) (int, any)
}

type call struct {
	Method, Path string
	Params       url.Values
}

func (f *fake) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Method == method && c.Path == path {
			n++
		}
	}
	return n
}

func (f *fake) last(method, path string) call {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Method == method && f.calls[i].Path == path {
			return f.calls[i]
		}
	}
	f.t.Fatalf("%s %s 호출 없음", method, path)
	return call{}
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var params url.Values
	query := ""
	if r.Method == http.MethodPost {
		raw, _ := io.ReadAll(r.Body)
		m := map[string]string{}
		if err := json.Unmarshal(raw, &m); err != nil {
			f.t.Errorf("본문이 JSON 이 아니다: %s", raw)
		}
		params = url.Values{}
		for k, v := range m {
			params.Set(k, v)
		}
		query = params.Encode()
	} else {
		params = r.URL.Query()
		query = r.URL.RawQuery
	}

	c := call{Method: r.Method, Path: strings.TrimPrefix(r.URL.Path, "/v1"), Params: params}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()

	if c.Path != "/ticker" {
		f.verifyJWT(r.Header.Get("Authorization"), query)
	} else if r.Header.Get("Authorization") != "" {
		f.t.Errorf("시세 API 에 인증 헤더가 붙었다")
	}

	h, ok := f.route[r.Method+" "+c.Path]
	if !ok {
		w.WriteHeader(404)
		return
	}
	status, body := h(c)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fake) verifyJWT(auth, query string) {
	tok := strings.TrimPrefix(auth, "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		f.t.Errorf("JWT 형식 아님: %q", auth)
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) != parts[2] {
		f.t.Errorf("JWT 서명 불일치")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]string
	_ = json.Unmarshal(raw, &claims)
	if claims["access_key"] != "ak" || claims["nonce"] == "" {
		f.t.Errorf("claims 누락: %v", claims)
	}
	if query == "" {
		if claims["query_hash"] != "" {
			f.t.Errorf("쿼리 없는데 query_hash 가 있다")
		}
		return
	}
	h := sha512.Sum512([]byte(query))
	if claims["query_hash"] != hex.EncodeToString(h[:]) || claims["query_hash_alg"] != "SHA512" {
		// ★ 이게 라이브에선 401 "verify the query of Jwt" 로 나타난다.
		f.t.Errorf("query_hash 가 보낸 쿼리와 다르다: %q", query)
	}
}

func newTest(t *testing.T, route map[string]func(c call) (int, any)) (*Broker, *fake) {
	t.Helper()
	f := &fake{t: t, route: route}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	clock := time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	b, err := New(Config{
		AccessKey: "ak", SecretKey: secret, APIURL: srv.URL + "/v1",
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return clock },
		Sleep: func(d time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			clock = clock.Add(d)
		},
		FillTimeout: 3 * time.Second, FillPoll: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b, f
}

func ok(v any) func(call) (int, any) { return func(call) (int, any) { return 200, v } }

func chanceOK(balance string) func(call) (int, any) {
	return ok(map[string]any{"bid_fee": "0.0005", "bid_account": map[string]string{"balance": balance}})
}

// 시장가 매수가 잔여 먼지로 끝난 모양 — state=cancel 인데 체결은 됐다.
func dustCancelBuy(uuid string) map[string]any {
	return map[string]any{
		"uuid": uuid, "side": "bid", "ord_type": "price", "state": "cancel",
		"executed_volume": "0.00099", "paid_fee": "49.5", "volume": nil,
		"trades": []map[string]any{
			{"price": "100000000", "volume": "0.0005", "funds": "50000", "created_at": "2026-09-26T10:00:01+09:00"},
			{"price": "101000000", "volume": "0.00049", "funds": "49490", "created_at": "2026-09-26T10:00:02+09:00"},
		},
	}
}

// ── 매수 ────────────────────────────────────────────────────────────────────

// ★ 시장가 매수는 수량이 아니라 금액(ord_type=price)이다. 수량을 volume 으로 보내면
// 업비트는 그걸 지정가로 읽거나 거부한다.
func TestMarketBuySendsAmountNotVolume(t *testing.T) {
	b, f := newTest(t, map[string]func(call) (int, any){
		"GET /orders/chance": chanceOK("10000000"),
		"POST /orders":       ok(map[string]any{"uuid": "u1"}),
		"GET /order":         ok(dustCancelBuy("u1")),
	})
	fill, err := b.Buy(ctx, broker.OrderRequest{Symbol: btc, Qty: 0.001, RefPrice: 100_000_000})
	if err != nil {
		t.Fatal(err)
	}
	p := f.last("POST", "/orders").Params
	if p.Get("ord_type") != "price" || p.Get("price") != "100000" || p.Get("volume") != "" || p.Get("side") != "bid" {
		t.Fatalf("시장가 매수 파라미터 %v", p)
	}
	// ★ state=cancel 이어도 체결이다 (먼지 잔여).
	if fill.Qty != 0.00099 {
		t.Fatalf("체결 수량 %v", fill.Qty)
	}
	if want := 99490.0 / 0.00099; fill.Price < want-1e-6 || fill.Price > want+1e-6 {
		t.Fatalf("평균가 %v, 기대 %v (Σfunds/Σvolume)", fill.Price, want)
	}
	if fill.FeeKRW != 49.5 {
		t.Fatalf("수수료는 paid_fee 실측이어야 한다: %v", fill.FeeKRW)
	}
	if fill.Partial {
		t.Fatal("먼지 cancel 을 부분체결로 읽었다")
	}
	if !fill.FilledAt.Equal(time.Date(2026, 9, 26, 1, 0, 2, 0, time.UTC)) {
		t.Fatalf("체결 시각은 마지막 trade 시각이어야 한다: %v", fill.FilledAt)
	}
}

// ★ 수수료는 주문 금액 위에 따로 묶인다. 주문가능금액 그대로 부르면 거부된다.
func TestBuyShrinksToChanceWithFee(t *testing.T) {
	b, f := newTest(t, map[string]func(call) (int, any){
		"GET /orders/chance": chanceOK("50000"),
		"POST /orders":       ok(map[string]any{"uuid": "u1"}),
		"GET /order":         ok(dustCancelBuy("u1")),
	})
	if _, err := b.Buy(ctx, broker.OrderRequest{Symbol: btc, Qty: 0.001, RefPrice: 100_000_000}); err != nil {
		t.Fatal(err)
	}
	if got := f.last("POST", "/orders").Params.Get("price"); got != "49975" {
		t.Fatalf("축소 금액 %s, 기대 49975 (= floor(50000/1.0005))", got)
	}
}

func TestBuyBelowMinOrderNeverHitsExchange(t *testing.T) {
	b, f := newTest(t, map[string]func(call) (int, any){
		"GET /orders/chance": chanceOK("10000000"),
	})
	_, err := b.Buy(ctx, broker.OrderRequest{Symbol: btc, Qty: 0.00001, RefPrice: 100_000_000})
	if !errors.Is(err, broker.ErrInsufficient) {
		t.Fatalf("최소주문금액 미달이 ErrInsufficient 가 아니다: %v", err)
	}
	if f.count("POST", "/orders") != 0 {
		t.Fatal("거부될 주문을 거래소에 보냈다")
	}
}

// ★ 주문(POST) 5xx 는 재시도하지 않는다 — 서버가 받았는지 모르는데 다시 쏘면 두 번 산다.
func TestOrderPostIsNeverRetriedOn5xx(t *testing.T) {
	b, f := newTest(t, map[string]func(call) (int, any){
		"GET /orders/chance": chanceOK("10000000"),
		"POST /orders":       func(call) (int, any) { return 502, map[string]string{} },
	})
	_, err := b.Buy(ctx, broker.OrderRequest{Symbol: btc, Qty: 0.001, RefPrice: 100_000_000})
	if !IsMaybeSent(err) {
		t.Fatalf("5xx 가 '나갔을 수 있다' 로 보고되지 않았다: %v", err)
	}
	if n := f.count("POST", "/orders"); n != 1 {
		t.Fatalf("주문이 %d 번 나갔다", n)
	}
}

// 429 는 서버가 처리하지 않았다고 말해준 것이라 주문도 재시도한다.
func TestOrderPostRetriesOn429(t *testing.T) {
	n := 0
	b, f := newTest(t, map[string]func(call) (int, any){
		"GET /orders/chance": chanceOK("10000000"),
		"POST /orders": func(call) (int, any) {
			n++
			if n == 1 {
				return 429, map[string]string{}
			}
			return 201, map[string]any{"uuid": "u1"}
		},
		"GET /order": ok(dustCancelBuy("u1")),
	})
	if _, err := b.Buy(ctx, broker.OrderRequest{Symbol: btc, Qty: 0.001, RefPrice: 100_000_000}); err != nil {
		t.Fatal(err)
	}
	if f.count("POST", "/orders") != 2 {
		t.Fatal("429 뒤 재시도가 없다")
	}
}

// ── 매도 ────────────────────────────────────────────────────────────────────

func TestMarketSellFloorsVolumeTo8Decimals(t *testing.T) {
	b, f := newTest(t, map[string]func(call) (int, any){
		"POST /orders": ok(map[string]any{"uuid": "s1"}),
		"GET /order": ok(map[string]any{
			"uuid": "s1", "side": "ask", "ord_type": "market", "state": "done",
			"volume": "0.00099999", "executed_volume": "0.00099999", "paid_fee": "50",
			"trades": []map[string]any{{"price": "100000000", "volume": "0.00099999", "funds": "99999"}},
		}),
	})
	fill, err := b.Sell(ctx, broker.OrderRequest{Symbol: btc, Qty: 0.000999999999, RefPrice: 100_000_000})
	if err != nil {
		t.Fatal(err)
	}
	p := f.last("POST", "/orders").Params
	// ★ 올리면 가진 것보다 많이 팔려 들어 insufficient_funds_ask.
	if p.Get("volume") != "0.00099999" || p.Get("ord_type") != "market" || p.Get("side") != "ask" {
		t.Fatalf("시장가 매도 파라미터 %v", p)
	}
	if fill.Partial || fill.Qty != 0.00099999 {
		t.Fatalf("체결 %+v", fill)
	}
}

// ★ 에러명을 삼키지 않는다 — 잔고 잠김을 일반 실패로 읽으면 무한 재시도한다.
func TestSellErrorNameSurvives(t *testing.T) {
	b, _ := newTest(t, map[string]func(call) (int, any){
		"POST /orders": func(call) (int, any) {
			return 400, map[string]any{"error": map[string]string{
				"name": "insufficient_funds_ask", "message": "매도가능 잔고가 부족합니다."}}
		},
	})
	_, err := b.Sell(ctx, broker.OrderRequest{Symbol: btc, Qty: 0.001})
	if !errors.Is(err, broker.ErrNotEnoughShare) || !strings.Contains(err.Error(), "insufficient_funds_ask") {
		t.Fatalf("에러명이 사라졌다: %v", err)
	}
}

// ★ 시간 안에 안 끝나면 취소하고 확정된 상태로 보고한다 — 살아 있는 주문을 남기지 않는다.
func TestTimeoutCancelsThenReportsPartial(t *testing.T) {
	cancelled := false
	b, f := newTest(t, map[string]func(call) (int, any){
		"POST /orders":  ok(map[string]any{"uuid": "s1"}),
		"DELETE /order": func(call) (int, any) { cancelled = true; return 200, map[string]any{"uuid": "s1"} },
		"GET /order": func(call) (int, any) {
			state := "wait"
			if cancelled {
				state = "cancel"
			}
			return 200, map[string]any{
				"uuid": "s1", "side": "ask", "ord_type": "limit", "state": state,
				"volume": "0.002", "executed_volume": "0.001", "paid_fee": "25",
				"trades": []map[string]any{{"price": "100000000", "volume": "0.001", "funds": "100000"}},
			}
		},
	})
	fill, err := b.Sell(ctx, broker.OrderRequest{Symbol: btc, Qty: 0.002, LimitPrice: 100_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if f.count("DELETE", "/order") != 1 {
		t.Fatal("대기 초과인데 취소하지 않았다")
	}
	if !fill.Partial || fill.Qty != 0.001 {
		t.Fatalf("부분체결 보고 %+v", fill)
	}
}

// ── 잔고 ────────────────────────────────────────────────────────────────────

// ★ TP 에 묶인(locked) 수량도 보유다. 빼면 TP 를 건 순간 포지션이 사라진 것으로 보인다.
func TestPositionsCountLockedAsHeld(t *testing.T) {
	b, _ := newTest(t, map[string]func(call) (int, any){
		"GET /accounts": ok([]map[string]string{
			{"currency": "KRW", "balance": "1000000", "locked": "50000", "avg_buy_price": "0", "unit_currency": "KRW"},
			{"currency": "BTC", "balance": "0", "locked": "0.001", "avg_buy_price": "100000000", "unit_currency": "KRW"},
			{"currency": "ETH", "balance": "0", "locked": "0", "avg_buy_price": "0", "unit_currency": "KRW"},
		}),
	})
	pos, err := b.Positions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 1 || pos[0].Symbol != btc || pos[0].Qty != 0.001 || pos[0].Sellable != 0 {
		t.Fatalf("포지션 %+v", pos)
	}
	cash, _ := b.Cash(ctx)
	if cash.Deposit != 1_050_000 || cash.Orderable != 1_000_000 {
		t.Fatalf("현금 두 층 %+v", cash)
	}
}

func TestQuoteIsPublicAndParsesNumber(t *testing.T) {
	b, f := newTest(t, map[string]func(call) (int, any){
		"GET /ticker": ok([]map[string]any{{"market": "KRW-BTC", "trade_price": 101_500_000.0}}),
	})
	q, err := b.Quote(ctx, btc)
	if err != nil || q.Price != 101_500_000 {
		t.Fatalf("시세 %v %v", q, err)
	}
	if f.last("GET", "/ticker").Params.Get("markets") != "KRW-BTC" {
		t.Fatal("markets 파라미터")
	}
}

func TestRejectsNonKRWMarket(t *testing.T) {
	b, _ := newTest(t, nil)
	for _, s := range []protocol.Symbol{{Exchange: "KRX", Code: "005930"}, {Exchange: Exchange, Code: "BTC-ETH"}} {
		if _, err := b.Buy(ctx, broker.OrderRequest{Symbol: s, Qty: 1, RefPrice: 1}); !errors.Is(err, broker.ErrUnknownSymbol) {
			t.Fatalf("%v 를 받았다: %v", s, err)
		}
	}
}

// ── 호가단위 ────────────────────────────────────────────────────────────────

func TestTickRounding(t *testing.T) {
	cases := []struct{ in, ceil, floor float64 }{
		{100_000_050, 100_001_000, 100_000_000},
		{1_234, 1_235, 1_230},
		{1_235, 1_235, 1_235}, // 정확히 틱 위 → 그대로
		{12.34, 12.4, 12.3},
		{0.1234, 0.124, 0.123}, // 1원 미만은 0.001 단위
		{0.0123, 0.013, 0.012},
	}
	for _, c := range cases {
		if got := CeilToTick(c.in); got != c.ceil {
			t.Errorf("Ceil(%v) = %v, 기대 %v", c.in, got, c.ceil)
		}
		if got := FloorToTick(c.in); got != c.floor {
			t.Errorf("Floor(%v) = %v, 기대 %v", c.in, got, c.floor)
		}
	}
}

// 키 없이 시세는 되고, 주문은 서버에 닿지도 않고 막힌다.
func TestPublicQuotesButCannotTrade(t *testing.T) {
	f := &fake{t: t, route: map[string]func(call) (int, any){
		"GET /ticker": ok([]map[string]any{{"market": "KRW-BTC", "trade_price": 101_000_000.0}}),
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	b := NewPublic(Config{APIURL: srv.URL + "/v1", Sleep: func(time.Duration) {}})

	if q, err := b.Quote(ctx, btc); err != nil || q.Price != 101_000_000 {
		t.Fatalf("%v %v", q, err)
	}
	if _, err := b.Sell(ctx, broker.OrderRequest{Symbol: btc, Qty: 0.001}); err == nil {
		t.Fatal("키 없이 주문이 통과했다")
	}
	if f.count("POST", "/orders") != 0 {
		t.Fatal("키 없는 주문이 서버에 닿았다")
	}
}
