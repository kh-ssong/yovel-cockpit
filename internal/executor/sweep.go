package executor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/ids"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// ── 집행 방식 sweep (2026-11-04) ─────────────────────────────────────────────────
//
// 진입: 창(entry.sweep_until) 안에서 매도1 ≤ cap_price 가 보일 때마다 잔량에 IOC 지정가(cap)를 내 조금씩 담고,
// 창이 끝나면 잔량을 시장가로 산다 (max_slip_bp 를 넘으면 잔량 포기). 청산(want=flat + exit_exec)은 거울 —
// 매수1 ≥ floor_price 면 IOC 지정가(floor) 매도, 창 끝 잔량 시장가.
// 근거 = dojo-etf CAPSWEEP (069500 T2 바탕 −1.0bp/건 → 건당 ≈ +4bp 후보 · 검증은 실전 체결로만).
//
// ★ 규칙
//   - 조각은 한 번에 하나만 나간다. 앞 조각의 체결을 모르는 채 다음을 내면 더 산다(판다).
//   - 호가에 남지 않는다 (IOC). 호가는 1초에 한 번만 묻는다 — 같은 호가에 IOC 를 거듭 내지 않게, 조회 한도도.
//   - 창 끝 잔량 시장가는 필수다 — 포기형은 역선택이다 (안 돌아오는 소수가 바로 가는 승자).
//   - 청산 sweep 은 stop·시간청산을 기다리지 않는다 — 매수1 이 stop 에 닿거나 시간청산 시각이면 잔량을 바로 시장가로.
//   - 부모 주문(working_orders "sweep-…")에 진행 상태를 남긴다. 조각을 내기 **전에** «보내는 중» 을 적고,
//     재시작했는데 그게 남아 있으면 그 조각은 결과 미상이다 (sweepUnknown — 더 내지 않고 사람에게 알린다).
//   - 원장엔 진입·청산 한 줄 (수량·평단 = 조각 합) + exec (조각 내역). 증권사 원장 대조는 exec 의 조각 번호로 한다.

const (
	// sweepBookEvery — 호가 조회 간격 (= 같은 호가에 IOC 를 다시 내기까지의 최소 간격).
	sweepBookEvery = time.Second
	// iocSettle — IOC 는 즉시 끝난다. 이만큼 지나도 «살아 있다» 로 보이면 취소를 한 번 보내 확인하고
	// 그때의 체결 수량을 확정으로 본다 (키움 ka10076 이 IOC 잔량 소멸을 늦게 싣는 경우).
	iocSettle = 2 * time.Second
	// iocUnseen — 체결 조회에 아예 안 나오는 IOC 를 0 체결로 보기까지 (조회 반영이 늦을 수 있다).
	iocUnseen = 5 * time.Second
	// iocMaxErrors — IOC 가 연달아 이만큼 거부되면 창을 일찍 닫고 잔량 시장가로 간다.
	iocMaxErrors = 3
)

func (x *Executor) bookReader() (broker.BookReader, bool) {
	b, ok := x.d.Broker.(broker.BookReader)
	return b, ok
}

// sweepUntil — 창 끝을 상한(SweepMaxWindow)으로 당긴다.
func sweepUntil(now, want time.Time) time.Time {
	if max := now.Add(protocol.SweepMaxWindow); want.After(max) {
		return max
	}
	return want
}

