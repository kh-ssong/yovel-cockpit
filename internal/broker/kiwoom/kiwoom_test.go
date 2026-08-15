package kiwoom

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

var (
	ctx = context.Background()
	sym = protocol.Symbol{Exchange: "KRX", Code: "005930"}
)

// ── 호가단위 ────────────────────────────────────────────────────────────────

// ★ 2023-01-25 현행 규정. 구 규정(2010~2022)이 박혀 있으면 세 대역이 5배 과대인데,
// 구 단위는 현행의 배수라 **주문이 거부되지 않는다** — 거절 로그 없이 손해만 난다.
func TestTickTableIsCurrent(t *testing.T) {
	cases := []struct {
		price float64
		want  float64
	}{
		{999, 1},
		{1_500, 1}, // 구 규정이면 5 — 여기가 사고 지점 ①
		{2_500, 5},
		{4_999, 5},
		{12_000, 10}, // 구 규정이면 50 — 사고 지점 ②
		{30_000, 50},
		{72_100, 100},
		{150_000, 100}, // 구 규정이면 500 — 사고 지점 ③
		{300_000, 500},
		{700_000, 1000},
	}
	for _, c := range cases {
		if got := TickSize(c.price, false); got != c.want {
			t.Errorf("%.0f원 → 틱 %.0f, 기대 %.0f", c.price, got, c.want)
		}
	}
}

// ★ ETF/ETN 은 위 표를 아예 안 따른다.
func TestETPTicks(t *testing.T) {
	if got := TickSize(50_000, true); got != 5 {
		t.Fatalf("ETP 5만원 → %v, 기대 5", got)
	}
	if got := TickSize(1_500, true); got != 1 {
		t.Fatalf("저가 ETP → %v, 기대 1", got)
	}
}

// 매도 지정가는 올리고 매수 지정가는 내린다 (불리한 쪽으로 틀리지 않도록).
func TestRounding(t *testing.T) {
	if got := CeilToTick(71_234, false); got != 71_300 {
		t.Fatalf("매도 올림 %v", got)
	}
	if got := FloorToTick(71_234, false); got != 71_200 {
		t.Fatalf("매수 내림 %v", got)
	}
}

// ── 숫자·코드 파싱 ──────────────────────────────────────────────────────────

func TestNumParsing(t *testing.T) {
	cases := map[string]float64{
		"+000012345": 12345,
		"1,234,567":  1234567,
		"-5":         -5,
		"  72100  ":  72100,
		"":           0,
		"쓰레기":        0,
	}
	for in, want := range cases {
		if got := num(in); got != want {
			t.Errorf("num(%q)=%v, 기대 %v", in, got, want)
		}
	}
	if _, ok := numOK(""); ok {
		t.Error("빈 문자열이 파싱 성공으로 나왔다 — 0 과 실패를 구분해야 한다")
	}
	if v, ok := numOK("0"); !ok || v != 0 {
		t.Error(`"0" 은 유효한 값이다`)
	}
}

// ★ 잔고 응답의 stk_cd 에는 "A" 프리픽스가 붙는다. 안 떼면 방금 산 종목을 못 찾아
// "보유 없음" 으로 읽고, 그 포지션을 유령으로 종결시킨다.
func TestCodePrefixStripped(t *testing.T) {
	if got := code("A005930"); got != "005930" {
		t.Fatalf("%q", got)
	}
	if got := code(" 005930 "); got != "005930" {
		t.Fatalf("%q", got)
	}
}

// ── 가짜 키움 서버 ──────────────────────────────────────────────────────────

type fakeKiwoom struct {
	mu       sync.Mutex
	requests map[string][]map[string]any // api-id → 받은 바디들
	handlers map[string]func(body map[string]any) any
	tokens   int
}

func newFake() *fakeKiwoom {
	return &fakeKiwoom{
		requests: map[string][]map[string]any{},
		handlers: map[string]func(map[string]any) any{},
	}
}

