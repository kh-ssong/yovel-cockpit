// Package executor 는 계획을 실제 주문으로 바꾸는 유일한 지점이다.
//
// ★ reconcile 이 "무엇을 할지" 를 순수 함수로 정하고, 여기서만 "그걸 낸다".
// 둘을 섞지 않는 이유는 테스트가 아니라 안전이다 — 계획 단계의 버그가 곧바로 주문이 되면
// 사람이 끼어들 자리가 없다.
//
// 순서가 곧 안전이다: 실상태 동기 → **청산 먼저** → stop 갱신 → TP 위임 → 진입.
package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/ids"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

type Deps struct {
	Broker    broker.Broker
	Store     *store.Store
	Engine    *engine.Engine
	Mode      protocol.Mode
	DaemonSHA string
	Log       *slog.Logger
}

type Executor struct{ d Deps }

func New(d Deps) *Executor {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Executor{d: d}
}

// Result — 이번 틱에 실제로 일어난 일.
type Result struct {
	Entered    int `json:"entered"`
	Exited     int `json:"exited"`
	StopsArmed int `json:"stops_armed"`
	TpPlaced   int `json:"tp_placed"`
	// ClosedByBroker — 우리가 안 팔았는데 브로커에서 사라진 포지션 (TP 체결 또는 수동 매도).
	ClosedByBroker int `json:"closed_by_broker"`
	// Mismatch — 장부와 실물이 어긋난 지점. ★ 추측해서 맞추지 않는다, 보고만 한다.
	Mismatch []string `json:"mismatch,omitempty"`
	Errors   []string `json:"errors,omitempty"`
}

func (r *Result) fail(format string, args ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}

// Tick 은 한 번 돌린다. ★ 한 종목이 실패해도 나머지는 계속 간다 —
// 09:00 에 한 종목의 거부가 전체 집행을 막으면 그날 매매가 통째로 증발한다.
func (x *Executor) Tick(ctx context.Context, now time.Time) Result {
	var res Result

	x.syncPositions(ctx, now, &res)

	plan := x.d.Engine.Plan(now)

	// ① 청산 먼저. 진입 실패는 기회 상실(유한)이지만 청산 실패는 손실 노출(무한)이다.
	for _, e := range plan.Exits {
		x.doExit(ctx, now, e.Position, e.Reason, &res)
	}

	// ② stop 갱신 — 브로커에 낼 주문이 아니라 우리가 기억할 숫자다.
	for _, u := range plan.StopUpdates {
		if err := x.armStop(ctx, u.Position, u.To); err != nil {
			res.fail("stop 갱신 %s: %v", u.Position.IntentID, err)
			continue
		}
		res.StopsArmed++
	}

	// ③ TP 위임 — exit 3층 중 제일 튼튼한 층. 데몬도 서버도 죽어도 이건 체결된다.
	for _, u := range plan.TpUpdates {
		if err := x.placeTP(ctx, u.Position, u.To); err != nil {
			res.fail("TP 위임 %s: %v", u.Position.IntentID, err)
			continue
		}
		res.TpPlaced++
	}

	// ④ 진입은 마지막.
	for _, e := range plan.Enters {
		x.doEnter(ctx, now, e.Target, e.Qty, e.Price, &res)
	}

	if plan.DroppedEnters > 0 {
		// ★ 조용한 절단 금지.
		x.d.Log.Warn("주문 상한에 걸려 진입을 잘랐다", "dropped", plan.DroppedEnters)
	}
	return res
}

