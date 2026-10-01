// Package upbit 는 업비트 REST 드라이버다.
//
// ★ 이 파일은 업비트 하나만 안다. 키움과 공통화하지 않는다 (broker 패키지 주석 참조).
//
// 여기 박힌 함정은 reflex-agent 라이브(`app/execution/upbit_*.py`)에서 실제로 당한 것들이다:
//   - 시장가 **매수**는 수량이 아니라 **금액**(ord_type=price)으로 낸다. 매도는 수량(ord_type=market)
//   - 시장가 매수는 잔여 먼지 때문에 state=cancel 로 끝난다 — executed_volume>0 이면 체결이다
//   - 응답 숫자가 API 마다 문자열이기도 숫자이기도 하다 (client.go fnum)
//   - 4xx 에러명을 삼키면 잔고 잠김을 일반 실패로 읽고 무한 재시도한다 (client.go apiError)
//   - JWT query_hash 는 실제로 보낸 문자열과 같아야 한다 (client.go token)
//   - 최소주문금액 5,000원 미만은 거래소가 거부한다
//   - 수수료는 요율로 추정하지 않는다 — 주문 응답의 paid_fee 가 실측이다
//
// ★ 한 데몬에 브로커 하나. 업비트는 **별도 cockpitd 인스턴스**(별도 data-dir·포트)로 띄운다.
package upbit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

const (
	prodURL = "https://api.upbit.com/v1"

	// Exchange — 프로토콜의 거래소 이름 (schema/v1/common.schema.json).
	Exchange = "UPBIT"

	// MinOrderKRW — KRW 마켓 최소주문금액. 미만이면 under_min_total_* 로 거부된다.
	MinOrderKRW = 5000.0

	// Lot — 코인 수량 최소 단위(소수 8자리). 사이징이 이 단위로 내림한다.
	Lot = 1e-8
)

type Config struct {
	AccessKey string
	SecretKey string
	// APIURL — 비면 운영. 업비트는 모의투자 도메인이 없다 — 연습은 paper 로 한다.
	APIURL string

	HTTP  *http.Client
	Now   func() time.Time
	Sleep func(time.Duration)

	// FillTimeout — 체결 대기 상한. 넘으면 **취소하고** 그때까지의 체결로 보고한다.
	FillTimeout time.Duration
	// FillPoll — 체결 조회 간격.
	FillPoll time.Duration
	// MinGap — 요청 사이 최소 간격. 주문 한도 8/s 에 여유를 둔 값이 기본.
	MinGap time.Duration
}

type Broker struct {
	cfg    Config
	apiURL string
	http   *http.Client
	now    func() time.Time
	sleep  func(time.Duration)
	limit  *limiter
}

func New(cfg Config) (*Broker, error) {
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("access/secret key 가 없다")
	}
	return build(cfg), nil
}

// NewPublic — 키 없이 **시세만** 쓰는 드라이버. 업비트 시세 API 는 공개라 키가 필요 없다.
//
// ★ paper 가 코인 시세를 받으려고 쓴다. 인증이 필요한 동작(잔고·주문)은 전부 오류를 낸다 —
// 키 없는 드라이버가 실수로 브로커 자리에 꽂혀도 주문이 나가지 않는다.
func NewPublic(cfg Config) *Broker {
	cfg.AccessKey, cfg.SecretKey = "", ""
	return build(cfg)
}

func build(cfg Config) *Broker {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = time.Sleep
	}
	if cfg.FillTimeout <= 0 {
		cfg.FillTimeout = 20 * time.Second
	}
	if cfg.FillPoll <= 0 {
		cfg.FillPoll = 300 * time.Millisecond
	}
	if cfg.MinGap <= 0 {
		cfg.MinGap = 170 * time.Millisecond // ≈ 6/s
	}
	u := cfg.APIURL
	if u == "" {
		u = prodURL
	}
	return &Broker{
		cfg: cfg, apiURL: strings.TrimRight(u, "/"), http: cfg.HTTP,
		now: cfg.Now, sleep: cfg.Sleep, limit: &limiter{gap: cfg.MinGap},
	}
}

func (b *Broker) Name() string { return "upbit" }

