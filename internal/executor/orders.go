package executor

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/ids"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// ── 진행 중 주문 (접수됐지만 아직 끝나지 않은 주문) ────────────────────────────────
//
// ★ 왜 (reflex 분석 2026-10-01 P0):
//   - 체결을 **기다리지 않는다** — 루프가 멈추면 다른 로트의 청산이 밀린다 (15:15 에 15 종목을 한꺼번에 판다).
//   - 동시호가·VI 중엔 체결이 늦게 오는 게 정상이다 — 청산 주문은 **시간으로 취소하지 않는다**.
//     (예전엔 20초 뒤 잔량을 취소해서, 장마감 동시호가에 내면 취소·재주문만 반복하다 장이 끝났다.)
//   - 접수 즉시 원장(working_orders)에 남긴다 — 기다리는 사이 데몬이 죽어도 재시작 뒤 이어서 추적한다.
//     (예전엔 체결 확인 뒤에야 남겨서, 그 사이 죽으면 주문번호를 잃고 같은 목표로 또 샀다.)

// defaultEntryFillTimeout — 진입 주문이 이 시간 안에 다 차지 않으면 잔량을 취소한다.
// ★ 60초 = reflex 실측(2026-08-13): 신호 뒤 60초 안 체결은 평균 +1.54%, 65초 넘으면 −4.09%. 늦은 체결은 다른 가격이다.
const defaultEntryFillTimeout = 60 * time.Second

// exitStuckAfter — 청산 주문이 이만큼 접속매매 중에 안 끝나면 사람에게 알린다 (취소는 안 한다).
const exitStuckAfter = 3 * time.Minute

func (x *Executor) entryTimeout() time.Duration {
	if x.d.EntryFillTimeout > 0 {
		return x.d.EntryFillTimeout
	}
	return defaultEntryFillTimeout
}

func (x *Executor) canExit(s protocol.Symbol, now time.Time) bool {
	return x.d.Session == nil || x.d.Session.CanExit(s.Exchange, now)
}

func (x *Executor) auction(s protocol.Symbol, now time.Time) bool {
	return x.d.Session != nil && x.d.Session.Of(s.Exchange, now).Auction()
}

func (x *Executor) isWorking(intentID string) bool {
	for _, w := range x.work {
		if w.IntentID == intentID {
			return true
		}
	}
	return false
}

// WorkingCount — 진행 중 주문 수 (하트비트용 — 집행 루프 고루틴에서만 부를 것).
func (x *Executor) WorkingCount() int { return len(x.work) }

// HasWorking — 진행 중 주문이 있는가 (있으면 데몬이 빠른 주기로 PollWorking 을 부른다).
func (x *Executor) HasWorking() bool { return len(x.work) > 0 }

// journal — ★ 보내기 **전에** 남긴다 (주문번호가 아직 없어 임시 번호로).
//
// 접수 응답을 받기 전에 데몬이 죽으면(전원·강제 종료·감시자의 재기동) 주문이 나갔는지 아무도 모른다.
// 이 기록이 없으면 재시작한 콕핏은 목표가 그대로라 **같은 목표로 또 산다** (2026-10-15 점검). 있으면
// 재시작 뒤 resolveUnconfirmed 가 "결과 미상" 으로 처리한다. ok=false 면 기록 실패 (진입은 내지 말 것).
func (x *Executor) journal(ctx context.Context, w store.WorkingOrder, res *Result) (string, bool) {
	w.OrderID = fmt.Sprintf("pre-%s-%s-%d", w.Purpose, w.IntentID, w.PlacedAt.UnixNano())
	w.Unconfirmed = true
	if err := x.d.Store.PutWorking(ctx, w); err != nil {
		res.fail("★ 주문 전 기록 실패 %s(%s) %s: %v — 진입이면 내지 않는다", w.IntentID, w.Symbol.Code, w.Purpose, err)
		return "", false
	}
	return w.OrderID, true
}

