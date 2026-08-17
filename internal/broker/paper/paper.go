// Package paper 는 실주문 없이 도는 브로커다.
//
// ★ 장난감이 아니라 기본값이다. 데몬의 기본 모드가 paper 이므로, 라이브로 넘어가기 전
// 모든 배선(계획 → 주문 → 원장 → 종결)이 여기서 먼저 완결된다.
//
// ★ 다만 이 브로커의 체결은 **낙관적**이다: 원하는 수량이 원하는 가격 근처에서 다 채워진다.
// 라이브에서는 부분체결·거부·잔고 잠김이 실재하므로, 여기서 통과했다고 라이브가 통과하는 게
// 아니다. 그래서 슬리피지·수수료는 0 이 아니라 **설정된 값으로 물린다** — 비용이 0 인 시뮬은
// 손익분기 근처 전략의 판정을 통째로 뒤집는다.
package paper

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

type Config struct {
	// Cash — 시작 예수금.
	Cash float64
	// FeeBpBuy / FeeBpSell — 편도 비용 (bp). 왕복이 아니라 편도다.
	//
	// ★★ **매수와 매도를 나눠 둔 이유는 대칭이 아니기 때문이다.** 국내 주식은 매수엔
	// 위탁수수료만 붙고, 매도엔 거기에 **증권거래세**가 얹힌다. 하나의 상수로 뭉개면
	// 크기만 틀리는 게 아니라 **모양**이 틀린다 — 회전율이 높을수록, 보유가 짧을수록
	// 왜곡 방향이 달라져서 "얼마 틀렸는지" 조차 일정하지 않다.
	// (옛 코드는 대칭 15bp 였고, 그래서 매수 체결에 국내엔 존재하지 않는 0.15% 가 붙었다.)
	//
	// ★ 값은 **여전히 추정치다.** 확정은 실제 체결 통지의 `fee`/`tax` 필드로만 된다
	// (flat6 `exec_reports` 가 원문을 보존한다). 그때까지 이 숫자를 성과 근거로 쓰지 말 것.
	FeeBpBuy  float64
	FeeBpSell float64
	// SlipBp — 시장가 체결이 기준가에서 밀리는 정도 (bp). 매수는 비싸게, 매도는 싸게.
	SlipBp float64
	// Lot — 최소 주문 단위.
	Lot float64
	// MinOrderValue — 최소 주문 금액.
	MinOrderValue float64
	// Now — 시각 주입 (테스트용). nil 이면 time.Now().
	Now func() time.Time
	// Price — 기준가 공급. 없으면 Quote 가 실패한다.
	Price func(protocol.Symbol) (float64, bool)
}

type Broker struct {
	mu       sync.Mutex
	cfg      Config
	cash     float64
	holdings map[string]*broker.Holding
	tpOrders map[string]tpOrder
	seq      int
}

type tpOrder struct {
	symbol protocol.Symbol
	qty    float64
	price  float64
}

func New(cfg Config) *Broker {
	if cfg.Lot <= 0 {
		cfg.Lot = 1
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Broker{
		cfg:      cfg,
		cash:     cfg.Cash,
		holdings: map[string]*broker.Holding{},
		tpOrders: map[string]tpOrder{},
	}
}

func (b *Broker) Name() string { return "paper" }

func key(s protocol.Symbol) string { return s.Exchange + ":" + s.Code }

func (b *Broker) Positions(context.Context) ([]broker.Holding, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]broker.Holding, 0, len(b.holdings))
	for _, h := range b.holdings {
		out = append(out, *h)
	}
	return out, nil
}

func (b *Broker) Cash(context.Context) (broker.Cash, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// paper 에서는 두 층이 같다. ★ 라이브에서는 절대 같지 않다는 걸 잊지 말 것.
	return broker.Cash{Deposit: b.cash, Orderable: b.cash, Currency: "KRW"}, nil
}

func (b *Broker) Quote(_ context.Context, s protocol.Symbol) (broker.Quote, error) {
	if b.cfg.Price == nil {
		return broker.Quote{}, broker.ErrUnknownSymbol
	}
	p, ok := b.cfg.Price(s)
	if !ok || p <= 0 {
		return broker.Quote{}, fmt.Errorf("%w: %s", broker.ErrUnknownSymbol, s.Code)
	}
	return broker.Quote{Symbol: s, Price: p, AsOf: b.cfg.Now()}, nil
}

