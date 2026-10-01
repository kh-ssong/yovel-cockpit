package store

import (
	"context"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// IntentPnL — 목표 하나의 돈 흐름. 장부별 성적(/v1/books)의 재료다.
//
// ★ 원장(orders)에서 **다시 계산한다.** 따로 누계를 쌓으면 그 누계와 원장이 갈리는 날이 오고,
// 그때 어느 쪽이 맞는지 아무도 모른다. 원장이 SSOT 다.
type IntentPnL struct {
	IntentID string
	Slot     string
	Kid      string
	Scope    string
	Closed   bool
	// Cost — 매수 체결금액 + 매수 수수료. Proceeds — 매도 체결금액 − 매도 수수료.
	Cost     float64
	Proceeds float64
	BuyQty   float64
	SellQty  float64
	// PriceUnknown — 체결가를 모르는 매도가 섞였다 (브로커 사후 감지). 이 건의 손익은 믿을 수 없다.
	PriceUnknown bool
}

// Realized — 실현손익. 종결된 로트는 전부, 열린 로트는 **이미 판 몫만**(분할매도).
// 판 몫의 원가 = 매수 원가 × (판 수량 / 산 수량) — 로트 안은 한 평단이다.
func (p IntentPnL) Realized() float64 {
	if p.PriceUnknown {
		return 0
	}
	if p.Closed {
		return p.Proceeds - p.Cost
	}
	if p.SellQty <= 0 || p.BuyQty <= 0 {
		return 0
	}
	return p.Proceeds - p.Cost*p.SellQty/p.BuyQty
}

// PnLByIntent — 한 mode 의 목표별 돈 흐름. ★ mode 는 필수다 (paper 와 live 를 합산하지 않는다).
func (s *Store) PnLByIntent(ctx context.Context, mode protocol.Mode) ([]IntentPnL, error) {
	if mode != protocol.ModePaper && mode != protocol.ModeLive {
		return nil, ErrModeRequired
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT i.intent_id, COALESCE(i.slot, ''), i.kid, i.scope, i.closed_at IS NOT NULL,
       o.side, o.qty, o.price, COALESCE(o.fee_krw, 0)
FROM orders o JOIN intents i ON i.intent_id = o.intent_id
WHERE o.mode = ? AND o.phase IN ('filled', 'exit_filled')
ORDER BY i.intent_id`, string(mode))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []IntentPnL
	idx := map[string]int{}
	for rows.Next() {
		var id, slot, kid, scope, side string
		var closed bool
		var qty, price, fee float64
		if err := rows.Scan(&id, &slot, &kid, &scope, &closed, &side, &qty, &price, &fee); err != nil {
			return nil, err
		}
		i, ok := idx[id]
		if !ok {
			i = len(out)
			idx[id] = i
			out = append(out, IntentPnL{IntentID: id, Slot: slot, Kid: kid, Scope: scope, Closed: closed})
		}
		p := &out[i]
		switch side {
		case "buy":
			p.BuyQty += qty
			p.Cost += qty*price + fee
		case "sell":
			p.SellQty += qty
			if price <= 0 {
				p.PriceUnknown = true
			}
			p.Proceeds += qty*price - fee
		}
	}
	return out, rows.Err()
}

// DayRealized — since 이후 확정된 청산의 실현손익 (원) 과 손실 건수. 일일 손실 한도가 쓴다.
//
// 손익 = Σ 판 수량 × (체결가 − 로트 진입가) − 매도 수수료·세금. 매수 수수료는 넣지 않는다(근사 — 작다).
// ★ 체결가를 모르는 청산(사후 감지·수동)은 뺀다 — 모르는 걸 0원 손익으로 세면 한도가 틀린다.
func (s *Store) DayRealized(ctx context.Context, mode protocol.Mode, since time.Time) (krw float64, losses int, err error) {
	if mode != protocol.ModePaper && mode != protocol.ModeLive {
		return 0, 0, ErrModeRequired
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT o.qty, o.price, COALESCE(o.fee_krw, 0), i.avg_entry_price
FROM orders o JOIN intents i ON i.intent_id = o.intent_id
WHERE o.mode = ? AND o.phase = 'exit_filled' AND o.price > 0 AND o.created_at >= ?`,
		string(mode), ts(since))
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var q, px, fee, avg float64
		if err := rows.Scan(&q, &px, &fee, &avg); err != nil {
			return 0, 0, err
		}
		pnl := q*(px-avg) - fee
		krw += pnl
		if pnl < 0 {
			losses++
		}
	}
	return krw, losses, rows.Err()
}