// startEntrySweep — 진입을 sweep 으로 시작한다. false = 이 브로커는 sweep 을 못 한다 (호출자가 시장가로 낸다).
func (x *Executor) startEntrySweep(ctx context.Context, now, asOfBar time.Time, t protocol.Target,
	qty, ref float64, kid, scope string, res *Result) bool {
	if _, ok := x.bookReader(); !ok || t.Entry == nil || t.Entry.SweepUntil == nil {
		x.d.Log.Warn("이 브로커는 sweep 을 못 한다 — 시장가로 낸다", "intent_id", t.IntentID,
			"code", t.Symbol.Code, "broker", x.d.Broker.Name())
		return false
	}
	tt := t
	w := store.WorkingOrder{
		OrderID:  fmt.Sprintf("sweep-entry-%s-%d", t.IntentID, now.UnixNano()),
		IntentID: t.IntentID, Purpose: "entry", Side: "buy", Symbol: t.Symbol,
		Qty: qty, RefPrice: ref, Kid: kid, Scope: scope, Target: &tt, AsOfBar: asOfBar, PlacedAt: now,
		Sweep: &store.Sweep{Limit: t.Entry.CapPrice, Until: sweepUntil(now, *t.Entry.SweepUntil),
			MaxSlipBp: t.Entry.MaxSlipBp},
	}
	// ★ 기록 없이는 사지 않는다 — 조각을 내는 도중 죽으면 재시작 뒤 같은 목표로 또 산다.
	if err := x.d.Store.PutWorking(ctx, w); err != nil {
		res.fail("★ sweep 진입 기록 실패 %s(%s): %v — 내지 않는다", t.IntentID, t.Symbol.Code, err)
		return true
	}
	ww := w
	x.work[w.OrderID] = &ww
	x.pendingLot(ww)
	x.d.Log.Info("sweep 진입 시작", "intent_id", t.IntentID, "code", t.Symbol.Code, "qty", qty,
		"cap", w.Sweep.Limit, "until", w.Sweep.Until, "max_slip_bp", w.Sweep.MaxSlipBp)
	x.stepSweep(ctx, now, &ww, res)
	return true
}

// wantsExitSweep — 이 청산을 sweep 으로 낼까. 로트 전량 flat 이고, 창이 아직 남았고, 접속매매 중일 때만.
// ★ 그 밖(분할매도·derisk·stop·시간청산·동시호가·창 지남)은 예전처럼 바로 시장가다.
func (x *Executor) wantsExitSweep(now time.Time, pos protocol.Position, qty float64, reason string,
	ex *protocol.ExitExec) bool {
	if ex == nil || ex.Mode != protocol.ExecSweep || reason != "flat" || ex.FloorPrice <= 0 {
		return false
	}
	if qty < pos.Qty-x.d.Broker.LotSize(pos.Symbol)/2 || !now.Before(ex.SweepUntil) {
		return false
	}
	if x.d.Session != nil && !x.d.Session.CanEnter(pos.Symbol.Exchange, now) {
		return false // IOC 는 접속매매에서만 — 동시호가엔 바로 시장가(단일가)로
	}
	if te := x.effectiveTimeExit(pos); te != nil && !now.Before(*te) {
		return false
	}
	_, ok := x.bookReader()
	return ok
}

// startExitSweep — want=flat 청산을 sweep 으로 시작한다. false = 기록 실패 (호출자가 시장가로 낸다).
func (x *Executor) startExitSweep(ctx context.Context, now time.Time, pos protocol.Position, qty float64,
	reason string, ex protocol.ExitExec, res *Result) bool {
	w := store.WorkingOrder{
		OrderID:  fmt.Sprintf("sweep-exit-%s-%d", pos.IntentID, now.UnixNano()),
		IntentID: pos.IntentID, Purpose: "exit", Reason: reason, Side: "sell", Symbol: pos.Symbol,
		Qty: qty, RefPrice: ex.FloorPrice, Kid: pos.Kid, Scope: pos.Scope, PlacedAt: now,
		Sweep: &store.Sweep{Limit: ex.FloorPrice, Until: sweepUntil(now, ex.SweepUntil)},
	}
	if err := x.d.Store.PutWorking(ctx, w); err != nil {
		// 청산은 미루지 않는다 — 기록을 못 남기면 sweep 을 포기하고 시장가로.
		res.fail("sweep 청산 기록 실패 %s(%s): %v — 시장가로 판다", pos.IntentID, pos.Symbol.Code, err)
		return false
	}
	ww := w
	x.work[w.OrderID] = &ww
	x.d.Log.Info("sweep 청산 시작", "intent_id", pos.IntentID, "code", pos.Symbol.Code, "qty", qty,
		"floor", ex.FloorPrice, "until", w.Sweep.Until)
	x.stepSweep(ctx, now, &ww, res)
	return true
}