// syncPositions — ★ 실상태의 SSOT 는 브로커다. 우리 원장은 "왜 샀는지" 만 안다.
//
// 브로커에서 사라진 포지션은 둘 중 하나다: 위임한 TP 가 체결됐거나, 사용자가 직접 팔았거나.
// ★ 이 둘을 뭉뚱그리면 형제 레그를 잘못 청산하는 사고가 난다. 구분할 근거는 하나뿐 —
// 우리가 TP 를 걸어뒀는가. 그마저도 확실하지 않으므로 detail 에 "사후 감지" 를 남긴다.
func (x *Executor) syncPositions(ctx context.Context, now time.Time, res *Result) {
	holdings, err := x.d.Broker.Positions(ctx)
	if err != nil {
		res.fail("브로커 포지션 조회: %v", err)
		return // ★ 조회 실패를 "포지션 없음" 으로 읽지 않는다. 그러면 전부 종결시켜 버린다.
	}
	byCode := map[string]broker.Holding{}
	for _, h := range holdings {
		byCode[h.Symbol.Code] = h
	}

	open, err := x.d.Store.OpenIntents(ctx)
	if err != nil {
		res.fail("로컬 원장 조회: %v", err)
		return
	}

	// 한 종목을 여러 intent 가 나눠 가질 수 있다. 그 경우 브로커 수량을 어떻게 배분할지
	// 알 방법이 없으므로 ★ 추측하지 않고 mismatch 로 보고만 한다.
	expected := map[string]float64{}
	for _, p := range open {
		expected[p.Symbol.Code] += p.Qty
	}

	for _, p := range open {
		h, ok := byCode[p.Symbol.Code]
		if !ok || h.Qty <= 0 {
			reason, src := "manual", store.SourceManual
			if p.TpOrderID != "" {
				reason, src = "tp", store.SourceBot
			}
			x.recordOrder(ctx, store.Order{
				ID: ids.NewAt(now), IntentID: p.IntentID, Phase: "exit_filled",
				Symbol: p.Symbol, Side: "sell", Qty: p.Qty, ExitReason: reason, Source: src,
				Detail: "브로커 조회로 사후 감지 — 체결가·시각 미상",
			}, res)

			if err := x.d.Store.CloseIntent(ctx, p.IntentID, reason, now); err != nil {
				res.fail("종결 %s: %v", p.IntentID, err)
				continue
			}
			x.d.Engine.MarkClosed(p.IntentID)
			res.ClosedByBroker++
			x.d.Log.Warn("브로커에서 사라진 포지션을 종결 처리했다",
				"intent_id", p.IntentID, "code", p.Symbol.Code, "reason", reason)
			continue
		}

		if expected[p.Symbol.Code] > h.Qty+1e-9 {
			res.Mismatch = append(res.Mismatch, fmt.Sprintf(
				"%s: 장부 %.0f주 vs 실물 %.0f주", p.Symbol.Code, expected[p.Symbol.Code], h.Qty))
		}

		pos := p
		pos.AvgEntryPrice = h.AvgPrice
		x.d.Engine.UpsertPosition(pos)
	}
}

func (x *Executor) doExit(ctx context.Context, now time.Time, pos protocol.Position, reason string, res *Result) {
	// ★ 걸어둔 TP 지정가를 먼저 취소하지 않으면 그 수량이 잠겨 시장가 매도가 거부된다.
	if pos.TpOrderID != "" {
		if err := x.d.Broker.CancelOrder(ctx, pos.Symbol, pos.TpOrderID); err != nil {
			res.fail("TP 취소 %s: %v", pos.IntentID, err)
			return // 취소 실패 상태로 매도를 시도하면 "잔고 부족" 만 반복한다
		}
	}

	fill, err := x.d.Broker.Sell(ctx, broker.OrderRequest{
		IntentID: pos.IntentID, Symbol: pos.Symbol, Qty: pos.Qty,
	})
	if err != nil {
		res.fail("매도 %s(%s): %v", pos.IntentID, pos.Symbol.Code, err)
		return
	}

	x.recordOrder(ctx, store.Order{
		ID: ids.NewAt(now), IntentID: pos.IntentID, Phase: "exit_filled",
		Symbol: pos.Symbol, Side: "sell", Qty: fill.Qty, Price: fill.Price,
		BrokerOrderID: fill.BrokerOrderID, SubmittedAt: &fill.SubmittedAt, FilledAt: &fill.FilledAt,
		SlippageBp: fill.SlippageBp, FeeKRW: fill.FeeKRW, ExitReason: reason,
		RealizedPct: realizedPct(pos.AvgEntryPrice, fill.Price),
		Source:      store.SourceBot,
	}, res)

	if err := x.d.Store.CloseIntent(ctx, pos.IntentID, reason, now); err != nil {
		res.fail("종결 %s: %v", pos.IntentID, err)
		return
	}
	x.d.Engine.MarkClosed(pos.IntentID)
	res.Exited++
}