func (b *Broker) LotSize(protocol.Symbol) float64       { return b.cfg.Lot }
func (b *Broker) MinOrderValue(protocol.Symbol) float64 { return b.cfg.MinOrderValue }

// fillPrice — 지정가면 그대로, 시장가면 기준가에서 SlipBp 만큼 불리하게.
func (b *Broker) fillPrice(req broker.OrderRequest, side string, ref float64) float64 {
	if req.LimitPrice > 0 {
		return req.LimitPrice
	}
	slip := ref * b.cfg.SlipBp / 10000
	if side == "buy" {
		return ref + slip
	}
	return ref - slip
}

func (b *Broker) refPrice(req broker.OrderRequest) (float64, error) {
	if req.RefPrice > 0 {
		return req.RefPrice, nil
	}
	q, err := b.Quote(context.Background(), req.Symbol)
	if err != nil {
		return 0, err
	}
	return q.Price, nil
}

func (b *Broker) Buy(_ context.Context, req broker.OrderRequest) (broker.Fill, error) {
	ref, err := b.refPrice(req)
	if err != nil {
		return broker.Fill{}, err
	}
	price := b.fillPrice(req, "buy", ref)
	notional := price * req.Qty
	fee := notional * b.cfg.FeeBpBuy / 10000

	b.mu.Lock()
	defer b.mu.Unlock()
	if notional+fee > b.cash {
		return broker.Fill{}, fmt.Errorf("%w: 필요 %.0f, 가용 %.0f", broker.ErrInsufficient, notional+fee, b.cash)
	}
	b.cash -= notional + fee

	h := b.holdings[key(req.Symbol)]
	if h == nil {
		h = &broker.Holding{Symbol: req.Symbol}
		b.holdings[key(req.Symbol)] = h
	}
	total := h.AvgPrice*h.Qty + notional
	h.Qty += req.Qty
	h.Sellable = h.Qty
	h.AvgPrice = total / h.Qty

	now := b.cfg.Now()
	b.seq++
	return broker.Fill{
		BrokerOrderID: fmt.Sprintf("paper-%d", b.seq),
		Qty:           req.Qty,
		Price:         price,
		SubmittedAt:   now,
		FilledAt:      now,
		FeeKRW:        fee,
		SlippageBp:    broker.SlippageBp("buy", ref, price),
	}, nil
}

func (b *Broker) Sell(_ context.Context, req broker.OrderRequest) (broker.Fill, error) {
	ref, err := b.refPrice(req)
	if err != nil {
		return broker.Fill{}, err
	}
	price := b.fillPrice(req, "sell", ref)

	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.holdings[key(req.Symbol)]
	if h == nil || h.Sellable < req.Qty {
		have := 0.0
		if h != nil {
			have = h.Sellable
		}
		return broker.Fill{}, fmt.Errorf("%w: 요청 %.0f, 보유 %.0f", broker.ErrNotEnoughShare, req.Qty, have)
	}

	notional := price * req.Qty
	fee := notional * b.cfg.FeeBpSell / 10000
	b.cash += notional - fee

	h.Qty -= req.Qty
	h.Sellable = h.Qty
	if h.Qty <= 0 {
		delete(b.holdings, key(req.Symbol))
	}

	now := b.cfg.Now()
	b.seq++
	return broker.Fill{
		BrokerOrderID: fmt.Sprintf("paper-%d", b.seq),
		Qty:           req.Qty,
		Price:         price,
		SubmittedAt:   now,
		FilledAt:      now,
		FeeKRW:        fee,
		SlippageBp:    broker.SlippageBp("sell", ref, price),
	}, nil
}