// stepSweep — sweep 한 걸음: 나가 있는 조각 확인 → 끝났으면 마무리 → 창이 닫혔으면 잔량 → 호가가 닿으면 IOC.
// 진행 중 주문 확인(PollWorking, 0.5초)이 부른다.
func (x *Executor) stepSweep(ctx context.Context, now time.Time, w *store.WorkingOrder, res *Result) {
	sw := w.Sweep
	lot := x.d.Broker.LotSize(w.Symbol)
	if c := sw.Child; c != nil {
		if c.Unconfirmed {
			x.sweepUnknown(ctx, now, w, fmt.Errorf("%w: 재시작 — 조각(%s %v) 을 보내던 중 접수 응답 전에 멈췄다",
				broker.ErrMaybeSent, c.Kind, c.Qty), res)
			return
		}
		if !x.settleChild(ctx, now, w, res) {
			return // 아직 결과를 모른다 — 다음에 다시
		}
	}
	filled, _, _ := sw.Filled()
	remaining := w.Qty - filled
	if remaining <= lot/2 {
		if sw.Ended == "" {
			sw.Ended = "all"
		}
		x.finishSweep(ctx, now, w, res)
		return
	}
	if sw.RestDone {
		x.finishSweep(ctx, now, w, res)
		return
	}
	if sw.Ended == "" {
		sw.Ended = x.sweepEndReason(now, w)
	}
	if sw.Ended != "" {
		x.placeRest(ctx, now, w, remaining, res)
		return
	}

	if now.Before(sw.NextBook) {
		return
	}
	sw.NextBook = now.Add(sweepBookEvery)
	br, _ := x.bookReader()
	bk, err := br.Book(ctx, w.Symbol)
	if err != nil {
		return // 호가를 모르면 이번엔 쉰다 — 창 끝에 잔량 시장가 (모름을 체결 사유로 쓰지 않는다)
	}
	if sw.RefAt.IsZero() {
		sw.RefAsk1, sw.RefBid1, sw.RefAt = bk.Ask1, bk.Bid1, now
		x.saveSweep(ctx, w, res)
	}
	px, depth := bk.Ask1, bk.Ask1Qty
	cross := px > 0 && px <= sw.Limit+1e-9
	if w.Side == "sell" {
		px, depth = bk.Bid1, bk.Bid1Qty
		cross = px > 0 && px >= sw.Limit-1e-9
		// ★ stop 은 기다리지 않는다 — 매수1 이 stop 에 닿으면 잔량을 바로 시장가로.
		if pos, ok := x.position(w.IntentID); ok && pos.StopArmed > 0 && px > 0 && px <= pos.StopArmed {
			sw.Ended = "stop"
			x.placeRest(ctx, now, w, remaining, res)
			return
		}
	}
	if !cross {
		return
	}
	q := remaining
	if depth > 0 && depth < q {
		q = math.Floor(depth/lot) * lot // 조각 = min(최우선 잔량, 우리 잔량)
	}
	if q <= lot/2 {
		return
	}
	x.placeChild(ctx, now, w, "ioc", q, sw.Limit, res)
}

// sweepEndReason — 창을 지금 닫을 사유 (없으면 "").
func (x *Executor) sweepEndReason(now time.Time, w *store.WorkingOrder) string {
	sw := w.Sweep
	if !now.Before(sw.Until) {
		return "window"
	}
	if sw.IOCErrors >= iocMaxErrors {
		return "error"
	}
	if x.d.Session != nil && !x.d.Session.CanEnter(w.Symbol.Exchange, now) {
		return "session" // IOC 는 접속매매에서만
	}
	if w.Purpose == "exit" {
		if pos, ok := x.position(w.IntentID); ok {
			if te := x.effectiveTimeExit(pos); te != nil && !now.Before(*te) {
				return "time" // ★ 시간청산은 기다리지 않는다
			}
		}
	}
	return ""
}