// unjournal — 보냈는데 접수되지 않았다 (거부, 또는 결과 미상 처리를 마쳤다). 임시 기록을 닫는다.
func (x *Executor) unjournal(ctx context.Context, temp string, now time.Time, res *Result) {
	if temp == "" {
		return
	}
	if err := x.d.Store.FinishWorking(ctx, temp, now); err != nil {
		res.fail("주문 전 기록 정리 %s: %v", temp, err)
	}
}

// track — 접수된 주문으로 임시 기록을 바꾸고 추적을 시작한다 (temp 가 비면 새로 남긴다).
func (x *Executor) track(ctx context.Context, temp string, w store.WorkingOrder, res *Result) {
	var err error
	if temp != "" {
		err = x.d.Store.ConfirmWorking(ctx, temp, w)
	} else {
		err = x.d.Store.PutWorking(ctx, w)
	}
	if err != nil {
		res.fail("★ 접수된 주문 기록 실패 %s(%s) 주문 %s: %v — 재시작하면 이 주문을 잃는다",
			w.IntentID, w.Symbol.Code, w.OrderID, err)
	}
	ww := w
	x.work[w.OrderID] = &ww
}

// pendingLot — 접수된 진입을 엔진에 **대기 로트**로 둔다: 같은 목표로 또 사지 않고, 예산도 먹는다.
func (x *Executor) pendingLot(w store.WorkingOrder) {
	p := protocol.Position{
		IntentID: w.IntentID, Kid: w.Kid, Scope: w.Scope, Symbol: w.Symbol,
		Qty: w.Qty, AvgEntryPrice: w.RefPrice, Pending: true,
	}
	if t := w.Target; t != nil {
		p.Slot, p.Group = t.Slot, t.Group
		if t.Exit != nil {
			p.StopArmed, p.TimeExitAt = t.Exit.StopPrice, t.Exit.TimeExitAt
		}
	}
	x.d.Engine.UpsertPosition(p)
}

// ensureLoaded — 재시작 뒤 첫 틱에 진행 중 주문을 원장에서 되살린다.
func (x *Executor) ensureLoaded(ctx context.Context, now time.Time, res *Result) {
	if x.loaded {
		return
	}
	ws, err := x.d.Store.OpenWorking(ctx)
	if err != nil {
		res.fail("진행 중 주문 복구 실패: %v", err)
		return
	}
	x.loaded = true
	for _, w := range ws {
		if w.Unconfirmed {
			x.resolveUnconfirmed(ctx, now, w, res)
			continue
		}
		ww := w
		x.work[w.OrderID] = &ww
		if w.Purpose == "entry" {
			x.pendingLot(w)
		}
	}
	if len(x.work) > 0 {
		x.d.Log.Warn("재시작 — 진행 중 주문을 이어서 추적한다", "orders", len(x.work))
	}
}

// resolveUnconfirmed — 보내기 전 기록만 있고 접수 기록이 없다 = 보내는 도중 죽었다. 나갔는지 모른다.
//
//   - 진입: 결과 미상 매수와 똑같이 처리한다 — 목표를 종결해 **다시 사지 않고**, 사람에게 알린다.
//     실제로 샀다면 그 주식은 장부 밖(external)으로 보인다. 두 번 사는 것보다 낫다.
//   - 청산: 로트는 그대로 두고 잠시 쉰 뒤 다시 판다. 그 사이 체결됐으면 보유가 줄어 콕핏 밖 매도로
//     귀속되고, 접수만 됐으면 수량이 잠겨 "매도가능 부족" 으로 드러난다 (sellFailed).
func (x *Executor) resolveUnconfirmed(ctx context.Context, now time.Time, w store.WorkingOrder, res *Result) {
	cause := fmt.Errorf("%w: 재시작 — %s 에 보내던 중 접수 응답 전에 멈췄다", broker.ErrMaybeSent,
		w.PlacedAt.Format(time.RFC3339))
	switch w.Purpose {
	case "entry":
		if w.Target == nil {
			res.fail("★ 보내던 진입 %s(%s) 에 목표가 없다 — 잔고를 사람이 확인할 것", w.IntentID, w.Symbol.Code)
			break
		}
		x.buyFailed(ctx, now, *w.Target, w.Qty, w.Kid, w.Scope, "", cause, res)
	case "exit":
		if pos, ok := x.position(w.IntentID); ok {
			x.sellFailed(ctx, now, pos, w.Reason, cause, res)
		}
	}
	x.unjournal(ctx, w.OrderID, now, res)
}