// market — 프로토콜 심볼 → 업비트 마켓 코드. 코드는 "KRW-BTC" 그대로 온다.
func market(s protocol.Symbol) (string, error) {
	if s.Exchange != Exchange || !strings.HasPrefix(s.Code, "KRW-") {
		// ★ KRW 마켓만 받는다. BTC-/USDT- 마켓은 사이징·손익이 원화가 아니라 원장이 틀어진다.
		return "", fmt.Errorf("%w: %s/%s (UPBIT KRW-* 만 지원)", broker.ErrUnknownSymbol, s.Exchange, s.Code)
	}
	return s.Code, nil
}

func (b *Broker) LotSize(protocol.Symbol) float64       { return Lot }
func (b *Broker) MinOrderValue(protocol.Symbol) float64 { return MinOrderKRW }

// ── 잔고 ────────────────────────────────────────────────────────────────────

type account struct {
	Currency     string `json:"currency"`
	Balance      fnum   `json:"balance"` // 주문 가능
	Locked       fnum   `json:"locked"`  // 미체결 주문에 묶임
	AvgBuyPrice  fnum   `json:"avg_buy_price"`
	UnitCurrency string `json:"unit_currency"`
}

func (b *Broker) accounts(ctx context.Context) ([]account, error) {
	var out []account
	err := b.do(ctx, http.MethodGet, "/accounts", nil, &out)
	return out, err
}

// Cash — ★ 두 층을 분리해 준다. locked 는 미체결 매수에 묶인 돈이라 주문에 못 쓴다.
func (b *Broker) Cash(ctx context.Context) (broker.Cash, error) {
	accs, err := b.accounts(ctx)
	if err != nil {
		return broker.Cash{}, err
	}
	for _, a := range accs {
		if a.Currency == "KRW" {
			return broker.Cash{
				Deposit:   float64(a.Balance + a.Locked),
				Orderable: float64(a.Balance),
				// 코인엔 증거금이 없다 — 가진 원화가 곧 한도다.
				Seed:     float64(a.Balance + a.Locked),
				Currency: "KRW",
			}, nil
		}
	}
	return broker.Cash{Currency: "KRW"}, nil
}

// Positions — KRW 로 산 코인 잔고.
//
// ★ Qty = balance + locked (TP 지정가에 묶인 것도 보유다). Sellable = balance.
// Qty 에서 locked 를 빼면 TP 를 건 순간 포지션이 "브로커에서 사라진" 것으로 보여 종결된다.
func (b *Broker) Positions(ctx context.Context) ([]broker.Holding, error) {
	accs, err := b.accounts(ctx)
	if err != nil {
		return nil, err
	}
	var out []broker.Holding
	for _, a := range accs {
		if a.Currency == "KRW" || a.UnitCurrency != "KRW" {
			continue
		}
		qty := float64(a.Balance + a.Locked)
		if qty <= 0 {
			continue
		}
		out = append(out, broker.Holding{
			Symbol:   protocol.Symbol{Exchange: Exchange, Code: "KRW-" + a.Currency},
			Qty:      qty,
			AvgPrice: float64(a.AvgBuyPrice),
			Sellable: float64(a.Balance),
		})
	}
	return out, nil
}

// ── 시세 ────────────────────────────────────────────────────────────────────

func (b *Broker) Quote(ctx context.Context, s protocol.Symbol) (broker.Quote, error) {
	m, err := market(s)
	if err != nil {
		return broker.Quote{}, err
	}
	var out []struct {
		TradePrice fnum `json:"trade_price"`
	}
	if err := b.public(ctx, "/ticker", url.Values{"markets": {m}}, &out); err != nil {
		return broker.Quote{}, err
	}
	if len(out) == 0 || out[0].TradePrice <= 0 {
		return broker.Quote{}, fmt.Errorf("%w: %s", broker.ErrUnknownSymbol, m)
	}
	return broker.Quote{Symbol: s, Price: float64(out[0].TradePrice), AsOf: b.now().UTC()}, nil
}

// ── 주문 ────────────────────────────────────────────────────────────────────

// chance — 매수 가능 금액을 수수료까지 감안해 묻는다 (키움 kt00011 의 대응물).
//
// ★ 시장가 매수는 주문 금액 **위에** 수수료를 따로 묶는다. 주문가능금액을 그대로 부르면
// insufficient_funds_bid 로 거부된다. ★ 조회 실패는 치명적이지 않다(fail-open) — 여기서 막으면
// API 혼잡 한 번에 진입 창을 통째로 잃는다.
func (b *Broker) chance(ctx context.Context, m string) (allow float64, ok bool) {
	var out struct {
		BidFee     fnum `json:"bid_fee"`
		BidAccount struct {
			Balance fnum `json:"balance"`
		} `json:"bid_account"`
	}
	if err := b.do(ctx, http.MethodGet, "/orders/chance", url.Values{"market": {m}}, &out); err != nil {
		return 0, false
	}
	return math.Floor(float64(out.BidAccount.Balance) / (1 + float64(out.BidFee))), true
}

