// Package kiwoom 은 키움증권 REST 드라이버다.
//
// ★ 이 파일은 키움 하나만 안다. 다른 증권사와 공통화하지 않는다 —
// 겉만 비슷하고 응답 스키마가 전부 다르므로, 섣부른 공통화는 한 증권사의 착각을
// 전 증권사로 번지게 한다.
//
// 여기 박힌 함정은 전부 라이브에서 실제로 당한 것들이다:
//   - 1계정 1토큰 (token.go)
//   - 숫자가 문자열로 오고 부호·콤마·프리픽스가 섞인다 (num.go)
//   - 잔고 stk_cd 에 "A" 프리픽스 (num.go code())
//   - 예수금(entr) ≠ 주문가능금액(ord_alowa) — 사이징을 예수금으로 하면 거부된다
//   - 봇이 계산한 수량을 키움이 거부한다 → kt00011 로 **사전 축소**
//   - 시장가는 분할체결된다 → 부분을 조기 반환하면 잔여가 장부 밖 유령이 된다
//   - 호가단위 상수표는 낡는다 (tick.go)
package kiwoom

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

const (
	prodURL = "https://api.kiwoom.com"
	mockURL = "https://mockapi.kiwoom.com"

	pathOrder = "/api/dostk/ordr"
	pathAcnt  = "/api/dostk/acnt"
	pathCond  = "/api/dostk/mrkcond"

	apiBuy       = "kt10000" // 주식 매수주문
	apiSell      = "kt10001" // 주식 매도주문
	apiCancel    = "kt10003" // 주식 취소주문
	apiBalance   = "kt00005" // 체결잔고요청
	apiBuyableQt = "kt00011" // 증거금율별 주문가능수량
	apiFills     = "ka10076" // 체결요청
	apiQuote     = "ka10007" // 시세표성정보요청

	// trde_tp — 거래구분. ★ 3=시장가 / 0=보통(지정가).
	// 진단 스크립트에 1 을 지정가로 쓴 흔적이 있으나 라이브 경로는 0 이다.
	tradeMarket = "3"
	tradeLimit  = "0"
)

type Config struct {
	AppKey    string
	SecretKey string
	// DataDir — 토큰 파일 기본 위치.
	DataDir string
	// TokenFile — 토큰 파일 경로를 직접 지정한다.
	//
	// ★ flat6 와 **같은 앱키**를 쓴다면 반드시 flat6 의 파일을 가리켜야 한다
	// (예: ../yovel-flat6/data/kiwoom_token.json). 각자 발급하면 1계정 1토큰이라
	// 서로의 토큰을 죽이고, 증상은 "가끔 8005" 가 아니라 "상대 세션 통째 유실" 이다.
	TokenFile string
	// Mock — 모의투자 도메인 사용 (KRX 만 지원).
	Mock bool
	// APIURL — 비면 Mock 에 따라 기본값.
	APIURL string

	HTTP *http.Client
	Now  func() time.Time
	// Sleep — 재시도·체결 폴링 대기 (테스트에서 즉시 반환시킬 수 있도록).
	Sleep func(time.Duration)

	// FillTimeout — 체결 대기 상한. ★ 이 시간을 넘으면 부분체결로 보고한다.
	FillTimeout time.Duration
	// FillPoll — 체결 조회 간격.
	FillPoll time.Duration

	// IsETP — ETF/ETN 여부. 호가단위가 다르다.
	IsETP func(protocol.Symbol) bool
}

type Broker struct {
	cfg    Config
	apiURL string
	http   *http.Client
	now    func() time.Time
	sleep  func(time.Duration)
	tokens *tokenStore
}

