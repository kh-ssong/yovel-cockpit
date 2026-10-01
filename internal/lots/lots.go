// Package lots 는 로트(진입 한 번 = intent 하나)를 사람이 보는 모양으로 만든다 — UI 의 재료.
//
// HTS 는 종목별 합계만 보여준다. 여기는 두 가지를 준다:
//   - Build     — 로트 목록: 어느 전략이, 언제, 얼마에, 어떤 청산 조건으로 들고 있나 + 평가손익
//   - Reconcile — 종목별 대조: 브로커 수량 = Σ로트 + 장부 밖. HTS 와 콕핏을 맞춰 보는 화면
//
// ★ 계산만 한다. 조회(브로커·시세·원장)는 호출자가 해서 넘긴다 — 엔진이 계좌를 모른다는 경계
// (httpapi.Options.Account 주석)를 여기서도 지킨다.
package lots

import (
	"sort"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

type Lot struct {
	IntentID string          `json:"intent_id"`
	Group    string          `json:"group,omitempty"`
	Book     string          `json:"book,omitempty"` // 장부(전략) 이름. 장부에 없는 소스면 빈 값
	Kid      string          `json:"kid,omitempty"`
	Scope    string          `json:"scope,omitempty"`
	Slot     string          `json:"slot,omitempty"`
	Symbol   protocol.Symbol `json:"symbol"`

	Qty           float64    `json:"qty"`
	EntryQty      float64    `json:"entry_qty,omitempty"` // 처음 산 수량 (분할매도 전)
	AvgEntryPrice float64    `json:"avg_entry_price"`
	EntryAt       *time.Time `json:"entry_at,omitempty"`
	HeldSec       int64      `json:"held_sec,omitempty"`

	StopArmed  float64    `json:"stop_armed,omitempty"`
	TpArmed    float64    `json:"tp_armed,omitempty"`
	TpOrderID  string     `json:"tp_order_id,omitempty"`
	TimeExitAt *time.Time `json:"time_exit_at,omitempty"`

	// Price — 평가에 쓴 현재가. ★ 모르면 0 이고 평가손익도 비운다 — 0 원으로 평가하면 전액 손실로 보인다.
	Price         float64 `json:"price,omitempty"`
	CostKRW       float64 `json:"cost_krw"` // 지금 수량 × 진입가
	ValueKRW      float64 `json:"value_krw,omitempty"`
	UnrealizedKRW float64 `json:"unrealized_krw,omitempty"`
	UnrealizedPct float64 `json:"unrealized_pct,omitempty"`
	// RealizedKRW — 이 로트에서 분할매도로 이미 실현한 손익.
	RealizedKRW float64 `json:"realized_krw,omitempty"`
}

// Group — 같은 group 의 로트 합 (분할매수로 쌓은 한 포지션).
type Group struct {
	Group         string          `json:"group"`
	Book          string          `json:"book,omitempty"`
	Symbol        protocol.Symbol `json:"symbol"`
	Lots          int             `json:"lots"`
	Qty           float64         `json:"qty"`
	AvgEntryPrice float64         `json:"avg_entry_price"` // 로트 원가 가중 — 그룹을 한 포지션으로 볼 때의 평단
	CostKRW       float64         `json:"cost_krw"`
	UnrealizedKRW float64         `json:"unrealized_krw,omitempty"`
	RealizedKRW   float64         `json:"realized_krw,omitempty"`
	PriceUnknown  bool            `json:"price_unknown,omitempty"`
}

type Inputs struct {
	Now       time.Time
	Positions []protocol.Position
	// BookOf — (kid, scope) → 장부 이름. nil 이면 비운다.
	BookOf func(kid, scope string) string
	// Price — 현재가. nil 이거나 모르면 평가손익을 비운다.
	Price func(protocol.Symbol) (float64, bool)
	// Realized — intent_id → 분할매도로 이미 실현한 손익.
	Realized map[string]float64
}

// Build — 로트 목록 (전략·종목·진입 시각 순)과 group 합.
func Build(in Inputs) ([]Lot, []Group) {
	prices := map[protocol.Symbol]float64{} // 같은 종목을 로트마다 다시 조회하지 않는다
	priceOf := func(s protocol.Symbol) float64 {
		if p, ok := prices[s]; ok {
			return p
		}
		p := 0.0
		if in.Price != nil {
			if v, ok := in.Price(s); ok && v > 0 {
				p = v
			}
		}
		prices[s] = p
		return p
	}

	out := make([]Lot, 0, len(in.Positions))
	for _, p := range in.Positions {
		l := Lot{
			IntentID: p.IntentID, Group: p.Group, Kid: p.Kid, Scope: p.Scope, Slot: p.Slot, Symbol: p.Symbol,
			Qty: p.Qty, EntryQty: p.EntryQty, AvgEntryPrice: p.AvgEntryPrice, EntryAt: p.EntryAt,
			StopArmed: p.StopArmed, TpArmed: p.TpArmed, TpOrderID: p.TpOrderID, TimeExitAt: p.TimeExitAt,
			CostKRW: p.Qty * p.AvgEntryPrice, RealizedKRW: in.Realized[p.IntentID],
		}
		if in.BookOf != nil {
			l.Book = in.BookOf(p.Kid, p.Scope)
		}
		if p.EntryAt != nil && !in.Now.IsZero() {
			l.HeldSec = int64(in.Now.Sub(*p.EntryAt).Seconds())
		}
		if px := priceOf(p.Symbol); px > 0 {
			l.Price = px
			l.ValueKRW = p.Qty * px
			l.UnrealizedKRW = l.ValueKRW - l.CostKRW
			if l.CostKRW > 0 {
				l.UnrealizedPct = l.UnrealizedKRW / l.CostKRW
			}
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Book != b.Book {
			return a.Book < b.Book
		}
		if a.Symbol.Code != b.Symbol.Code {
			return a.Symbol.Code < b.Symbol.Code
		}
		return entryTime(a).Before(entryTime(b))
	})

	byGroup := map[string]*Group{}
	var order []string
	for _, l := range out {
		if l.Group == "" {
			continue
		}
		g, ok := byGroup[l.Group]
		if !ok {
			g = &Group{Group: l.Group, Book: l.Book, Symbol: l.Symbol}
			byGroup[l.Group] = g
			order = append(order, l.Group)
		}
		g.Lots++
		g.Qty += l.Qty
		g.CostKRW += l.CostKRW
		g.RealizedKRW += l.RealizedKRW
		if l.Price > 0 {
			g.UnrealizedKRW += l.UnrealizedKRW
		} else {
			g.PriceUnknown = true
		}
	}
	groups := make([]Group, 0, len(order))
	for _, k := range order {
		g := byGroup[k]
		if g.Qty > 0 {
			g.AvgEntryPrice = g.CostKRW / g.Qty
		}
		if g.PriceUnknown {
			g.UnrealizedKRW = 0 // 일부 로트 가격을 모르면 합계를 내지 않는다 — 반쪽 합계는 틀린 숫자다
		}
		groups = append(groups, *g)
	}
	return out, groups
}

func entryTime(l Lot) time.Time {
	if l.EntryAt == nil {
		return time.Time{}
	}
	return *l.EntryAt
}

// ── 종목별 대조 ──────────────────────────────────────────────────────────────

const (
	// StatusOK — 브로커 수량 = Σ로트.
	StatusOK = "ok"
	// StatusExternal — 브로커가 더 많다: 장부 밖 보유 (직접 산 것·다른 봇). 콕핏은 건드리지 않는다.
	StatusExternal = "external"
	// StatusShort — ★ 장부가 더 많다: 로트가 팔렸는데 콕핏이 모른다. 그 로트를 팔면 남의 주식을 판다.
	StatusShort = "short"
)

type LotRef struct {
	IntentID string  `json:"intent_id"`
	Book     string  `json:"book,omitempty"`
	Group    string  `json:"group,omitempty"`
	Qty      float64 `json:"qty"`
}

type Holding struct {
	Symbol    protocol.Symbol `json:"symbol"`
	BrokerQty float64         `json:"broker_qty"`
	BrokerAvg float64         `json:"broker_avg,omitempty"` // HTS 에 보이는 평단 (모든 로트 + 장부 밖 합산)
	LotsQty   float64         `json:"lots_qty"`
	// OffBook — 브로커 − Σ로트. + 면 장부 밖 보유, − 면 장부가 실물보다 많다.
	OffBook float64  `json:"off_book"`
	Status  string   `json:"status"`
	Lots    []LotRef `json:"lots,omitempty"`
}

// Reconcile — 종목별로 브로커 수량과 로트 합을 나란히 놓는다. lots 는 Build 결과(장부 이름 포함).
func Reconcile(lots []Lot, hs []broker.Holding, lotSize func(protocol.Symbol) float64) []Holding {
	by := map[protocol.Symbol]*Holding{}
	get := func(s protocol.Symbol) *Holding {
		h, ok := by[s]
		if !ok {
			h = &Holding{Symbol: s}
			by[s] = h
		}
		return h
	}
	for _, b := range hs {
		h := get(b.Symbol)
		h.BrokerQty += b.Qty
		h.BrokerAvg = b.AvgPrice
	}
	for _, l := range lots {
		h := get(l.Symbol)
		h.LotsQty += l.Qty
		h.Lots = append(h.Lots, LotRef{IntentID: l.IntentID, Book: l.Book, Group: l.Group, Qty: l.Qty})
	}

	out := make([]Holding, 0, len(by))
	for _, h := range by {
		eps := 1e-9
		if lotSize != nil {
			if ls := lotSize(h.Symbol); ls > 0 {
				eps = ls / 2
			}
		}
		h.OffBook = h.BrokerQty - h.LotsQty
		switch {
		case h.OffBook > eps:
			h.Status = StatusExternal
		case h.OffBook < -eps:
			h.Status = StatusShort
		default:
			h.Status, h.OffBook = StatusOK, 0
		}
		out = append(out, *h)
	}
	// 문제 있는 것부터 (short → external → ok), 그 안은 종목 코드 순.
	rank := map[string]int{StatusShort: 0, StatusExternal: 1, StatusOK: 2}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Status] != rank[out[j].Status] {
			return rank[out[i].Status] < rank[out[j].Status]
		}
		return out[i].Symbol.Code < out[j].Symbol.Code
	})
	return out
}