// placeRest — 창 끝 잔량을 시장가로 낸다 (매수는 max_slip_bp 를 넘으면 포기).
func (x *Executor) placeRest(ctx context.Context, now time.Time, w *store.WorkingOrder, remaining float64, res *Result) {
	sw := w.Sweep
	if w.Purpose == "exit" && !x.canExit(w.Symbol, now) {
		res.Deferred++ // 장 밖 — 열리면(08:59~) 낸다. 로트는 그대로다.
		return
	}
	if w.Purpose == "entry" && x.d.Session != nil && !x.d.Session.CanEnter(w.Symbol.Exchange, now) {
		sw.RestDone = true
		sw.Detail = joinDetail(fmt.Sprintf("장이 닫혀(접속매매 끝) 잔량 %v 을 내지 않았다", remaining), sw.Detail)
		x.saveSweep(ctx, w, res)
		x.finishSweep(ctx, now, w, res)
		return
	}
	if w.Side == "buy" && sw.MaxSlipBp > 0 {
		capPx := sw.Limit * (1 + float64(sw.MaxSlipBp)/10000)
		br, _ := x.bookReader()
		// 호가를 모르면 낸다 — 잔량 시장가는 필수다 (모름을 포기 사유로 쓰지 않는다).
		if bk, err := br.Book(ctx, w.Symbol); err == nil && bk.Ask1 > capPx+1e-9 {
			sw.Ended, sw.RestDone = "slip", true
			sw.Detail = joinDetail(fmt.Sprintf("slip — 창 끝 매도1 %.0f 가 상한 %.0f (cap %.0f + %dbp) 를 넘어 잔량 %v 포기",
				bk.Ask1, capPx, sw.Limit, sw.MaxSlipBp, remaining), sw.Detail)
			x.saveSweep(ctx, w, res)
			x.finishSweep(ctx, now, w, res)
			return
		}
	}
	x.placeChild(ctx, now, w, "rest", remaining, 0, res)
}

