package kiwoom

import (
	"context"
	"encoding/json"
	"math"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// SellFills — 이 종목의 오늘 매도 체결 (ka10076, 매도만). 주문번호 단위로 합친다.
// ★ ka10076 은 금일 체결만 준다 — 전날 매도는 안 보인다.
func (b *Broker) SellFills(ctx context.Context, s protocol.Symbol) ([]broker.ExecFill, error) {
	body := map[string]string{"stk_cd": s.Code, "qry_tp": "1", "sell_tp": "1", "ord_no": "", "stex_tp": "0"}
	var raw map[string]json.RawMessage
	if err := b.call(ctx, apiFills, pathAcnt, body, &raw); err != nil {
		return nil, err
	}
	byOrd := map[string]*broker.ExecFill{}
	var order []string
	for _, row := range pickRows(raw, "cntr_qty") {
		var r struct {
			OrdNo    string `json:"ord_no"`
			CntrQty  string `json:"cntr_qty"`
			CntrPric string `json:"cntr_pric"`
			Cmsn     string `json:"tdy_trde_cmsn"`
			Tax      string `json:"tdy_trde_tax"`
			OrdTm    string `json:"ord_tm"`
		}
		if json.Unmarshal(row, &r) != nil {
			continue
		}
		q := math.Abs(num(r.CntrQty))
		if q <= 0 {
			continue
		}
		f := byOrd[r.OrdNo]
		if f == nil {
			f = &broker.ExecFill{OrderID: r.OrdNo}
			byOrd[r.OrdNo] = f
			order = append(order, r.OrdNo)
		}
		amt := f.Price*f.Qty + q*math.Abs(num(r.CntrPric))
		f.Qty += q
		f.Price = amt / f.Qty
		f.FeeKRW += num(r.Cmsn) + num(r.Tax)
		if t, ok := parseOrdTime(r.OrdTm, b.now()); ok && t.After(f.At) {
			f.At = t
		}
	}
	out := make([]broker.ExecFill, 0, len(order))
	for _, id := range order {
		out = append(out, *byOrd[id])
	}
	return out, nil
}
