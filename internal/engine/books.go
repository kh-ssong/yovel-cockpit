package engine

import (
	"context"
	"sort"

	"github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// BookStat — 활성화된 전략(장부) 하나의 성적. `/v1/books` 로 나간다.
//
// ★ 수익률의 분모는 **시드**다 (복리 아님). 도장 원장이 고정 크기(종목당 N원)로 재므로
// 같은 잣대여야 대조가 된다.
type BookStat struct {
	Name    string  `json:"name"`
	Kid     string  `json:"kid,omitempty"`
	Scope   string  `json:"scope"`
	Seed    float64 `json:"seed"`
	Enabled bool    `json:"enabled"`
	// Unbooked — 장부 설정에 없는 소스인데 포지션·거래가 있다 (설정 전에 산 것 등). 진입은 막혀 있다.
	Unbooked bool `json:"unbooked,omitempty"`

	Open     int     `json:"open"`
	OpenCost float64 `json:"open_cost"`

	Closed      int     `json:"closed"`
	Wins        int     `json:"wins"`
	RealizedKRW float64 `json:"realized_krw"`
	RealizedPct float64 `json:"realized_pct"` // realized_krw / seed
	// PriceUnknown — 체결가를 몰라 손익에서 뺀 종결 건수. 0 이 아니면 성적을 그대로 믿지 말 것.
	PriceUnknown int `json:"price_unknown,omitempty"`
}

type pnlStore interface {
	PnLByIntent(context.Context, protocol.Mode) ([]store.IntentPnL, error)
}

// BookStats — 장부별 성적. 원장에서 매번 다시 계산한다 (원장이 SSOT).
func (e *Engine) BookStats(ctx context.Context, mode protocol.Mode) ([]BookStat, error) {
	e.mu.Lock()
	open := make([]protocol.Position, 0, len(e.positions))
	for _, p := range e.positions {
		open = append(open, p)
	}
	e.mu.Unlock()

	books := e.cfg.Books
	stats := map[string]*BookStat{}
	get := func(kid, scope string) *BookStat {
		name, seed, active := books.Of(kid, scope)
		if books == nil {
			name, seed, active = book.Default, e.cfg.EngineBudget, true
		}
		key := name
		if key == "" {
			key = "?" + book.Key(kid, scope)
		}
		st, ok := stats[key]
		if !ok {
			st = &BookStat{Name: key, Kid: kid, Scope: scope, Seed: seed, Enabled: active, Unbooked: name == ""}
			stats[key] = st
		}
		return st
	}
	// 설정된 장부는 거래가 없어도 보인다 — "안 샀다" 도 성적이다.
	for _, b := range books.Books() {
		stats[b.Name] = &BookStat{Name: b.Name, Kid: b.Kid, Scope: b.Scope, Seed: b.Seed, Enabled: b.On()}
	}

	for _, p := range open {
		st := get(p.Kid, p.Scope)
		st.Open++
		st.OpenCost += p.Qty * p.AvgEntryPrice
	}

	if ps, ok := e.cfg.Store.(pnlStore); ok && e.cfg.Store != nil {
		rows, err := ps.PnLByIntent(ctx, mode)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if !r.Closed {
				// 열린 로트라도 분할매도로 판 몫은 이미 실현됐다.
				if r.SellQty > 0 && !r.PriceUnknown {
					get(r.Kid, r.Scope).RealizedKRW += r.Realized()
				}
				continue
			}
			st := get(r.Kid, r.Scope)
			if r.PriceUnknown {
				st.PriceUnknown++
				continue
			}
			st.Closed++
			pnl := r.Realized()
			st.RealizedKRW += pnl
			if pnl > 0 {
				st.Wins++
			}
		}
	}

	out := make([]BookStat, 0, len(stats))
	for _, st := range stats {
		if st.Seed > 0 {
			st.RealizedPct = st.RealizedKRW / st.Seed
		}
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// OpenRealized — 열린 로트별로 분할매도로 이미 실현한 손익 (intent_id → 원).
func (e *Engine) OpenRealized(ctx context.Context, mode protocol.Mode) (map[string]float64, error) {
	out := map[string]float64{}
	ps, ok := e.cfg.Store.(pnlStore)
	if !ok || e.cfg.Store == nil {
		return out, nil
	}
	rows, err := ps.PnLByIntent(ctx, mode)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if !r.Closed && r.SellQty > 0 && !r.PriceUnknown {
			out[r.IntentID] = r.Realized()
		}
	}
	return out, nil
}

// BookName — (kid, scope) 의 장부 이름. 장부 설정이 없으면 기본 장부, 장부에 없는 소스면 빈 값.
func (e *Engine) BookName(kid, scope string) string {
	if e.cfg.Books == nil {
		return book.Default
	}
	name, _, _ := e.cfg.Books.Of(kid, scope)
	return name
}
