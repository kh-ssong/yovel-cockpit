package upbit

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// SellFills — 이 마켓의 최근 종결 매도 주문 (GET /orders/closed).
// ★ 시각·상태 배열 파라미터는 안 보낸다 — `:`·`+`·`[]` 가 JWT 해시와 전송에서 다르게 인코딩돼 401 이 난다
// (reflex 2026-07-18). 최근 100건을 받아 여기서 거른다.
func (b *Broker) SellFills(ctx context.Context, s protocol.Symbol) ([]broker.ExecFill, error) {
	m, err := market(s)
	if err != nil {
		return nil, err
	}
	var os []struct {
		UUID           string `json:"uuid"`
		Side           string `json:"side"`
		ExecutedVolume fnum   `json:"executed_volume"`
		ExecutedFunds  fnum   `json:"executed_funds"`
		PaidFee        fnum   `json:"paid_fee"`
		CreatedAt      string `json:"created_at"`
	}
	if err := b.do(ctx, http.MethodGet, "/orders/closed",
		url.Values{"market": {m}, "limit": {"100"}, "order_by": {"desc"}}, &os); err != nil {
		return nil, err
	}
	var out []broker.ExecFill
	for _, o := range os {
		if o.Side != "ask" || o.ExecutedVolume <= 0 {
			continue
		}
		f := broker.ExecFill{OrderID: o.UUID, Qty: float64(o.ExecutedVolume), FeeKRW: float64(o.PaidFee)}
		if o.ExecutedFunds > 0 {
			f.Price = float64(o.ExecutedFunds) / f.Qty
		}
		if t, err := time.Parse(time.RFC3339, o.CreatedAt); err == nil {
			f.At = t.UTC()
		}
		out = append(out, f)
	}
	return out, nil
}