// PlaceTP — paper 에서는 주문서만 기억한다. ★ 자동 체결시키지 않는다.
// 지정가가 언제 체결되는지는 시세를 봐야 알고, 그걸 낙관적으로 흉내내면
// "백테는 TP 가 다 맞았는데 라이브는 아니다" 를 만든다.
func (b *Broker) PlaceTP(_ context.Context, s protocol.Symbol, qty, price float64) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.holdings[key(s)]
	if h == nil || h.Qty < qty {
		return "", broker.ErrNotEnoughShare
	}
	b.seq++
	id := fmt.Sprintf("paper-tp-%d", b.seq)
	b.tpOrders[id] = tpOrder{symbol: s, qty: qty, price: price}
	// 지정가가 걸리면 그만큼은 팔 수 없다 — 라이브의 잔고 잠김을 흉내낸다.
	h.Sellable = h.Qty - qty
	if h.Sellable < 0 {
		h.Sellable = 0
	}
	return id, nil
}

func (b *Broker) CancelOrder(_ context.Context, s protocol.Symbol, orderID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.tpOrders[orderID]
	if !ok {
		return nil // 이미 없는 주문을 취소하는 건 성공으로 친다 (멱등)
	}
	delete(b.tpOrders, orderID)
	if h := b.holdings[key(o.symbol)]; h != nil {
		h.Sellable = h.Qty
	}
	return nil
}

// OpenTPOrders 는 테스트·진단용.
func (b *Broker) OpenTPOrders() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.tpOrders)
}

// SettleLimits — 들고 있던 TP 지정가 중 **가격이 닿은 것**을 체결시킨다.
//
// ★★ 왜 필요한가 — 라이브에서 TP 는 브로커(거래소)가 들고 있다가 체결시키고, 데몬은 그걸
// «포지션이 사라졌다» 로 사후 감지한다. paper 에는 그 브로커가 없으므로, 이걸 안 하면
// **TP 로 닫히는 경로가 아예 없다** — 승자가 전부 시간청산까지 끌려가서 손익 분포가
// 라이브와 구조적으로 달라진다. (reflex 의 페이퍼는 엔진이 TP 를 직접 봐서 이 문제가 없었다.
// 콕핏은 «끊겨도 체결되는 층» 을 얻으려고 브로커에 위임했고, 그 대가가 여기였다.)
//
// ★ 낙관적으로 흉내내지 않는다:
//
//	· 체결가는 **지정가 그대로**다. 갭으로 훌쩍 넘어가면 실제론 더 좋게 체결되지만
//	  그걸 반영하지 않는다 = 우리에게 불리한 쪽.
//	· 폴링 시점에만 본다. 폴링 사이에 스쳤다 내려온 건 **놓친다** = 역시 불리한 쪽.
//	⟹ 두 편향 모두 «paper 가 라이브보다 좋아 보이는» 방향이 아니다.
func (b *Broker) SettleLimits(context.Context) ([]broker.LimitFill, error) {
	if b.cfg.Price == nil {
		return nil, nil // 시세원이 없으면 체결 판정 자체가 불가 — 조용히 아무것도 안 한다
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	var out []broker.LimitFill
	for id, o := range b.tpOrders {
		px, ok := b.cfg.Price(o.symbol)
		if !ok || px < o.price {
			continue
		}
		h := b.holdings[key(o.symbol)]
		if h == nil || h.Qty < o.qty {
			// 장부와 실물이 어긋났다. ★ 추측해서 맞추지 않고 주문만 거둔다.
			delete(b.tpOrders, id)
			continue
		}

		notional := o.price * o.qty
		fee := notional * b.cfg.FeeBpSell / 10000
		b.cash += notional - fee

		h.Qty -= o.qty
		h.Sellable = h.Qty
		if h.Qty <= 0 {
			delete(b.holdings, key(o.symbol))
		}
		delete(b.tpOrders, id)

		now := b.cfg.Now()
		out = append(out, broker.LimitFill{
			OrderID: id,
			Symbol:  o.symbol,
			Fill: broker.Fill{
				BrokerOrderID: id,
				Qty:           o.qty,
				Price:         o.price,
				SubmittedAt:   now,
				FilledAt:      now,
				FeeKRW:        fee,
				// ★ 지정가는 원하는 가격에 체결된다 = 슬리피지 0. 시장가와 달리 이건 사실이다.
				SlippageBp: 0,
			},
		})
	}
	return out, nil
}
