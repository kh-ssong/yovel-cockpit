package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/ids"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// tpCheckEvery — 한 로트의 TP 를 다시 물어보는 간격.
const tpCheckEvery = 10 * time.Second

// checkDelegatedTPs — 걸어 둔 TP 가 체결됐는지 **주문번호로** 묻고, 다 체결된 로트를 체결가와 함께 닫는다.
//
// ★ 부분 체결은 기다린다 — TP 지정가는 남은 수량이 계속 걸려 있다. 다 채워지면 그때 한 번에 닫는다.
// ★ 조회 실패는 "미체결" 이 아니다 — 아무것도 하지 않고 다음에 다시 묻는다.
func (x *Executor) checkDelegatedTPs(ctx context.Context, now time.Time, res *Result) {
	lc, ok := x.d.Broker.(broker.LimitChecker)
	if !ok {
		return
	}
	for _, p := range x.d.Engine.Positions() {
		if p.TpOrderID == "" {
			continue
		}
		if last, seen := x.tpChecked[p.IntentID]; seen && now.Sub(last) < tpCheckEvery {
			continue
		}
		x.tpChecked[p.IntentID] = now

		st, err := lc.LimitStatus(ctx, p.Symbol, p.TpOrderID)
		if err != nil {
			continue
		}
		if st.FilledQty < p.Qty-x.d.Broker.LotSize(p.Symbol)/2 {
			continue
		}

		filledAt := st.FilledAt
		if filledAt.IsZero() {
			filledAt = now
		}
		x.recordFill(ctx, p.Slot, p.Kid, p.Scope, store.Order{
			ID: ids.NewAt(now), IntentID: p.IntentID, Phase: "exit_filled",
			Symbol: p.Symbol, Side: "sell", Qty: st.FilledQty, Price: st.AvgPrice,
			BrokerOrderID: p.TpOrderID, FilledAt: &filledAt, FeeKRW: st.FeeKRW,
			ExitReason: "tp", RealizedPct: realizedPct(p.AvgEntryPrice, st.AvgPrice),
			Source: store.SourceBot,
		}, res)
		if err := x.d.Store.CloseIntent(ctx, p.IntentID, "tp", now); err != nil {
			res.fail("TP 종결 %s: %v", p.IntentID, err)
			continue
		}
		x.d.Engine.MarkClosed(p.IntentID)
		x.noteClose(p, "tp", st.AvgPrice, now)
		delete(x.tpChecked, p.IntentID)
		res.Exited++
		x.d.Log.Info("TP 체결 (주문번호 확인)", "intent_id", p.IntentID, "code", p.Symbol.Code,
			"price", st.AvgPrice, "realized_pct", fmt.Sprintf("%.2f%%", realizedPct(p.AvgEntryPrice, st.AvgPrice)*100))
	}
}