func New(cfg Config) (*Broker, error) {
	if cfg.AppKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("appkey/secretkey 가 없다")
	}
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("data-dir 이 없다 (토큰 공유 파일 위치)")
	}
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
		cfg.FillPoll = time.Second
	}

	url := cfg.APIURL
	if url == "" {
		url = prodURL
		if cfg.Mock {
			url = mockURL
		}
	}

	b := &Broker{cfg: cfg, apiURL: url, http: cfg.HTTP, now: cfg.Now, sleep: cfg.Sleep}
	tokenPath := cfg.TokenFile
	if tokenPath == "" {
		tokenPath = filepath.Join(cfg.DataDir, "kiwoom_token.json")
	}
	b.tokens = newTokenStore(tokenPath, cfg.AppKey, cfg.SecretKey, url, cfg.HTTP, cfg.Now)
	return b, nil
}

func (b *Broker) Name() string { return "kiwoom" }

func (b *Broker) etp(s protocol.Symbol) bool {
	return b.cfg.IsETP != nil && b.cfg.IsETP(s)
}

// ── 잔고 ────────────────────────────────────────────────────────────────────

type balanceResp struct {
	Entr     string            `json:"entr"`      // 예수금 (L1)
	OrdAlowa string            `json:"ord_alowa"` // 주문가능현금 (L2)
	Rows     []json.RawMessage `json:"stk_cntr_remn"`
}

func (b *Broker) balance(ctx context.Context) (balanceResp, error) {
	var out balanceResp
	err := b.call(ctx, apiBalance, pathAcnt, map[string]string{"dmst_stex_tp": "KRX"}, &out)
	return out, err
}

// Cash — ★ 두 층을 분리해 준다.
//
// 예수금이 있어도 주문가능금액이 0 일 수 있다 (보유 종목 증거금·미체결·해외 원화배정).
// 사이징을 예수금으로 하면 그대로 주문 거부가 된다.
func (b *Broker) Cash(ctx context.Context) (broker.Cash, error) {
	r, err := b.balance(ctx)
	if err != nil {
		return broker.Cash{}, err
	}
	return broker.Cash{
		Deposit:   num(r.Entr),
		Orderable: num(r.OrdAlowa),
		Currency:  "KRW",
	}, nil
}

