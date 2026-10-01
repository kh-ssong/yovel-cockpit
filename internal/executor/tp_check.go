package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/ids"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
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
		if p.TpOrderID == "" || x.isWorking(p.IntentID) {
			continue
		}
		if last, seen := x.tpChecked[p.IntentID]; seen && now.Sub(last) < tpCheckEvery {
			continue
		}
		x.tpChecked[p.IntentID] = now
		x.closeIfTPFilled(ctx, now, p, lc, res)
	}
}

// closeIfTPFilled — 이 로트의 TP 가 다 체결됐으면 체결가와 함께 닫고 true.
// ★ 조회 실패·미체결·부분은 false (아무것도 안 한다).
func (x *Executor) closeIfTPFilled(ctx context.Context, now time.Time, p protocol.Position,
	lc broker.LimitChecker, res *Result) bool {
	st, err := lc.LimitStatus(ctx, p.Symbol, p.TpOrderID)
	if err != nil || st.FilledQty < p.Qty-x.d.Broker.LotSize(p.Symbol)/2 {
		return false
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
		return true // 원장엔 적었다 — 사후 감지로 한 번 더 적지 않게 true
	}
	x.d.Engine.MarkClosed(p.IntentID)
	x.noteClose(p, "tp", st.AvgPrice, now)
	delete(x.tpChecked, p.IntentID)
	res.Exited++
	x.d.Log.Info("TP 체결 (주문번호 확인)", "intent_id", p.IntentID, "code", p.Symbol.Code,
		"price", st.AvgPrice, "realized_pct", fmt.Sprintf("%.2f%%", realizedPct(p.AvgEntryPrice, st.AvgPrice)*100))
	return true
}