// PollWorking — 진행 중 주문을 전부 한 번씩 확인한다. 데몬이 짧은 주기로 부른다.
func (x *Executor) PollWorking(ctx context.Context, now time.Time) Result {
	var res Result
	x.ensureLoaded(ctx, now, &res)
	ids := make([]string, 0, len(x.work))
	for id := range x.work {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if w := x.work[id]; w != nil {
			x.pollOne(ctx, now, w, &res)
		}
	}
	return res
}

// pollOne — 주문 하나의 체결을 확인하고, 끝났으면 원장에 확정한다.
func (x *Executor) pollOne(ctx context.Context, now time.Time, w *store.WorkingOrder, res *Result) {
	if w == nil {
		return
	}
	lc, ok := x.d.Broker.(broker.LimitChecker)
	if !ok {
		return
	}
	st, err := lc.LimitStatus(ctx, w.Symbol, w.OrderID)
	if err != nil {
		return // 조회 실패는 "미체결" 이 아니다 — 다음에 다시 묻는다
	}
	lot := x.d.Broker.LotSize(w.Symbol)
	done := (st.Known && !st.Open) || st.FilledQty >= w.Qty-lot/2

	if !done {
		switch w.Purpose {
		case "entry":
			// 진입은 늦게 차면 다른 가격이다 — 마감을 넘으면 잔량을 취소하고 찬 만큼만 산다.
			// ★ 동시호가 중엔 취소하지 않는다(곧 단일가로 체결된다). 진입은 원래 접속매매에서만 낸다.
			if !w.Deadline.IsZero() && now.After(w.Deadline) && !x.auction(w.Symbol, now) {
				done = x.cancelAndRecheck(ctx, w, lc, &st, res)
			}
		case "exit":
			// ★ 청산은 시간으로 취소하지 않는다. 장이 끝났는데 남아 있으면(당일 주문 소멸) 취소하고
			// 찬 만큼만 확정한다 — 잔량은 로트에 남아 다음 장 08:59 에 다시 판다.
			if x.d.Session != nil && x.d.Session.Of(w.Symbol.Exchange, now) == session.Closed {
				done = x.cancelAndRecheck(ctx, w, lc, &st, res)
			} else if !x.auction(w.Symbol, now) && !w.PlacedAt.IsZero() && now.Sub(w.PlacedAt) > exitStuckAfter {
				res.fail("청산 주문 %s(%s) 이 %s 째 다 체결되지 않았다 (%v/%v) — 정지·VI 확인",
					w.IntentID, w.Symbol.Code, now.Sub(w.PlacedAt).Round(time.Second), st.FilledQty, w.Qty)
			}
		}
	}
	if !done {
		return
	}

	fill := fillFromStatus(*w, st, now, lot)
	switch w.Purpose {
	case "entry":
		x.finishWorkingEntry(ctx, now, *w, fill, res)
	case "exit":
		if pos, ok := x.position(w.IntentID); ok {
			x.finishSell(ctx, now, pos, w.Qty, w.Reason, fill, res)
		} else {
			res.fail("청산 체결 %s(%s) — 로트가 이미 없다 (주문 %s, %v주)", w.IntentID, w.Symbol.Code, w.OrderID, fill.Qty)
		}
	}
	if err := x.d.Store.FinishWorking(ctx, w.OrderID, now); err != nil {
		res.fail("진행 중 주문 종료 기록 %s: %v", w.OrderID, err)
	}
	delete(x.work, w.OrderID)
}