func (b *Broker) Positions(ctx context.Context) ([]broker.Holding, error) {
	r, err := b.balance(ctx)
	if err != nil {
		return nil, err
	}

	var out []broker.Holding
	for _, raw := range r.Rows {
		var row struct {
			StkCd  string `json:"stk_cd"`
			CurQty string `json:"cur_qty"`
			BuyUv  string `json:"buy_uv"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			continue
		}
		qty := num(row.CurQty)
		if qty <= 0 {
			continue
		}
		out = append(out, broker.Holding{
			Symbol:   protocol.Symbol{Exchange: "KRX", Code: code(row.StkCd)},
			Qty:      qty,
			AvgPrice: num(row.BuyUv),
			// ★ 키움 잔고는 "매도 가능 수량" 을 따로 주지 않는다. 미체결 매도(위임한 TP)가
			// 걸려 있으면 실제 가용은 이보다 적다 — 그래서 청산 경로가 TP 를 **먼저 취소**한다.
			Sellable: qty,
		})
	}
	return out, nil
}

// ── 시세 ────────────────────────────────────────────────────────────────────

func (b *Broker) Quote(ctx context.Context, s protocol.Symbol) (broker.Quote, error) {
	var out struct {
		CurPrc string `json:"cur_prc"`
	}
	if err := b.call(ctx, apiQuote, pathCond, map[string]string{"stk_cd": s.Code}, &out); err != nil {
		return broker.Quote{}, err
	}
	// ★ 현재가는 부호가 붙어 올 수 있다("+72100" = 상승). 절대값이 가격이다.
	p := math.Abs(num(out.CurPrc))
	if p <= 0 {
		return broker.Quote{}, fmt.Errorf("%w: %s (cur_prc=%q)", broker.ErrUnknownSymbol, s.Code, out.CurPrc)
	}
	return broker.Quote{Symbol: s, Price: p, AsOf: b.now().UTC()}, nil
}

func (b *Broker) LotSize(protocol.Symbol) float64       { return 1 }
func (b *Broker) MinOrderValue(protocol.Symbol) float64 { return 0 }

// ── 주문 ────────────────────────────────────────────────────────────────────

// buyableQty — kt00011 로 키움이 인정하는 매수가능수량을 묻는다.
//
// ★ 왜 필수인가: 봇이 계산한 수량을 키움이 거부한 사고가 실재한다(9주 시도 → "6주 가능" 거부).
// 종목별 증거금률·미체결·메인 매매 잠금까지 반영한 정답은 서버만 안다.
// min_ord_alowq = 미수불가(100% 현금) 기준이라 가장 보수적이다.
//
// ★ 조회 실패는 치명적이지 않다 — 봇 추정을 그대로 쓴다(fail-open). 여기서 막으면
// API 혼잡 한 번에 진입 윈도를 통째로 잃는다.
func (b *Broker) buyableQty(ctx context.Context, s protocol.Symbol, price float64) (float64, bool) {
	var out struct {
		MinOrdAlowq string `json:"min_ord_alowq"`
	}
	body := map[string]string{"stk_cd": s.Code, "uv": strconv.Itoa(int(math.Round(price)))}
	if err := b.call(ctx, apiBuyableQt, pathAcnt, body, &out); err != nil {
		return 0, false
	}
	v, ok := numOK(out.MinOrdAlowq)
	return v, ok
}

func (b *Broker) Buy(ctx context.Context, req broker.OrderRequest) (broker.Fill, error) {
	qty := math.Floor(req.Qty)
	if qty <= 0 {
		return broker.Fill{}, fmt.Errorf("%w: 수량 0", broker.ErrInsufficient)
	}

	ref := req.RefPrice
	if ref <= 0 {
		q, err := b.Quote(ctx, req.Symbol)
		if err != nil {
			return broker.Fill{}, err
		}
		ref = q.Price
	}

	// ★ 사전 축소. 키움이 인정하는 수량보다 많이 부르면 주문 자체가 거부된다.
	if allow, ok := b.buyableQty(ctx, req.Symbol, ref); ok && allow < qty {
		if allow <= 0 {
			return broker.Fill{}, fmt.Errorf("%w: 키움 매수가능수량 0", broker.ErrInsufficient)
		}
		qty = allow
	}

	body := map[string]string{
		"dmst_stex_tp": "KRX",
		"stk_cd":       req.Symbol.Code,
		"ord_qty":      strconv.FormatFloat(qty, 'f', -1, 64),
		"ord_uv":       "",
		"trde_tp":      tradeMarket,
		"cond_uv":      "",
	}
	if req.LimitPrice > 0 {
		body["trde_tp"] = tradeLimit
		body["ord_uv"] = strconv.Itoa(int(FloorToTick(req.LimitPrice, b.etp(req.Symbol))))
	}

	submitted := b.now().UTC()
	var out struct {
		OrdNo string `json:"ord_no"`
	}
	if err := b.call(ctx, apiBuy, pathOrder, body, &out); err != nil {
		return broker.Fill{}, err
	}
	return b.waitFill(ctx, req.Symbol, out.OrdNo, qty, "buy", ref, submitted)
}

func (b *Broker) Sell(ctx context.Context, req broker.OrderRequest) (broker.Fill, error) {
	qty := math.Floor(req.Qty)
	if qty <= 0 {
		return broker.Fill{}, fmt.Errorf("%w: 수량 0", broker.ErrNotEnoughShare)
	}

	body := map[string]string{
		"dmst_stex_tp": "KRX",
		"stk_cd":       req.Symbol.Code,
		"ord_qty":      strconv.FormatFloat(qty, 'f', -1, 64),
		"ord_uv":       "",
		"trde_tp":      tradeMarket,
		"cond_uv":      "",
	}
	if req.LimitPrice > 0 {
		body["trde_tp"] = tradeLimit
		body["ord_uv"] = strconv.Itoa(int(CeilToTick(req.LimitPrice, b.etp(req.Symbol))))
	}

	submitted := b.now().UTC()
	var out struct {
		OrdNo string `json:"ord_no"`
	}
	if err := b.call(ctx, apiSell, pathOrder, body, &out); err != nil {
		return broker.Fill{}, err
	}
	return b.waitFill(ctx, req.Symbol, out.OrdNo, qty, "sell", req.RefPrice, submitted)
}

// PlaceTP — 익절 지정가 매도를 브로커에 위임한다. 체결을 기다리지 않는다.
//
// ★ 이게 exit 3층 중 제일 튼튼한 층이다: 데몬도 서버도 죽어도 이건 체결된다.
func (b *Broker) PlaceTP(ctx context.Context, s protocol.Symbol, qty, price float64) (string, error) {
	if qty <= 0 || price <= 0 {
		return "", fmt.Errorf("TP 수량·가격이 0")
	}
	body := map[string]string{
		"dmst_stex_tp": "KRX",
		"stk_cd":       s.Code,
		"ord_qty":      strconv.FormatFloat(math.Floor(qty), 'f', -1, 64),
		// ★ 매도 지정가는 호가단위로 **올린다**. 내리면 의도보다 싸게 팔린다.
		"ord_uv":  strconv.Itoa(int(CeilToTick(price, b.etp(s)))),
		"trde_tp": tradeLimit,
		"cond_uv": "",
	}
	var out struct {
		OrdNo string `json:"ord_no"`
	}
	if err := b.call(ctx, apiSell, pathOrder, body, &out); err != nil {
		return "", err
	}
	return out.OrdNo, nil
}

func (b *Broker) CancelOrder(ctx context.Context, s protocol.Symbol, orderID string) error {
	if orderID == "" {
		return nil
	}
	body := map[string]string{
		"dmst_stex_tp": "KRX",
		"orig_ord_no":  orderID,
		"stk_cd":       s.Code,
		"cncl_qty":     "0", // 0 = 잔량 전부
	}
	return b.call(ctx, apiCancel, pathOrder, body, nil)
}

// ── 체결 확인 ───────────────────────────────────────────────────────────────

// waitFill 은 주문번호로 체결을 확인한다.
//
// ★ 시장가는 분할체결된다. 부분(예: 13/29주)을 조기 반환하면 남은 16주가 봇 장부 밖
// 유령이 되고, 그 유령은 stop 도 TP 도 없이 방치된다. 그래서 **전량 또는 타임아웃**까지 기다린다.
//
// ★ 수수료는 요율로 추정하지 않고 ka10076 이 주는 실측(tdy_trde_cmsn + tdy_trde_tax)을 쓴다.
// 요율은 계좌마다·이벤트마다 다르고, 실제로 "기록 수수료가 왕복 1.9배 과다" 였던 적이 있다.
func (b *Broker) waitFill(ctx context.Context, s protocol.Symbol, ordNo string, want float64,
	side string, ref float64, submitted time.Time) (broker.Fill, error) {

	if ordNo == "" {
		return broker.Fill{}, fmt.Errorf("주문번호가 비었다 — 체결을 추적할 수 없다")
	}

	deadline := b.now().Add(b.cfg.FillTimeout)
	var last fillAgg

	for {
		agg, err := b.fetchFills(ctx, s, ordNo)
		if err == nil {
			last = agg
			if agg.qty >= want-1e-9 {
				return b.toFill(ordNo, agg, side, ref, submitted, false), nil
			}
		}
		if !b.now().Before(deadline) {
			break
		}
		b.sleep(b.cfg.FillPoll)
	}

	if last.qty <= 0 {
		// ★ 조회로 확인 못 했다고 "미체결" 로 단정하지 않는다. 주문은 나갔을 수 있다.
		// 상위가 다음 틱의 브로커 조회로 실상태를 다시 본다.
		return broker.Fill{BrokerOrderID: ordNo, SubmittedAt: submitted},
			fmt.Errorf("체결 확인 실패 (주문번호 %s) — 주문은 나갔을 수 있다", ordNo)
	}
	return b.toFill(ordNo, last, side, ref, submitted, true), nil
}

type fillAgg struct {
	qty    float64
	amount float64 // Σ(체결가 × 수량)
	fee    float64
	last   time.Time
}

func (b *Broker) toFill(ordNo string, a fillAgg, side string, ref float64,
	submitted time.Time, partial bool) broker.Fill {

	avg := 0.0
	if a.qty > 0 {
		avg = a.amount / a.qty
	}
	filled := a.last
	if filled.IsZero() {
		filled = b.now().UTC()
	}
	return broker.Fill{
		BrokerOrderID: ordNo,
		Qty:           a.qty,
		Price:         avg,
		SubmittedAt:   submitted,
		FilledAt:      filled,
		FeeKRW:        a.fee,
		SlippageBp:    broker.SlippageBp(side, ref, avg),
		Partial:       partial,
	}
}

// fetchFills — ka10076. ★ 금일 체결만 반환한다 (전일 손매도는 여기서 안 보인다).
func (b *Broker) fetchFills(ctx context.Context, s protocol.Symbol, ordNo string) (fillAgg, error) {
	body := map[string]string{
		"stk_cd":  s.Code,
		"qry_tp":  "1", // 1 = 종목
		"sell_tp": "0", // 0 = 전체
		"ord_no":  "",  // 빈값 = 전체 (최근순)
		"stex_tp": "0", // 0 = 통합
	}
	var raw map[string]json.RawMessage
	if err := b.call(ctx, apiFills, pathAcnt, body, &raw); err != nil {
		return fillAgg{}, err
	}

	var agg fillAgg
	for _, row := range pickRows(raw, "cntr_qty") {
		var r struct {
			OrdNo    string `json:"ord_no"`
			CntrQty  string `json:"cntr_qty"`
			CntrPric string `json:"cntr_pric"`
			Cmsn     string `json:"tdy_trde_cmsn"`
			Tax      string `json:"tdy_trde_tax"`
			OrdTm    string `json:"ord_tm"`
		}
		if err := json.Unmarshal(row, &r); err != nil {
			continue
		}
		if r.OrdNo != ordNo {
			continue
		}
		q := math.Abs(num(r.CntrQty))
		if q <= 0 {
			continue // 미체결·취소(체결 0) 행
		}
		agg.qty += q
		agg.amount += q * math.Abs(num(r.CntrPric))
		agg.fee += num(r.Cmsn) + num(r.Tax)
		if t, ok := parseOrdTime(r.OrdTm, b.now()); ok && t.After(agg.last) {
			agg.last = t
		}
	}
	return agg, nil
}

// pickRows 는 응답에서 "marker 필드를 가진 객체 배열" 을 찾아 준다.
//
// ★ 리스트 키 이름을 하드코딩하지 않는 이유: 키움 응답의 배열 키 이름은 API 마다 다르고
// (stk_cntr_remn / cntr / dt_stk_div_rlzt_pl …) 문서와 실제가 어긋난 전례가 있다.
// 키 이름이 틀리면 "체결 0건" 으로 읽혀 **주문은 나갔는데 장부엔 없는** 최악의 형태가 된다.
// 그래서 이름 대신 **내용**으로 찾는다.
func pickRows(raw map[string]json.RawMessage, marker string) []json.RawMessage {
	for _, v := range raw {
		var arr []json.RawMessage
		if err := json.Unmarshal(v, &arr); err != nil || len(arr) == 0 {
			continue
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(arr[0], &probe); err != nil {
			continue
		}
		if _, ok := probe[marker]; ok {
			return arr
		}
	}
	return nil
}

// parseOrdTime — ord_tm 은 "HHMMSS" 다. 날짜가 없으므로 오늘로 붙인다.
func parseOrdTime(s string, now time.Time) (time.Time, bool) {
	if len(s) != 6 {
		return time.Time{}, false
	}
	h, err1 := strconv.Atoi(s[0:2])
	m, err2 := strconv.Atoi(s[2:4])
	sec, err3 := strconv.Atoi(s[4:6])
	if err1 != nil || err2 != nil || err3 != nil {
		return time.Time{}, false
	}
	n := now.Local()
	return time.Date(n.Year(), n.Month(), n.Day(), h, m, sec, 0, time.Local).UTC(), true
}