func (x *Executor) doEnter(ctx context.Context, now time.Time, t protocol.Target, qty, ref float64, res *Result) {
	req := broker.OrderRequest{
		IntentID: t.IntentID, Symbol: t.Symbol, Qty: qty, RefPrice: ref,
	}
	if t.Entry != nil {
		req.MaxSlipBp = t.Entry.MaxSlipBp
		if t.Entry.Mode == "limit" {
			req.LimitPrice = t.Entry.LimitPrice
		}
	}

	fill, err := x.d.Broker.Buy(ctx, req)
	if err != nil {
		res.fail("매수 %s(%s): %v", t.IntentID, t.Symbol.Code, err)
		return
	}

	var signalTS *time.Time
	if t.Entry != nil && !t.Entry.NotAfter.IsZero() {
		// 신호가 나온 봉을 근사한다. 정확한 as_of_bar 는 목표 스냅샷에 있고,
		// 원장에서 재는 것은 "신호 → 체결" 이므로 이 근사로도 방향은 맞는다.
		signalTS = &t.Entry.NotAfter
	}

	x.recordOrder(ctx, store.Order{
		ID: ids.NewAt(now), IntentID: t.IntentID, Phase: "filled",
		Symbol: t.Symbol, Side: "buy", Qty: fill.Qty, Price: fill.Price,
		BrokerOrderID: fill.BrokerOrderID, SignalTS: signalTS,
		SubmittedAt: &fill.SubmittedAt, FilledAt: &fill.FilledAt,
		SlippageBp: fill.SlippageBp, FeeKRW: fill.FeeKRW, Source: store.SourceBot,
	}, res)

	in := store.Intent{
		IntentID: t.IntentID, Slot: t.Slot, Symbol: t.Symbol, Side: string(t.Side),
		Qty: fill.Qty, AvgEntryPrice: fill.Price, EntryAt: &fill.FilledAt,
	}
	if t.Exit != nil {
		in.StopArmed, in.TpPrice, in.TimeExitAt = t.Exit.StopPrice, t.Exit.TpPrice, t.Exit.TimeExitAt
	}
	if err := x.d.Store.UpsertIntent(ctx, in); err != nil {
		// ★ 이건 조용히 넘길 수 없다. 원장에 없으면 이 포지션은 다음 틱에 유령이 된다.
		res.fail("★ 진입은 체결됐는데 원장 기록 실패 %s: %v", t.IntentID, err)
	}

	x.d.Engine.UpsertPosition(protocol.Position{
		IntentID: t.IntentID, Slot: t.Slot, Symbol: t.Symbol,
		Qty: fill.Qty, AvgEntryPrice: fill.Price, EntryAt: &fill.FilledAt,
		StopArmed: in.StopArmed,
	})
	res.Entered++
}

// armStop 은 stop 을 로컬에 무장한다. 브로커에 주문을 내지 않는다.
func (x *Executor) armStop(ctx context.Context, pos protocol.Position, to float64) error {
	pos.StopArmed = to
	if err := x.d.Store.UpsertIntent(ctx, store.Intent{
		IntentID: pos.IntentID, Slot: pos.Slot, Symbol: pos.Symbol, Side: "long",
		Qty: pos.Qty, AvgEntryPrice: pos.AvgEntryPrice, StopArmed: to, TpOrderID: pos.TpOrderID,
	}); err != nil {
		return err
	}
	x.d.Engine.UpsertPosition(pos)
	return nil
}

func (x *Executor) placeTP(ctx context.Context, pos protocol.Position, price float64) error {
	if pos.TpOrderID != "" {
		if err := x.d.Broker.CancelOrder(ctx, pos.Symbol, pos.TpOrderID); err != nil {
			return fmt.Errorf("기존 TP 취소: %w", err)
		}
	}
	id, err := x.d.Broker.PlaceTP(ctx, pos.Symbol, pos.Qty, price)
	if err != nil {
		return err
	}
	pos.TpOrderID = id
	if err := x.d.Store.UpsertIntent(ctx, store.Intent{
		IntentID: pos.IntentID, Slot: pos.Slot, Symbol: pos.Symbol, Side: "long",
		Qty: pos.Qty, AvgEntryPrice: pos.AvgEntryPrice, StopArmed: pos.StopArmed,
		TpPrice: price, TpOrderID: id,
	}); err != nil {
		return err
	}
	x.d.Engine.UpsertPosition(pos)
	return nil
}

// recordOrder 는 원장에 남기고 업링크 큐에 넣는다.
//
// ★ 큐에는 봉투가 아니라 body 만 넣는다. 봉투(서명·seq·acct)는 릴레이 계층이 만든다 —
// 여기서 만들면 릴레이가 바뀔 때 원장 기록 코드까지 따라 바뀐다.
func (x *Executor) recordOrder(ctx context.Context, o store.Order, res *Result) {
	o.Mode = x.d.Mode
	o.DaemonSHA = x.d.DaemonSHA
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	if err := x.d.Store.RecordOrder(ctx, o); err != nil {
		res.fail("원장 기록 %s: %v", o.IntentID, err)
		return
	}

	ev := protocol.EventOrder{
		IntentID: o.IntentID, Phase: o.Phase, Symbol: o.Symbol, Side: o.Side,
		Qty: o.Qty, Price: o.Price, BrokerOrderID: o.BrokerOrderID,
		SignalTS: o.SignalTS, SubmittedAt: o.SubmittedAt, FilledAt: o.FilledAt,
		SlippageBp: o.SlippageBp, FeeKRW: o.FeeKRW, ExitReason: o.ExitReason,
		RealizedPct: o.RealizedPct, Detail: o.Detail,
	}
	body, err := json.Marshal(ev)
	if err != nil {
		res.fail("업링크 직렬화 %s: %v", o.IntentID, err)
		return
	}
	if err := x.d.Store.Enqueue(ctx, o.ID, protocol.TypeEventOrder, body); err != nil {
		res.fail("업링크 큐 %s: %v", o.IntentID, err)
	}
}

func realizedPct(entry, exit float64) float64 {
	if entry <= 0 {
		return 0
	}
	return exit/entry - 1
}