// placeChild — 조각 하나를 낸다. kind = ioc (지정가 IOC) | rest (시장가).
func (x *Executor) placeChild(ctx context.Context, now time.Time, w *store.WorkingOrder, kind string,
	qty, price float64, res *Result) {
	sw := w.Sweep
	req := broker.OrderRequest{IntentID: w.IntentID, Symbol: w.Symbol, Qty: qty, Kid: w.Kid, Scope: w.Scope,
		RefPrice: w.RefPrice}
	if kind == "ioc" {
		req.LimitPrice, req.IOC, req.RefPrice = price, true, price
	} else if w.Side == "buy" {
		req.MaxSlipBp = sw.MaxSlipBp
	}

	if sub, ok := x.d.Broker.(broker.Submitter); ok {
		sw.Child = &store.SweepChild{Kind: kind, Qty: qty, Price: price, PlacedAt: now, Unconfirmed: true}
		if err := x.d.Store.PutWorking(ctx, *w); err != nil {
			if w.Purpose == "entry" {
				sw.Child = nil
				res.fail("★ sweep 조각 기록 실패 %s(%s): %v — 내지 않는다", w.IntentID, w.Symbol.Code, err)
				return
			}
			res.fail("sweep 조각 기록 실패 %s(%s): %v — 청산이라 낸다", w.IntentID, w.Symbol.Code, err)
		}
		var s broker.Submitted
		var err error
		if w.Side == "buy" {
			s, err = sub.SubmitBuy(ctx, req)
		} else {
			s, err = sub.SubmitSell(ctx, req)
		}
		if err != nil {
			sw.Child = nil
			x.childFailed(ctx, now, w, kind, err, res)
			return
		}
		sw.Child.OrderID, sw.Child.SubmittedAt, sw.Child.Unconfirmed = s.OrderID, s.SubmittedAt, false
		if s.Qty > 0 {
			sw.Child.Qty = s.Qty
		}
		sw.OrderIDs = append(sw.OrderIDs, s.OrderID)
		if kind == "ioc" {
			sw.IOCs++
		}
		sw.IOCErrors = 0
		x.saveSweep(ctx, w, res)
		return
	}

	// 동기 브로커 (paper) — 체결이 바로 돌아온다.
	var f broker.Fill
	var err error
	if w.Side == "buy" {
		f, err = x.d.Broker.Buy(ctx, req)
	} else {
		f, err = x.d.Broker.Sell(ctx, req)
	}
	if err != nil {
		x.childFailed(ctx, now, w, kind, err, res)
		return
	}
	if f.BrokerOrderID != "" {
		sw.OrderIDs = append(sw.OrderIDs, f.BrokerOrderID)
	}
	if kind == "ioc" {
		sw.IOCs++
	}
	sw.IOCErrors = 0
	if f.Qty > 0 {
		at := f.FilledAt
		if at.IsZero() {
			at = now
		}
		sw.Fills = append(sw.Fills, protocol.SweepFill{OrderID: f.BrokerOrderID, Kind: kind, At: at,
			Price: f.Price, Qty: f.Qty, FeeKRW: f.FeeKRW})
	}
	if kind == "rest" {
		sw.RestDone = true
		sw.Detail = joinDetail(f.Detail, sw.Detail)
	}
	x.saveSweep(ctx, w, res)
	if kind == "rest" {
		x.finishSweep(ctx, now, w, res)
	}
}

// settleChild — 나가 있는 조각의 결과를 본다. true = 끝났다 (체결분을 담고 Child 를 비웠다).
func (x *Executor) settleChild(ctx context.Context, now time.Time, w *store.WorkingOrder, res *Result) bool {
	sw := w.Sweep
	c := sw.Child
	lc, ok := x.d.Broker.(broker.LimitChecker)
	if !ok {
		sw.Child = nil // 동기 브로커는 조각이 나가 있을 수 없다
		return true
	}
	st, err := lc.LimitStatus(ctx, w.Symbol, c.OrderID)
	age := now.Sub(c.PlacedAt)
	if err != nil {
		if age > exitStuckAfter {
			res.fail("sweep 조각 %s(%s) 주문 %s 의 체결을 %s 째 확인하지 못했다: %v",
				w.IntentID, w.Symbol.Code, c.OrderID, age.Round(time.Second), err)
		}
		return false // 조회 실패는 «미체결» 이 아니다
	}
	lot := x.d.Broker.LotSize(w.Symbol)
	done := (st.Known && !st.Open) || st.FilledQty >= c.Qty-lot/2
	if !done && c.Kind == "ioc" {
		wait := iocSettle
		if !st.Known {
			wait = iocUnseen
		}
		if age >= wait {
			// ★ IOC 는 이미 끝났어야 한다. 취소를 한 번 보내(죽은 주문이면 거부 = 확인) 다시 보고, 그 체결을 확정으로 본다.
			if !c.CancelSent {
				c.CancelSent = true
				_ = x.d.Broker.CancelOrder(ctx, w.Symbol, c.OrderID)
				if again, qerr := lc.LimitStatus(ctx, w.Symbol, c.OrderID); qerr == nil {
					st = again
				}
			}
			done = true
		}
	}
	if !done && c.Kind == "rest" {
		tmp := store.WorkingOrder{IntentID: w.IntentID, Symbol: w.Symbol, OrderID: c.OrderID, Qty: c.Qty}
		switch {
		case w.Purpose == "entry" && age > x.entryTimeout() && !x.auction(w.Symbol, now):
			// 진입 잔량이 늦게 차면 다른 가격이다 — 일반 진입과 같은 마감으로 취소하고 찬 만큼만.
			done = x.cancelAndRecheck(ctx, &tmp, lc, &st, res)
		case w.Purpose == "exit" && x.d.Session != nil && x.d.Session.Of(w.Symbol.Exchange, now) == session.Closed:
			done = x.cancelAndRecheck(ctx, &tmp, lc, &st, res)
		case w.Purpose == "exit" && !x.auction(w.Symbol, now) && age > exitStuckAfter:
			res.fail("sweep 청산 잔량 %s(%s) 이 %s 째 다 체결되지 않았다 (%v/%v) — 정지·VI 확인",
				w.IntentID, w.Symbol.Code, age.Round(time.Second), st.FilledQty, c.Qty)
		}
	}
	if !done {
		return false
	}
	if st.FilledQty > lot/2 || (lot < 1 && st.FilledQty > 0) {
		at := st.FilledAt
		if at.IsZero() {
			at = now
		}
		sw.Fills = append(sw.Fills, protocol.SweepFill{OrderID: c.OrderID, Kind: c.Kind, At: at,
			Price: st.AvgPrice, Qty: st.FilledQty, FeeKRW: st.FeeKRW})
	}
	if c.Kind == "rest" {
		sw.RestDone = true
	}
	sw.Child = nil
	x.saveSweep(ctx, w, res)
	return true
}