// Buy — 시장가면 **금액**(qty × 기준가)으로 낸다.
//
// ★ 업비트 시장가 매수는 수량을 받지 않는다. 사이징이 준 qty 를 기준가로 금액화하고,
// 실제 체결 수량은 시세에 따라 조금 달라진다 — 원장엔 **체결 수량**이 실린다.
func (b *Broker) Buy(ctx context.Context, req broker.OrderRequest) (broker.Fill, error) {
	sub, err := b.SubmitBuy(ctx, req)
	if err != nil {
		return broker.Fill{BrokerOrderID: sub.OrderID, SubmittedAt: sub.SubmittedAt}, err
	}
	return b.waitFill(ctx, sub.OrderID, "buy", sub.RefPrice, sub.SubmittedAt)
}

// SubmitBuy — 매수를 내고 접수되면 바로 돌아온다. 시장가는 **금액**(qty × 기준가)으로 낸다 —
// 업비트 시장가 매수는 수량을 받지 않는다. Submitted.Qty 는 추정치이고 실제 수량은 체결로 정해진다.
func (b *Broker) SubmitBuy(ctx context.Context, req broker.OrderRequest) (broker.Submitted, error) {
	m, err := market(req.Symbol)
	if err != nil {
		return broker.Submitted{}, err
	}
	if req.Qty <= 0 {
		return broker.Submitted{}, fmt.Errorf("%w: 수량 0", broker.ErrInsufficient)
	}
	ref := req.RefPrice
	if ref <= 0 {
		q, err := b.Quote(ctx, req.Symbol)
		if err != nil {
			return broker.Submitted{}, err
		}
		ref = q.Price
	}

	qty := floor8(req.Qty)
	p := url.Values{"market": {m}, "side": {"bid"}}
	if req.LimitPrice > 0 {
		price := FloorToTick(req.LimitPrice)
		if qty*price < MinOrderKRW {
			return broker.Submitted{}, fmt.Errorf("%w: %.0f원 < 최소주문금액", broker.ErrInsufficient, qty*price)
		}
		p.Set("ord_type", "limit")
		p.Set("price", priceStr(price))
		p.Set("volume", volStr(qty))
	} else {
		amount := math.Floor(req.Qty * ref)
		// ★ 사전 축소 — 거래소가 인정하는 금액보다 많이 부르면 주문 자체가 거부된다.
		if allow, ok := b.chance(ctx, m); ok && allow < amount {
			amount = allow
		}
		if amount < MinOrderKRW {
			return broker.Submitted{}, fmt.Errorf("%w: %.0f원 < 최소주문금액", broker.ErrInsufficient, amount)
		}
		p.Set("ord_type", "price")
		p.Set("price", priceStr(amount))
		qty = floor8(amount / ref)
	}
	sub, err := b.submit(ctx, p)
	sub.Qty, sub.RefPrice = qty, ref
	return sub, err
}

func (b *Broker) Sell(ctx context.Context, req broker.OrderRequest) (broker.Fill, error) {
	sub, err := b.SubmitSell(ctx, req)
	if err != nil {
		return broker.Fill{BrokerOrderID: sub.OrderID, SubmittedAt: sub.SubmittedAt}, err
	}
	return b.waitFill(ctx, sub.OrderID, "sell", sub.RefPrice, sub.SubmittedAt)
}

// SubmitSell — 매도를 내고 접수되면 바로 돌아온다.
func (b *Broker) SubmitSell(ctx context.Context, req broker.OrderRequest) (broker.Submitted, error) {
	m, err := market(req.Symbol)
	if err != nil {
		return broker.Submitted{}, err
	}
	qty := floor8(req.Qty)
	if qty <= 0 {
		return broker.Submitted{}, fmt.Errorf("%w: 수량 0", broker.ErrNotEnoughShare)
	}
	p := url.Values{"market": {m}, "side": {"ask"}, "volume": {volStr(qty)}}
	if req.LimitPrice > 0 {
		p.Set("ord_type", "limit")
		p.Set("price", priceStr(CeilToTick(req.LimitPrice)))
	} else {
		p.Set("ord_type", "market")
	}
	sub, err := b.submit(ctx, p)
	sub.Qty, sub.RefPrice = qty, req.RefPrice
	return sub, err
}