// cancelAndRecheck — 잔량을 취소하고 체결분을 다시 본다. 취소가 성공해야 "끝났다" 고 본다.
func (x *Executor) cancelAndRecheck(ctx context.Context, w *store.WorkingOrder, lc broker.LimitChecker,
	st *broker.LimitStatus, res *Result) bool {
	if err := x.d.Broker.CancelOrder(ctx, w.Symbol, w.OrderID); err != nil {
		// 이미 다 체결돼 취소할 게 없을 수도 있다 — 다시 보고, 다 찼으면 끝이다.
		if again, qerr := lc.LimitStatus(ctx, w.Symbol, w.OrderID); qerr == nil {
			*st = again
			if again.FilledQty >= w.Qty-x.d.Broker.LotSize(w.Symbol)/2 || (again.Known && !again.Open) {
				return true
			}
		}
		res.fail("잔량 취소 실패 %s(%s) 주문 %s: %v — 주문이 살아 있다", w.IntentID, w.Symbol.Code, w.OrderID, err)
		return false
	}
	w.CancelRequested = true
	if again, err := lc.LimitStatus(ctx, w.Symbol, w.OrderID); err == nil {
		*st = again
	}
	return true
}

func fillFromStatus(w store.WorkingOrder, st broker.LimitStatus, now time.Time, lot float64) broker.Fill {
	at := st.FilledAt
	if at.IsZero() {
		at = now
	}
	return broker.Fill{
		BrokerOrderID: w.OrderID, Qty: st.FilledQty, Price: st.AvgPrice,
		SubmittedAt: w.SubmittedAt, FilledAt: at, FeeKRW: st.FeeKRW,
		SlippageBp: broker.SlippageBp(w.Side, w.RefPrice, st.AvgPrice),
		Partial:    st.FilledQty < w.Qty-lot/2,
	}
}

// finishWorkingEntry — 진입 주문이 끝났다. 하나도 안 찼으면 진입 포기(목표 종결 — 늦게 다시 사지 않는다).
func (x *Executor) finishWorkingEntry(ctx context.Context, now time.Time, w store.WorkingOrder,
	fill broker.Fill, res *Result) {
	t := w.Target
	if t == nil {
		res.fail("진입 주문 %s 에 목표가 없다 — 복구 불가", w.OrderID)
		return
	}
	if fill.Qty > x.d.Broker.LotSize(w.Symbol)/2 {
		x.finishEnter(ctx, now, w.AsOfBar, *t, w.Kid, w.Scope, fill, res)
		return
	}
	x.recordFill(ctx, t.Slot, w.Kid, w.Scope, store.Order{
		ID: ids.NewAt(now), IntentID: t.IntentID, Phase: "canceled", Symbol: t.Symbol, Side: "buy",
		Qty: w.Qty, BrokerOrderID: w.OrderID, SubmittedAt: &w.SubmittedAt,
		Detail: "진입 주문이 체결 없이 끝났다 — 진입 포기(목표 종결)",
	}, res)
	if err := x.upsertIntent(ctx, store.Intent{
		IntentID: t.IntentID, Slot: t.Slot, Kid: w.Kid, Scope: w.Scope, Group: t.Group,
		Symbol: t.Symbol, Side: string(t.Side),
	}); err == nil {
		_ = x.d.Store.CloseIntent(ctx, t.IntentID, "unfilled", now)
	}
	x.d.Engine.MarkClosed(t.IntentID) // 대기 로트도 같이 지운다
	x.noteClose(protocol.Position{IntentID: t.IntentID, Kid: w.Kid, Scope: w.Scope, Group: t.Group, Symbol: t.Symbol},
		"unfilled", 0, now)
}

func (x *Executor) position(intentID string) (protocol.Position, bool) {
	for _, p := range x.d.Engine.Positions() {
		if p.IntentID == intentID {
			return p, true
		}
	}
	return protocol.Position{}, false
}
