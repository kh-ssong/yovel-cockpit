package engine

import (
	"context"
	"sort"

	"github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// BookStat — 전략(장부) 하나의 성적. `/v1/books` 로 나간다.
//
// ★ 수익률의 분모는 **시드**다 (복리 아님). 도장 원장이 고정 크기(종목당 N원)로 재므로
// 같은 잣대여야 대조가 된다.
type BookStat struct {
	Name  string   `json:"name"`
	Seed  float64  `json:"seed"`
	Slots []string `json:"slots,omitempty"`

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
	get := func(slot string) *BookStat {
		name, seed := books.Of(slot)
		if books == nil {
			name, seed = book.Default, e.cfg.EngineBudget
		}
		st, ok := stats[name]
		if !ok {
			st = &BookStat{Name: name, Seed: seed}
			stats[name] = st
		}
		return st
	}
	// 설정된 장부는 거래가 없어도 보인다 — "안 샀다" 도 성적이다.
	for _, b := range books.Books() {
		stats[b.Name] = &BookStat{Name: b.Name, Seed: b.Seed, Slots: b.Slots}
	}

	for _, p := range open {
		st := get(p.Slot)
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
				continue
			}
			st := get(r.Slot)
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