// submit — 주문을 내고 uuid 를 받는다 (체결을 기다리지 않는다).
func (b *Broker) submit(ctx context.Context, p url.Values) (broker.Submitted, error) {
	submitted := b.now().UTC()
	var o order
	if err := b.do(ctx, http.MethodPost, "/orders", p, &o); err != nil {
		return broker.Submitted{SubmittedAt: submitted}, err
	}
	if o.UUID == "" {
		return broker.Submitted{SubmittedAt: submitted},
			fmt.Errorf("주문 uuid 가 비었다 — 체결을 추적할 수 없다 (%w)", errMaybeSent)
	}
	return broker.Submitted{OrderID: o.UUID, SubmittedAt: submitted}, nil
}

// PlaceTP — 익절 지정가 매도를 거래소에 위임한다. 체결을 기다리지 않는다.
func (b *Broker) PlaceTP(ctx context.Context, s protocol.Symbol, qty, price float64) (string, error) {
	m, err := market(s)
	if err != nil {
		return "", err
	}
	if qty <= 0 || price <= 0 {
		return "", fmt.Errorf("TP 수량·가격이 0")
	}
	var o order
	err = b.do(ctx, http.MethodPost, "/orders", url.Values{
		"market": {m}, "side": {"ask"}, "ord_type": {"limit"},
		"volume": {volStr(qty)}, "price": {priceStr(CeilToTick(price))},
	}, &o)
	if err != nil {
		return "", err
	}
	return o.UUID, nil
}

func (b *Broker) CancelOrder(ctx context.Context, _ protocol.Symbol, orderID string) error {
	if orderID == "" {
		return nil
	}
	return b.do(ctx, http.MethodDelete, "/order", url.Values{"uuid": {orderID}}, nil)
}

// LimitStatus — 걸어 둔 지정가(TP)의 체결 현황 (GET /order).
func (b *Broker) LimitStatus(ctx context.Context, _ protocol.Symbol, orderID string) (broker.LimitStatus, error) {
	var o order
	if err := b.do(ctx, http.MethodGet, "/order", url.Values{"uuid": {orderID}}, &o); err != nil {
		return broker.LimitStatus{Open: true}, err
	}
	st := broker.LimitStatus{Open: !o.final(), FeeKRW: float64(o.PaidFee), Known: true}
	var funds float64
	for _, t := range o.Trades {
		st.FilledQty += float64(t.Volume)
		f := float64(t.Funds)
		if f <= 0 {
			f = float64(t.Volume) * float64(t.Price)
		}
		funds += f
		if ts, err := time.Parse(time.RFC3339, t.CreatedAt); err == nil && ts.After(st.FilledAt) {
			st.FilledAt = ts.UTC()
		}
	}
	if st.FilledQty <= 0 && o.ExecutedVolume > 0 {
		st.FilledQty, funds = float64(o.ExecutedVolume), float64(o.ExecutedFunds)
	}
	if st.FilledQty > 0 && funds > 0 {
		st.AvgPrice = funds / st.FilledQty
	}
	return st, nil
}

// ── 체결 확인 ───────────────────────────────────────────────────────────────

type trade struct {
	Price     fnum   `json:"price"`
	Volume    fnum   `json:"volume"`
	Funds     fnum   `json:"funds"`
	CreatedAt string `json:"created_at"`
}

type order struct {
	UUID           string  `json:"uuid"`
	Side           string  `json:"side"`
	OrdType        string  `json:"ord_type"`
	State          string  `json:"state"` // wait | watch | done | cancel
	Volume         fnum    `json:"volume"`
	ExecutedVolume fnum    `json:"executed_volume"`
	ExecutedFunds  fnum    `json:"executed_funds"`
	PaidFee        fnum    `json:"paid_fee"`
	CreatedAt      string  `json:"created_at"`
	Trades         []trade `json:"trades"`
}

func (o order) final() bool { return o.State == "done" || o.State == "cancel" }