// childFailed — 조각이 거부됐다.
func (x *Executor) childFailed(ctx context.Context, now time.Time, w *store.WorkingOrder, kind string,
	err error, res *Result) {
	sw := w.Sweep
	switch {
	case errors.Is(err, broker.ErrMaybeSent):
		x.sweepUnknown(ctx, now, w, err, res)
		return
	case w.Side == "buy" && errors.Is(err, broker.ErrInsufficient):
		// 계좌 돈이 모자란다 — 더 사지 않는다. 한 주도 못 샀으면 비종결(E_CAPITAL)로 돌린다 (finishSweep).
		sw.Ended, sw.RestDone = "short", true
		sw.Detail = joinDetail("계좌 주문가능금액 부족 — 남은 수량 포기: "+err.Error(), sw.Detail)
		x.saveSweep(ctx, w, res)
		x.finishSweep(ctx, now, w, res)
		return
	case kind == "ioc":
		// IOC 거부 (가격제한·호가단위 등) — 다음 호가에 다시. 연달아 거부되면 창을 닫고 잔량 시장가로.
		sw.IOCErrors++
		res.fail("sweep IOC 거부 %s(%s) %d회째: %v", w.IntentID, w.Symbol.Code, sw.IOCErrors, err)
		x.saveSweep(ctx, w, res)
		return
	}
	// 잔량 주문이 거부됐다 — 찬 만큼으로 마무리한다.
	sw.RestDone = true
	if sw.Ended == "" {
		sw.Ended = "error"
	}
	sw.Detail = joinDetail("창 끝 잔량 주문 거부: "+err.Error(), sw.Detail)
	x.saveSweep(ctx, w, res)
	x.finishSweep(ctx, now, w, res)
	if w.Purpose == "exit" {
		if pos, ok := x.position(w.IntentID); ok {
			x.sellFailed(ctx, now, pos, w.Reason, err, res) // 잔량은 로트에 남는다 — 쉬었다가 다시 판다
		}
	} else {
		res.fail("sweep 진입 잔량 거부 %s(%s): %v", w.IntentID, w.Symbol.Code, err)
	}
}

