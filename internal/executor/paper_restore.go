package executor

import (
	"context"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/paper"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// PaperState — paper 계좌를 원장에서 다시 세운다 (재시작 직후, 첫 틱 전에 paper.Broker.Restore 로).
//
// ★ paper 는 원장이 진실이다 (실계좌는 브로커가 진실이고 원장이 기억이지만, paper 의 "브로커" 는
// 메모리일 뿐이다). 그래서:
//   - 예수금 = 시드 + paper 원장의 현금 흐름 합 (매도 대금 − 매수 대금 − 수수료)
//   - 보유   = 열린 로트의 합 (종목별 수량, 원가 가중 평단)
//   - 지정가 = 열린 로트에 걸려 있던 TP (같은 주문번호 — 원장이 그 번호로 체결을 확인한다)
//
// 엔진 복구(모드 대조)를 **먼저** 통과한 뒤 부를 것 — 열린 로트가 전부 paper 라는 전제다.
func PaperState(ctx context.Context, st *store.Store, seed float64) (paper.State, error) {
	open, err := st.OpenIntents(ctx)
	if err != nil {
		return paper.State{}, err
	}
	net, seq, err := st.PaperCashFlow(ctx)
	if err != nil {
		return paper.State{}, err
	}
	out := paper.State{Cash: seed + net, Seq: seq}
	by := map[protocol.Symbol]*broker.Holding{}
	var order []protocol.Symbol
	for _, p := range open {
		if p.Qty <= 0 {
			continue
		}
		h, ok := by[p.Symbol]
		if !ok {
			h = &broker.Holding{Symbol: p.Symbol}
			by[p.Symbol] = h
			order = append(order, p.Symbol)
		}
		cost := h.AvgPrice*h.Qty + p.AvgEntryPrice*p.Qty
		h.Qty += p.Qty
		h.AvgPrice = cost / h.Qty
		if p.TpOrderID != "" && p.TpArmed > 0 {
			out.TPs = append(out.TPs, paper.TP{ID: p.TpOrderID, Symbol: p.Symbol, Qty: p.Qty, Price: p.TpArmed})
		}
	}
	for _, s := range order {
		out.Holdings = append(out.Holdings, *by[s])
	}
	return out, nil
}