// waitFill — 주문이 끝날(done/cancel) 때까지 조회한다.
//
// ★ 시간 안에 안 끝나면 **취소하고** 한 번 더 본다. 키움과 달리 업비트는 취소 후 상태가
// 확정되므로, "주문이 살아 있는지 모르는" 상태로 상위에 넘기지 않는다. 잔량이 장부 밖에서
// 나중에 체결되면 그게 유령이다.
func (b *Broker) waitFill(ctx context.Context, uuid, side string, ref float64,
	submitted time.Time) (broker.Fill, error) {

	deadline := b.now().Add(b.cfg.FillTimeout)
	var last order
	seen := false
	for {
		var o order
		if err := b.do(ctx, http.MethodGet, "/order", url.Values{"uuid": {uuid}}, &o); err == nil {
			last, seen = o, true
			if o.final() {
				return b.toFill(o, side, ref, submitted, false)
			}
		}
		if !b.now().Before(deadline) {
			break
		}
		b.sleep(b.cfg.FillPoll)
	}

	cancelErr := b.CancelOrder(ctx, protocol.Symbol{}, uuid)
	var o order
	if err := b.do(ctx, http.MethodGet, "/order", url.Values{"uuid": {uuid}}, &o); err == nil {
		last, seen = o, true
	}
	if !seen {
		return broker.Fill{BrokerOrderID: uuid, SubmittedAt: submitted},
			fmt.Errorf("체결 확인 실패 (uuid %s) — %w", uuid, errMaybeSent)
	}
	if !last.final() && cancelErr != nil {
		// ★ 취소도 안 됐고 주문은 살아 있다. 숨기지 않는다.
		return broker.Fill{BrokerOrderID: uuid, SubmittedAt: submitted},
			fmt.Errorf("체결 대기 초과 + 취소 실패 (uuid %s, state=%s): %v", uuid, last.State, cancelErr)
	}
	return b.toFill(last, side, ref, submitted, true)
}

// toFill — 체결가는 trades 의 Σfunds/Σvolume, 수수료는 paid_fee(실측).
func (b *Broker) toFill(o order, side string, ref float64, submitted time.Time,
	timedOut bool) (broker.Fill, error) {

	var qty, funds float64
	var last time.Time
	for _, t := range o.Trades {
		v := float64(t.Volume)
		qty += v
		f := float64(t.Funds)
		if f <= 0 {
			f = v * float64(t.Price)
		}
		funds += f
		if ts, err := time.Parse(time.RFC3339, t.CreatedAt); err == nil && ts.After(last) {
			last = ts
		}
	}

	detail := ""
	if qty <= 0 && o.ExecutedVolume > 0 {
		// trades 가 비어 오는 경우 — 주문 단위 합계로 대신한다.
		qty, funds = float64(o.ExecutedVolume), float64(o.ExecutedFunds)
		if funds <= 0 {
			detail = "trades·executed_funds 없음 — 체결가 미상"
		}
	}
	if qty <= 0 {
		return broker.Fill{BrokerOrderID: o.UUID, SubmittedAt: submitted},
			fmt.Errorf("미체결로 종료 (uuid %s, state=%s)", o.UUID, o.State)
	}
	if last.IsZero() {
		// ★ 체결 시각을 모르면 "지금" 으로 둔다 — 지연의 상한일 뿐이라고 detail 에 남긴다.
		last = b.now()
		if detail == "" {
			detail = "체결 시각 미상 — 조회 시각으로 대체(지연 상한)"
		}
	}

	price := 0.0
	if funds > 0 {
		price = funds / qty
	}

	// ★ 부분 판정 — 시장가 매수(price)는 잔여 먼지로 늘 cancel 이라 수량으로 판단할 수 없다.
	// 수량 주문(market 매도·limit)은 요청 수량 대비로, 그리고 우리가 취소했다면 부분이다.
	partial := timedOut
	if o.OrdType != "price" && float64(o.Volume) > 0 && qty < float64(o.Volume)-1e-12 {
		partial = true
	}

	return broker.Fill{
		BrokerOrderID: o.UUID,
		Qty:           qty,
		Price:         price,
		SubmittedAt:   submitted,
		FilledAt:      last.UTC(),
		FeeKRW:        float64(o.PaidFee),
		SlippageBp:    broker.SlippageBp(side, ref, price),
		Partial:       partial,
		Detail:        detail,
	}, nil
}

// IsMaybeSent — 이 오류가 "주문이 나갔는지 모른다" 는 뜻인가. 호출자는 다음 틱의 잔고 조회로
// 실상태를 다시 봐야 하며, **같은 주문을 즉시 다시 내면 안 된다**.
func IsMaybeSent(err error) bool { return errors.Is(err, errMaybeSent) }