// sweepUnknown — 조각이 나갔는지 모른다. ★ 더 내지 않는다 (나갔다면 두 번 산다·판다). 확정된 만큼만 반영하고 알린다.
func (x *Executor) sweepUnknown(ctx context.Context, now time.Time, w *store.WorkingOrder, cause error, res *Result) {
	sw := w.Sweep
	sw.Child = nil
	sw.Ended, sw.RestDone = "unknown", true
	sw.Detail = joinDetail("★ 조각 주문 결과 미상 — 잔고를 사람이 확인할 것: "+cause.Error(), sw.Detail)
	x.saveSweep(ctx, w, res)
	lot := x.d.Broker.LotSize(w.Symbol)
	if filled, _, _ := sw.Filled(); w.Purpose == "entry" && filled <= lot/2 {
		// 확정된 체결이 없다 — 결과 미상 매수와 같다 (목표 종결 · 재진입 차단 · 사람에게).
		x.closeParent(ctx, now, w, res)
		if w.Target != nil {
			x.buyFailed(ctx, now, *w.Target, w.Qty, w.Kid, w.Scope, "", cause, res)
		}
		return
	}
	res.fail("★ sweep 조각 결과 미상 %s(%s) — 확정된 체결만 반영했다. 잔고 확인 필요: %v",
		w.IntentID, w.Symbol.Code, cause)
	x.finishSweep(ctx, now, w, res)
	if w.Purpose == "exit" {
		if pos, ok := x.position(w.IntentID); ok {
			x.sellFailed(ctx, now, pos, w.Reason, cause, res) // 1분 쉬고 다시 본다 (그 사이 체결됐으면 콕핏 밖 매도로 귀속)
		}
	}
}

// finishSweep — 조각을 합쳐 진입·청산 하나로 확정하고 부모 주문을 닫는다.
func (x *Executor) finishSweep(ctx context.Context, now time.Time, w *store.WorkingOrder, res *Result) {
	sw := w.Sweep
	lot := x.d.Broker.LotSize(w.Symbol)
	fill := sweepFill(*w)
	x.d.Log.Info("sweep 끝", "intent_id", w.IntentID, "code", w.Symbol.Code, "side", w.Side,
		"want", w.Qty, "filled", fill.Qty, "avg", fill.Price, "limit", sw.Limit, "ended", sw.Ended,
		"iocs", sw.IOCs, "rest_qty", fill.Exec.RestQty, "ref_ask1", sw.RefAsk1, "ref_bid1", sw.RefBid1)

	switch w.Purpose {
	case "entry":
		t := w.Target
		if t == nil {
			x.closeParent(ctx, now, w, res)
			res.fail("sweep 진입 %s 에 목표가 없다 — 복구 불가 (체결 %v)", w.OrderID, fill.Qty)
			return
		}
		if fill.Qty > lot/2 {
			x.finishEnter(ctx, now, w.AsOfBar, *t, w.Kid, w.Scope, fill, res)
			x.closeParent(ctx, now, w, res)
			return
		}
		x.closeParent(ctx, now, w, res)
		if sw.Ended == "short" {
			// ★ 비종결 — 돈이 돌아오면 같은 목표로 다시 산다 (시장가 진입의 E_CAPITAL 과 같다).
			x.d.Engine.DropPending(t.IntentID)
			x.buyFailed(ctx, now, *t, w.Qty, w.Kid, w.Scope, "", fmt.Errorf("%w: sweep", broker.ErrInsufficient), res)
			return
		}
		phase, detail := "canceled", "sweep 진입이 체결 없이 끝났다 — 진입 포기(목표 종결)"
		if sw.Ended == "slip" {
			phase, detail = "rejected", "slip — sweep 창 끝 잔량이 max_slip_bp 를 넘어 포기 (목표 종결)"
		}
		x.recordFill(ctx, t.Slot, w.Kid, w.Scope, store.Order{
			ID: ids.NewAt(now), IntentID: t.IntentID, Phase: phase, Symbol: t.Symbol, Side: "buy",
			Qty: w.Qty, Detail: joinDetail(detail, sw.Detail), Exec: fill.Exec,
		}, res)
		if err := x.upsertIntent(ctx, store.Intent{
			IntentID: t.IntentID, Slot: t.Slot, Kid: w.Kid, Scope: w.Scope, Group: t.Group,
			Symbol: t.Symbol, Side: string(t.Side),
		}); err == nil {
			_ = x.d.Store.CloseIntent(ctx, t.IntentID, "unfilled", now)
		}
		x.d.Engine.MarkClosed(t.IntentID)
		x.noteClose(protocol.Position{IntentID: t.IntentID, Kid: w.Kid, Scope: w.Scope, Group: t.Group, Symbol: t.Symbol},
			"unfilled", 0, now)

	case "exit":
		pos, ok := x.position(w.IntentID)
		if !ok {
			x.closeParent(ctx, now, w, res)
			res.fail("sweep 청산 %s(%s) — 로트가 이미 없다 (체결 %v)", w.IntentID, w.Symbol.Code, fill.Qty)
			return
		}
		// 체결 0 이면 finishSell 이 «로트 유지 · 다음에 다시» 로 남긴다.
		x.finishSell(ctx, now, pos, w.Qty, w.Reason, fill, res)
		x.closeParent(ctx, now, w, res)
	}
}

