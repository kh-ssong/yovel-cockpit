package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/ids"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// expireDayTPs — ★ KRX 지정가(TP)는 당일 주문이라 장이 끝나면 거래소에서 사라진다. 콕핏이 그걸 모르면
// 날 넘겨 드는 로트(스윙)에서 두 가지가 틀어진다 (2026-10-17 flat6 스윙 질의에서 드러남):
//   - 둘째 날부터 TP 가 없는데 TpOrderID 가 남아 있어 계획이 «이미 걸려 있다» 로 보고 다시 걸지 않는다.
//   - 나중의 청산이 죽은 주문부터 취소하려다 거부당해(키움 800033) 매도를 못 낸다 — doSell 은 TP 취소가
//     실패하면 멈춘다.
//
// 그래서 장이 끝난 뒤(Closed) KRX 로트의 TP 를 정산하고 지운다. 금일 체결은 그날 안에만 물을 수 있다(ka10076):
//   - 다 찼으면 tp 로 종결 (낮의 checkDelegatedTPs 와 같은 경로)
//   - 일부 찼으면 찬 만큼 적고 로트를 줄인다 (남은 수량은 계속 든다)
//   - 안 찼으면 지우기만 한다
//
// 다음 장 접속매매에 계획이 남은 수량으로 다시 건다. TP 는 접속매매에서만 걸리므로 장 밖에 남은 TpOrderID 는
// 늘 지난 장의 것이다. ★ 업비트 지정가는 기한이 없어(GTC) 건드리지 않는다.
func (x *Executor) expireDayTPs(ctx context.Context, now time.Time, res *Result) {
	if x.d.Session == nil || x.d.Session.KRX(now) != session.Closed {
		return
	}
	lc, _ := x.d.Broker.(broker.LimitChecker)
	_, selfHeld := x.d.Broker.(limitSettler)
	for _, p := range x.d.Engine.Positions() {
		if p.Symbol.Exchange != "KRX" || p.TpOrderID == "" || p.Pending || x.isWorking(p.IntentID) {
			continue
		}
		if lc != nil {
			st, err := lc.LimitStatus(ctx, p.Symbol, p.TpOrderID)
			if err != nil {
				// ★ 조회 실패를 "미체결" 로 읽지 않는다 — 지우고 나면 그날 체결을 다시는 못 센다.
				res.fail("지난 장 TP 정산 %s(%s): %v — 1분 뒤 다시", p.IntentID, p.Symbol.Code, err)
				continue
			}
			lot := x.d.Broker.LotSize(p.Symbol)
			if st.FilledQty >= p.Qty-lot/2 {
				x.closeTPFill(ctx, now, p, st, res)
				continue
			}
			if st.FilledQty > lot/2 {
				x.reduceByDayTP(ctx, now, p, st, res)
				continue
			}
		}
		if selfHeld {
			// paper 는 지정가를 자기가 들고 있다 — 거래소처럼 지워 줘야 잡혀 있던 수량이 풀린다.
			_ = x.d.Broker.CancelOrder(ctx, p.Symbol, p.TpOrderID)
		}
		if err := x.d.Store.ClearTP(ctx, p.IntentID); err != nil {
			res.fail("지난 장 TP 지우기 %s: %v", p.IntentID, err)
			continue
		}
		p.TpOrderID, p.TpArmed = "", 0
		x.d.Engine.UpsertPosition(p)
		x.d.Log.Info("지난 장 TP 소멸 — 다음 장에 다시 건다", "intent_id", p.IntentID, "code", p.Symbol.Code)
	}
}

// reduceByDayTP — 지난 장 TP 가 일부만 찼다. 찬 만큼 tp 로 적고 로트를 남은 수량으로 줄인다
// (ReduceIntent 가 TP 도 지운다). ★ 행 id 는 주문번호에서 파생한다 — 줄이기가 실패해 다음 틱이 다시 와도 한 행이다.
func (x *Executor) reduceByDayTP(ctx context.Context, now time.Time, p protocol.Position, st broker.LimitStatus,
	res *Result) {
	remaining := p.Qty - st.FilledQty
	filledAt := st.FilledAt
	if filledAt.IsZero() {
		filledAt = now
	}
	x.recordFill(ctx, p.Slot, p.Kid, p.Scope, store.Order{
		ID: ids.Derive(filledAt, "daytp:"+p.TpOrderID), IntentID: p.IntentID, Phase: "exit_filled",
		Symbol: p.Symbol, Side: "sell", Qty: st.FilledQty, Price: st.AvgPrice,
		BrokerOrderID: p.TpOrderID, FilledAt: &filledAt, FeeKRW: st.FeeKRW,
		ExitReason: "tp", RealizedPct: realizedPct(p.AvgEntryPrice, st.AvgPrice),
		Source: store.SourceBot,
		Detail: fmt.Sprintf("TP 일부 체결 — 당일 지정가 소멸, 잔량 %v 은 계속 든다 (다음 장에 TP 다시)", remaining),
	}, res)
	if err := x.d.Store.ReduceIntent(ctx, p.IntentID, remaining); err != nil {
		res.fail("★ TP 일부 체결 잔량 기록 실패 %s: %v", p.IntentID, err)
		return
	}
	p.Qty = remaining
	p.TpOrderID, p.TpArmed = "", 0
	x.d.Engine.UpsertPosition(p)
	res.Reduced++
	x.d.Log.Info("지난 장 TP 일부 체결 — 로트를 줄였다", "intent_id", p.IntentID, "code", p.Symbol.Code,
		"sold", st.FilledQty, "remaining", remaining)
}
