package store

import (
	"context"
	"database/sql"
)

// ObservedCost — **실제 라이브 체결에서 도출한** 편도 비용 (bp).
//
// ★★ 왜 도출하나 — 수수료율은 사용자마다 다르다. 비대면 개설 이벤트, 등급 우대, 증권사별
// 정책… 어떤 상수를 코드에 박아도 그건 **남의 요율**이다. 제품이 되는 순간 더 그렇다.
// ⟹ 정답은 "좋은 기본값" 이 아니라 **자기 원장에서 재는 것**이다. 브로커가 체결 통지에
// 수수료를 실어 주므로(`fee_krw`), 라이브 체결이 한 건이라도 있으면 요율은 관측된다.
//
// ★★ **`live` 만 본다.** paper 의 `fee_krw` 는 *우리가 가정한 요율로 계산한 값*이라,
// 그걸로 요율을 도출하면 가정이 자기 자신을 확인하는 **순환**이 된다. 그런 숫자는 틀려도
// 언제나 일관돼 보여서 제일 늦게 발견된다.
type ObservedCost struct {
	BuyBp        float64 // Σfee / Σ(price*qty) × 10000
	SellBp       float64
	BuyFills     int
	SellFills    int
	BuyNotional  float64
	SellNotional float64
}

// HasBuy / HasSell — 관측이 실재하는가. ★ "0bp" 와 "관측 없음" 은 다른 사실이고,
// 합치면 수수료가 공짜인 것처럼 보인다.
func (o ObservedCost) HasBuy() bool  { return o.BuyFills > 0 && o.BuyNotional > 0 }
func (o ObservedCost) HasSell() bool { return o.SellFills > 0 && o.SellNotional > 0 }

// ObservedCost 는 라이브 원장에서 편도 비용을 도출한다. 체결이 없으면 빈 값(에러 아님).
func (s *Store) ObservedCost(ctx context.Context) (ObservedCost, error) {
	const q = `
SELECT side, COUNT(*), COALESCE(SUM(price * qty), 0), COALESCE(SUM(fee_krw), 0)
  FROM orders
 WHERE mode = 'live'
   AND phase IN ('filled', 'exit_filled')
   AND fee_krw IS NOT NULL
   AND price > 0 AND qty > 0
 GROUP BY side`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return ObservedCost{}, err
	}
	defer rows.Close()

	var out ObservedCost
	for rows.Next() {
		var side string
		var n int
		var notional, fee float64
		if err := rows.Scan(&side, &n, &notional, &fee); err != nil {
			return ObservedCost{}, err
		}
		bp := 0.0
		if notional > 0 {
			bp = fee / notional * 10000
		}
		switch side {
		case "buy":
			out.BuyBp, out.BuyFills, out.BuyNotional = bp, n, notional
		case "sell":
			out.SellBp, out.SellFills, out.SellNotional = bp, n, notional
		}
	}
	if err := rows.Err(); err != nil && err != sql.ErrNoRows {
		return ObservedCost{}, err
	}
	return out, nil
}