func (f *fakeKiwoom) on(apiID string, h func(map[string]any) any) { f.handlers[apiID] = h }

func (f *fakeKiwoom) bodies(apiID string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[apiID]
}

func (f *fakeKiwoom) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)

		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
			f.mu.Lock()
			f.tokens++
			n := f.tokens
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{
				"return_code": 0, "token": "tok-" + string(rune('A'+n-1)),
				"expires_dt": time.Now().Add(24 * time.Hour).Format("20060102150405"),
			})
			return
		}

		apiID := r.Header.Get("api-id")
		f.mu.Lock()
		f.requests[apiID] = append(f.requests[apiID], body)
		f.mu.Unlock()

		h, ok := f.handlers[apiID]
		if !ok {
			json.NewEncoder(w).Encode(map[string]any{"return_code": 0})
			return
		}
		json.NewEncoder(w).Encode(h(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newBroker(t *testing.T, f *fakeKiwoom) (*Broker, *clock) {
	t.Helper()
	srv := f.start(t)
	cl := &clock{t: time.Date(2026, 8, 15, 9, 5, 0, 0, time.Local)}

	b, err := New(Config{
		AppKey: "k", SecretKey: "s", DataDir: t.TempDir(), APIURL: srv.URL,
		HTTP: srv.Client(), Now: cl.now,
		// 폴링을 실제로 재우지 않는다 — 대신 시계를 앞으로 민다.
		Sleep:       func(d time.Duration) { cl.advance(d) },
		FillTimeout: 5 * time.Second, FillPoll: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b, cl
}

// ── 잔고 ────────────────────────────────────────────────────────────────────

// ★ 예수금(entr)과 주문가능금액(ord_alowa)은 다른 층이다.
// 사이징을 예수금으로 하면 증거금·미체결 때문에 그대로 거부된다.
func TestCashSeparatesTwoLayers(t *testing.T) {
	f := newFake()
	f.on(apiBalance, func(map[string]any) any {
		return map[string]any{
			"return_code": 0, "entr": "1,000,000", "ord_alowa": "+000094352",
			"stk_cntr_remn": []any{},
		}
	})
	b, _ := newBroker(t, f)

	c, err := b.Cash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.Deposit != 1_000_000 {
		t.Fatalf("예수금 %v", c.Deposit)
	}
	if c.Orderable != 94_352 {
		t.Fatalf("주문가능 %v", c.Orderable)
	}
	if c.Deposit == c.Orderable {
		t.Fatal("두 층이 같아졌다 — 이걸 합치면 주문이 거부된다")
	}
}

func TestPositionsParsing(t *testing.T) {
	f := newFake()
	f.on(apiBalance, func(map[string]any) any {
		return map[string]any{
			"return_code": 0, "entr": "0", "ord_alowa": "0",
			"stk_cntr_remn": []any{
				map[string]any{"stk_cd": "A005930", "cur_qty": "+14", "buy_uv": "72,100"},
				map[string]any{"stk_cd": "A000660", "cur_qty": "0", "buy_uv": "100"}, // 0주 = 제외
			},
		}
	})
	b, _ := newBroker(t, f)

	pos, err := b.Positions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 1 {
		t.Fatalf("%d건 (0주 행이 섞였다)", len(pos))
	}
	if pos[0].Symbol.Code != "005930" || pos[0].Qty != 14 || pos[0].AvgPrice != 72_100 {
		t.Fatalf("%+v", pos[0])
	}
}

// ── 주문 ────────────────────────────────────────────────────────────────────

func fillsResp(ordNo string, rows ...map[string]any) map[string]any {
	for _, r := range rows {
		r["ord_no"] = ordNo
	}
	list := make([]any, len(rows))
	for i, r := range rows {
		list[i] = r
	}
	return map[string]any{"return_code": 0, "cntr": list}
}

// ★ 봇이 계산한 수량을 키움이 거부한 사고가 실재한다(9주 시도 → "6주 가능" 거부).
// kt00011 로 사전 축소하지 않으면 그 거부가 그대로 재현된다.
func TestBuyShrinksToBrokerAllowance(t *testing.T) {
	f := newFake()
	f.on(apiBuyableQt, func(map[string]any) any {
		return map[string]any{"return_code": 0, "min_ord_alowq": "6"}
	})
	f.on(apiBuy, func(map[string]any) any {
		return map[string]any{"return_code": 0, "ord_no": "00024"}
	})
	f.on(apiFills, func(map[string]any) any {
		return fillsResp("00024", map[string]any{
			"cntr_qty": "6", "cntr_pric": "72,100",
			"tdy_trde_cmsn": "0", "tdy_trde_tax": "0", "ord_tm": "090512",
		})
	})
	b, _ := newBroker(t, f)

	fill, err := b.Buy(ctx, broker.OrderRequest{Symbol: sym, Qty: 9, RefPrice: 72_100})
	if err != nil {
		t.Fatal(err)
	}
	if fill.Qty != 6 {
		t.Fatalf("체결 %v주", fill.Qty)
	}

	sent := f.bodies(apiBuy)
	if len(sent) != 1 {
		t.Fatalf("주문 %d회", len(sent))
	}
	if got := sent[0]["ord_qty"]; got != "6" {
		t.Fatalf("★ 축소 안 하고 %v주를 불렀다", got)
	}
	if got := sent[0]["trde_tp"]; got != tradeMarket {
		t.Fatalf("거래구분 %v", got)
	}
}

// ★ 수수료는 요율로 추정하지 않고 브로커가 준 실측을 쓴다.
// 요율 추정이 "기록 수수료 왕복 1.9배 과다" 를 만든 전례가 있다.
func TestFeeComesFromBrokerNotRate(t *testing.T) {
	f := newFake()
	f.on(apiBuy, func(map[string]any) any {
		return map[string]any{"return_code": 0, "ord_no": "77"}
	})
	f.on(apiFills, func(map[string]any) any {
		return fillsResp("77", map[string]any{
			"cntr_qty": "10", "cntr_pric": "1000",
			"tdy_trde_cmsn": "15", "tdy_trde_tax": "20", "ord_tm": "090512",
		})
	})
	b, _ := newBroker(t, f)

	fill, err := b.Buy(ctx, broker.OrderRequest{Symbol: sym, Qty: 10, RefPrice: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if fill.FeeKRW != 35 {
		t.Fatalf("수수료 %v, 기대 35 (수수료 15 + 세금 20)", fill.FeeKRW)
	}
}

// ★ 시장가는 분할체결된다. 부분을 조기 반환하면 잔여가 장부 밖 유령이 된다.
func TestPartialFillWaitsThenReportsPartial(t *testing.T) {
	f := newFake()
	f.on(apiSell, func(map[string]any) any {
		return map[string]any{"return_code": 0, "ord_no": "88"}
	})
	calls := 0
	f.on(apiFills, func(map[string]any) any {
		calls++
		// 계속 13/29 만 채워진 채로 둔다 → 타임아웃까지 기다린 뒤 부분으로 보고.
		return fillsResp("88", map[string]any{
			"cntr_qty": "13", "cntr_pric": "1000",
			"tdy_trde_cmsn": "0", "tdy_trde_tax": "0", "ord_tm": "090512",
		})
	})
	b, _ := newBroker(t, f)

	fill, err := b.Sell(ctx, broker.OrderRequest{Symbol: sym, Qty: 29, RefPrice: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if !fill.Partial {
		t.Fatal("★ 부분체결을 완전체결로 보고했다 — 잔여 16주가 장부 밖으로 샌다")
	}
	if fill.Qty != 13 {
		t.Fatalf("체결 %v주", fill.Qty)
	}
	if calls < 2 {
		t.Fatalf("한 번만 보고 포기했다 (%d회) — 전량까지 기다려야 한다", calls)
	}
}

// ★ 매도 지정가는 호가단위로 올린다. 내리면 의도보다 싸게 팔린다.
func TestPlaceTPRoundsUpToTick(t *testing.T) {
	f := newFake()
	f.on(apiSell, func(map[string]any) any {
		return map[string]any{"return_code": 0, "ord_no": "tp-1"}
	})
	b, _ := newBroker(t, f)

	id, err := b.PlaceTP(ctx, sym, 10, 71_234)
	if err != nil {
		t.Fatal(err)
	}
	if id != "tp-1" {
		t.Fatalf("주문번호 %q", id)
	}
	body := f.bodies(apiSell)[0]
	if body["ord_uv"] != "71300" {
		t.Fatalf("지정가 %v, 기대 71300 (틱 100 올림)", body["ord_uv"])
	}
	if body["trde_tp"] != tradeLimit {
		t.Fatalf("거래구분 %v — 지정가여야 한다", body["trde_tp"])
	}
}

// ★ HTTP 200 인데 return_code != 0 = 거부다. 성공으로 읽으면 "주문 거부" 가 원장에 체결로 남는다.
func TestRejectIsNotSuccess(t *testing.T) {
	f := newFake()
	f.on(apiBuy, func(map[string]any) any {
		return map[string]any{"return_code": 40, "return_msg": "주문가능금액 부족"}
	})
	b, _ := newBroker(t, f)

	if _, err := b.Buy(ctx, broker.OrderRequest{Symbol: sym, Qty: 1, RefPrice: 1000}); err == nil {
		t.Fatal("거부를 성공으로 읽었다")
	} else if !strings.Contains(err.Error(), "주문가능금액") {
		t.Fatalf("거부 사유를 안 전달한다: %v", err)
	}
}

// ★ 8005 는 국내에선 return_code, 해외에선 return_msg 안에 온다.
// 그리고 재발급은 **요청당 한 번만** — 무한 재발급이 곧 1계정 1토큰 사고의 형태다.
func TestTokenInvalidRefreshesOnce(t *testing.T) {
	f := newFake()
	n := 0
	f.on(apiQuote, func(map[string]any) any {
		n++
		if n == 1 {
			return map[string]any{"return_code": 3,
				"return_msg": "인증에 실패했습니다[8005:Token이 유효하지 않습니다]"}
		}
		return map[string]any{"return_code": 0, "cur_prc": "+72100"}
	})
	b, _ := newBroker(t, f)

	q, err := b.Quote(ctx, sym)
	if err != nil {
		t.Fatal(err)
	}
	if q.Price != 72_100 {
		t.Fatalf("현재가 %v (부호 처리 실패)", q.Price)
	}
	if f.tokens != 2 {
		t.Fatalf("토큰 발급 %d회, 기대 2 (최초 + 8005 후 1회)", f.tokens)
	}
}

func TestTokenIsReusedAcrossCalls(t *testing.T) {
	f := newFake()
	f.on(apiQuote, func(map[string]any) any {
		return map[string]any{"return_code": 0, "cur_prc": "1000"}
	})
	b, _ := newBroker(t, f)

	for i := 0; i < 5; i++ {
		if _, err := b.Quote(ctx, sym); err != nil {
			t.Fatal(err)
		}
	}
	// ★ 호출마다 발급하면 다른 프로세스의 토큰을 죽인다.
	if f.tokens != 1 {
		t.Fatalf("토큰 발급 %d회 — 재사용이 안 된다", f.tokens)
	}
}

func TestCancelSendsOriginalOrderNo(t *testing.T) {
	f := newFake()
	b, _ := newBroker(t, f)
	if err := b.CancelOrder(ctx, sym, "00024"); err != nil {
		t.Fatal(err)
	}
	body := f.bodies(apiCancel)[0]
	if body["orig_ord_no"] != "00024" || body["cncl_qty"] != "0" {
		t.Fatalf("%+v", body)
	}
	// 빈 주문번호는 취소할 것이 없다 (멱등).
	if err := b.CancelOrder(ctx, sym, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.bodies(apiCancel)) != 1 {
		t.Fatal("빈 주문번호로 취소를 쐈다")
	}
}

// ★ 리스트 키 이름을 하드코딩하지 않는다 — 문서와 실제가 어긋난 전례가 있고,
// 키가 틀리면 "체결 0건" 으로 읽혀 주문은 나갔는데 장부엔 없는 최악이 된다.
func TestFillRowsFoundByContentNotKeyName(t *testing.T) {
	f := newFake()
	f.on(apiBuy, func(map[string]any) any {
		return map[string]any{"return_code": 0, "ord_no": "99"}
	})
	f.on(apiFills, func(map[string]any) any {
		// 키 이름을 일부러 다르게 준다.
		return map[string]any{"return_code": 0, "cntr_list_v2": []any{
			map[string]any{"ord_no": "99", "cntr_qty": "5", "cntr_pric": "1000",
				"tdy_trde_cmsn": "0", "tdy_trde_tax": "0", "ord_tm": "090512"},
		}}
	})
	b, _ := newBroker(t, f)

	fill, err := b.Buy(ctx, broker.OrderRequest{Symbol: sym, Qty: 5, RefPrice: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if fill.Qty != 5 {
		t.Fatalf("리스트 키가 바뀌자 체결을 못 읽었다: %+v", fill)
	}
}

// 다른 주문번호의 체결을 내 것으로 세면 안 된다 (같은 종목 multi-entry).
func TestFillsAreFilteredByOrderNo(t *testing.T) {
	f := newFake()
	f.on(apiBuy, func(map[string]any) any {
		return map[string]any{"return_code": 0, "ord_no": "me"}
	})
	f.on(apiFills, func(map[string]any) any {
		return map[string]any{"return_code": 0, "cntr": []any{
			map[string]any{"ord_no": "someone-else", "cntr_qty": "100", "cntr_pric": "1000",
				"tdy_trde_cmsn": "0", "tdy_trde_tax": "0", "ord_tm": "090500"},
			map[string]any{"ord_no": "me", "cntr_qty": "3", "cntr_pric": "1000",
				"tdy_trde_cmsn": "0", "tdy_trde_tax": "0", "ord_tm": "090512"},
		}}
	})
	b, _ := newBroker(t, f)

	fill, err := b.Buy(ctx, broker.OrderRequest{Symbol: sym, Qty: 3, RefPrice: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if fill.Qty != 3 {
		t.Fatalf("남의 체결까지 셌다: %v주", fill.Qty)
	}
}

// kt00011 조회가 실패해도 진입을 막지 않는다 (fail-open).
// ★ 여기서 막으면 API 혼잡 한 번에 09:00 진입 윈도를 통째로 잃는다.
func TestBuyableQueryFailureIsFailOpen(t *testing.T) {
	f := newFake()
	f.on(apiBuyableQt, func(map[string]any) any {
		return map[string]any{"return_code": 500, "return_msg": "일시 오류"}
	})
	f.on(apiBuy, func(map[string]any) any {
		return map[string]any{"return_code": 0, "ord_no": "1"}
	})
	f.on(apiFills, func(map[string]any) any {
		return fillsResp("1", map[string]any{"cntr_qty": "7", "cntr_pric": "1000",
			"tdy_trde_cmsn": "0", "tdy_trde_tax": "0", "ord_tm": "090512"})
	})
	b, _ := newBroker(t, f)

	fill, err := b.Buy(ctx, broker.OrderRequest{Symbol: sym, Qty: 7, RefPrice: 1000})
	if err != nil {
		t.Fatalf("사전 조회 실패가 진입을 막았다: %v", err)
	}
	if fill.Qty != 7 {
		t.Fatalf("%v주", fill.Qty)
	}
}

// 이 드라이버가 broker.Broker 계약을 만족하는지 컴파일 타임에 고정.
var _ broker.Broker = (*Broker)(nil)