// sweepFill — 조각 체결을 합친 체결 하나 (+ exec 내역).
func sweepFill(w store.WorkingOrder) broker.Fill {
	sw := w.Sweep
	qty, amt, fee := sw.Filled()
	rep := &protocol.ExecReport{Mode: protocol.ExecSweep, RefAsk1: sw.RefAsk1, RefBid1: sw.RefBid1,
		Fills: append([]protocol.SweepFill(nil), sw.Fills...), OrderIDs: append([]string(nil), sw.OrderIDs...),
		IOCOrders: sw.IOCs, Ended: sw.Ended, Detail: sw.Detail}
	until := sw.Until
	rep.SweepUntil = &until
	if !sw.RefAt.IsZero() {
		at := sw.RefAt
		rep.RefAt = &at
	}
	if w.Side == "buy" {
		rep.CapPrice = sw.Limit
	} else {
		rep.FloorPrice = sw.Limit
	}
	var restAmt float64
	f := broker.Fill{Qty: qty, FeeKRW: fee, SubmittedAt: w.PlacedAt, FilledAt: w.PlacedAt, Exec: rep, Detail: sw.Detail}
	for _, s := range sw.Fills {
		if f.BrokerOrderID == "" {
			f.BrokerOrderID = s.OrderID // 원장 broker_order_id = 첫 체결 조각 (조각 전부는 exec 에)
		}
		if s.At.After(f.FilledAt) {
			f.FilledAt = s.At
		}
		if s.Kind == "rest" {
			rep.RestQty += s.Qty
			restAmt += s.Qty * s.Price
		}
	}
	if rep.RestQty > 0 {
		rep.RestPrice = restAmt / rep.RestQty
	}
	if qty > 0 {
		f.Price = amt / qty
		f.SlippageBp = broker.SlippageBp(w.Side, w.RefPrice, f.Price)
	}
	f.Partial = qty < w.Qty-1e-9
	return f
}

func (x *Executor) saveSweep(ctx context.Context, w *store.WorkingOrder, res *Result) {
	if err := x.d.Store.PutWorking(ctx, *w); err != nil {
		res.fail("sweep 진행 기록 %s(%s): %v — 재시작하면 이 진행을 잃는다", w.IntentID, w.Symbol.Code, err)
	}
}

// closeParent — 부모 주문을 끝낸다 (원장 working_orders · 진행 중 목록).
func (x *Executor) closeParent(ctx context.Context, now time.Time, w *store.WorkingOrder, res *Result) {
	if err := x.d.Store.FinishWorking(ctx, w.OrderID, now); err != nil {
		res.fail("sweep 종료 기록 %s: %v", w.OrderID, err)
	}
	delete(x.work, w.OrderID)
}
