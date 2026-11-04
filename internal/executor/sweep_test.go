package executor

// 집행 방식 sweep — 창 안 IOC 조각 + 창 끝 잔량 시장가 (sweep.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// sweepBroker — 호가를 손으로 정하는 가짜 실브로커 (Submitter · LimitChecker · BookReader).
// IOC 는 매도1(매수1)이 지정가 안이면 최우선 잔량까지 그 호가에 체결, 아니면 0 체결. 시장가는 최우선 호가에 전량.
type sweepBroker struct {
	mu      sync.Mutex
	book    broker.Book
	seq     int
	orders  []sbOrder
	status  map[string]broker.LimitStatus
	pending map[string]bool // 체결을 아직 안 물어본 주문
	held    map[string]float64
	// overlap — 앞 조각의 결과를 묻기 전에 다음 조각이 나갔다 (★ 있으면 안 된다).
	overlap int
	failBuy error
}

type sbOrder struct {
	ID, Side     string
	Qty, Limit   float64
	IOC          bool
	Filled, Avg  float64
	SubmittedQty float64
}

func newSweepBroker() *sweepBroker {
	return &sweepBroker{status: map[string]broker.LimitStatus{}, pending: map[string]bool{}, held: map[string]float64{}}
}

func (b *sweepBroker) setBook(ask, askQty, bid, bidQty float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.book = broker.Book{Ask1: ask, Ask1Qty: askQty, Bid1: bid, Bid1Qty: bidQty}
}

func (b *sweepBroker) Name() string { return "sweepfake" }
func (b *sweepBroker) Positions(context.Context) ([]broker.Holding, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []broker.Holding
	for c, q := range b.held {
		if q > 0 {
			out = append(out, broker.Holding{Symbol: protocol.Symbol{Exchange: "KRX", Code: c}, Qty: q, Sellable: q})
		}
	}
	return out, nil
}
func (b *sweepBroker) Cash(context.Context) (broker.Cash, error) {
	return broker.Cash{Deposit: 1e8, Orderable: 1e8, Seed: 1e8}, nil
}
func (b *sweepBroker) Quote(_ context.Context, s protocol.Symbol) (broker.Quote, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return broker.Quote{Symbol: s, Price: b.book.Bid1, AsOf: time.Now()}, nil
}
func (b *sweepBroker) Buy(context.Context, broker.OrderRequest) (broker.Fill, error) {
	return broker.Fill{}, fmt.Errorf("동기 매수는 안 쓴다")
}
func (b *sweepBroker) Sell(context.Context, broker.OrderRequest) (broker.Fill, error) {
	return broker.Fill{}, fmt.Errorf("동기 매도는 안 쓴다")
}
func (b *sweepBroker) PlaceTP(context.Context, protocol.Symbol, float64, float64) (string, error) {
	return "", broker.ErrNotSupported
}
func (b *sweepBroker) CancelOrder(context.Context, protocol.Symbol, string) error { return nil }
func (b *sweepBroker) LotSize(protocol.Symbol) float64                            { return 1 }
func (b *sweepBroker) MinOrderValue(protocol.Symbol) float64                      { return 0 }
func (b *sweepBroker) Book(context.Context, protocol.Symbol) (broker.Book, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.book.Ask1 <= 0 && b.book.Bid1 <= 0 {
		return broker.Book{}, fmt.Errorf("호가 없음")
	}
	return b.book, nil
}

func (b *sweepBroker) submit(side string, req broker.OrderRequest) (broker.Submitted, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if side == "buy" && b.failBuy != nil {
		return broker.Submitted{}, b.failBuy
	}
	if len(b.pending) > 0 {
		b.overlap++
	}
	b.seq++
	id := fmt.Sprintf("k-%d", b.seq)
	px, depth := b.book.Ask1, b.book.Ask1Qty
	ok := px > 0 && (req.LimitPrice <= 0 || px <= req.LimitPrice)
	if side == "sell" {
		px, depth = b.book.Bid1, b.book.Bid1Qty
		ok = px > 0 && (req.LimitPrice <= 0 || px >= req.LimitPrice)
		if b.held[req.Symbol.Code] < req.Qty {
			return broker.Submitted{}, fmt.Errorf("%w: 보유 %v", broker.ErrNotEnoughShare, b.held[req.Symbol.Code])
		}
	}
	q := 0.0
	if ok {
		q = req.Qty
		if req.IOC && depth > 0 && depth < q {
			q = depth
		}
	}
	if !req.IOC && req.LimitPrice > 0 {
		return broker.Submitted{}, fmt.Errorf("이 가짜는 IOC 아닌 지정가를 받지 않는다")
	}
	if side == "buy" {
		b.held[req.Symbol.Code] += q
	} else {
		b.held[req.Symbol.Code] -= q
	}
	b.orders = append(b.orders, sbOrder{ID: id, Side: side, Qty: req.Qty, Limit: req.LimitPrice, IOC: req.IOC, Filled: q, Avg: px})
	b.status[id] = broker.LimitStatus{FilledQty: q, AvgPrice: px, FeeKRW: math.Round(q * px * 0.0001), Known: true, Open: false}
	b.pending[id] = true
	return broker.Submitted{OrderID: id, Qty: req.Qty, RefPrice: req.RefPrice, SubmittedAt: time.Now()}, nil
}

func (b *sweepBroker) SubmitBuy(_ context.Context, req broker.OrderRequest) (broker.Submitted, error) {
	return b.submit("buy", req)
}
func (b *sweepBroker) SubmitSell(_ context.Context, req broker.OrderRequest) (broker.Submitted, error) {
	return b.submit("sell", req)
}
func (b *sweepBroker) LimitStatus(_ context.Context, _ protocol.Symbol, id string) (broker.LimitStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, id)
	st, ok := b.status[id]
	if !ok {
		return broker.LimitStatus{Open: true}, nil
	}
	return st, nil
}

type sweepHarness struct {
	*harness
	fb *sweepBroker
}

func newSweepHarness(t *testing.T, t0 time.Time) *sweepHarness {
	t.Helper()
	h := newHarness(t)
	h.eng = engine.New(engine.Config{
		Mode: protocol.ModeLive, Policy: h.eng.Policy(), TargetMaxAge: 180 * time.Second, MaxOrders: 10,
		EngineBudget: 1_000_000,
		Price:        func(protocol.Symbol) (float64, bool) { return 1000, true },
		Market:       func(protocol.Symbol) sizing.Market { return sizing.StockMarket() },
		Store:        h.st,
	}, t0)
	fb := newSweepBroker()
	cal, _ := session.NewCalendar(session.DefaultKRXHolidays)
	h.x = New(Deps{
		Broker: fb, Store: h.st, Engine: h.eng, Mode: protocol.ModeLive, DaemonSHA: "test",
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Session: cal, ExitCutoff: "15:15", BrokerExchange: "KRX",
	})
	return &sweepHarness{harness: h, fb: fb}
}

func (h *sweepHarness) apply(t *testing.T, seq uint64, at time.Time, tgts ...map[string]any) {
	t.Helper()
	arr := make([]any, len(tgts))
	for i, x := range tgts {
		arr[i] = x
	}
	m := map[string]any{
		"v": 1, "typ": "intent.target", "id": fmt.Sprintf("01J9Z8QK3M7X2ABCDEFGHJK%03d", seq),
		"acct": "acc_7f3a", "ts": at, "exp": at.Add(time.Minute), "seq": seq,
		"body": map[string]any{"as_of_bar": at, "book_state": "normal", "targets": arr},
	}
	raw, _ := json.Marshal(m)
	signed, err := protocol.Sign(raw, kid, h.pk)
	if err != nil {
		t.Fatal(err)
	}
	if ack := h.eng.Apply(signed, at.Add(500*time.Millisecond)); ack.Status != "applied" {
		t.Fatalf("목표 %+v", ack)
	}
}

func (h *sweepHarness) ledger(t *testing.T) []store.Order {
	t.Helper()
	rows, err := h.st.Ledger(ctx, store.LedgerQuery{Mode: protocol.ModeLive})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func (h *sweepHarness) lot(t *testing.T) protocol.Position {
	t.Helper()
	ps := h.eng.Positions()
	if len(ps) != 1 {
		t.Fatalf("로트 %d 개: %+v", len(ps), ps)
	}
	return ps[0]
}

// run — t 부터 to 까지 0.5초마다 진행 중 주문을 확인한다 (데몬의 빠른 루프).
func (h *sweepHarness) run(from, to time.Time) {
	for now := from; !now.After(to); now = now.Add(500 * time.Millisecond) {
		h.x.PollWorking(ctx, now)
	}
}

// sweepOpen — weight 0.01 × 100만 ÷ ref 1000 = 10주.
func sweepOpen(t0 time.Time, capPx float64, window, notAfter time.Duration, slip int) map[string]any {
	return map[string]any{
		"intent_id": aid, "slot": "main", "side": "long", "want": "open", "weight": 0.01,
		"symbol": map[string]any{"exchange": "KRX", "code": "005930"},
		"entry": map[string]any{"mode": "sweep", "not_after": t0.Add(notAfter), "ref_price": 1000,
			"cap_price": capPx, "sweep_until": t0.Add(window), "max_slip_bp": slip},
		"exit": map[string]any{"stop_price": 850},
	}
}

func sweepFlat(until time.Time, floor float64) map[string]any {
	return map[string]any{"intent_id": aid, "slot": "main", "side": "long", "want": "flat",
		"symbol":    map[string]any{"exchange": "KRX", "code": "005930"},
		"exit_exec": map[string]any{"mode": "sweep", "floor_price": floor, "sweep_until": until}}
}

func onlyRow(t *testing.T, rows []store.Order, phase string) store.Order {
	t.Helper()
	var got []store.Order
	for _, o := range rows {
		if o.Phase == phase {
			got = append(got, o)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%s 행 %d 개: %+v", phase, len(got), rows)
	}
	return got[0]
}

// ★ 창 안에서는 cap 이하 호가만 IOC 로 담고, 창이 끝나면 잔량을 시장가로 산다. 원장엔 진입 한 줄 + 조각 내역.
func TestSweepEntryIOCThenRestAtWindowEnd(t *testing.T) {
	t0 := kst("10:00:00")
	h := newSweepHarness(t, t0)
	h.fb.setBook(1000, 3, 995, 50) // 매도1 1000 (= cap) 에 3주
	h.apply(t, 1, t0, sweepOpen(t0, 1000, 30*time.Second, 35*time.Second, 80))

	h.x.Tick(ctx, t0.Add(time.Second)) // 시작 — 첫 호가에 IOC 3주
	if l := h.lot(t); !l.Pending || l.Qty != 10 {
		t.Fatalf("대기 로트 %+v", l)
	}
	h.fb.setBook(1005, 100, 1000, 50) // cap 위로 — 창 안에서는 사지 않는다
	h.run(t0.Add(1500*time.Millisecond), t0.Add(28500*time.Millisecond))
	if n := len(h.fb.orders); n != 1 {
		t.Fatalf("cap 위 호가에 IOC 를 냈다: %+v", h.fb.orders)
	}
	h.fb.setBook(1000, 2, 995, 50) // 다시 cap — 2주 더 (호가는 1초마다 본다: 29초)
	h.run(t0.Add(29*time.Second), t0.Add(29500*time.Millisecond))
	h.fb.setBook(1005, 100, 1000, 50)
	h.run(t0.Add(30*time.Second), t0.Add(32*time.Second)) // 창 끝 — 잔량 5주 시장가 @1005

	if h.x.HasWorking() {
		t.Fatalf("sweep 이 안 끝났다: %+v", h.x.work)
	}
	if h.fb.overlap != 0 {
		t.Fatalf("★ 앞 조각 결과를 묻기 전에 다음 조각을 냈다 (%d)", h.fb.overlap)
	}
	l := h.lot(t)
	if l.Pending || l.Qty != 10 || math.Abs(l.AvgEntryPrice-(5*1000+5*1005)/10.0) > 1e-9 {
		t.Fatalf("로트 %+v", l)
	}
	o := onlyRow(t, h.ledger(t), "filled")
	e := o.Exec
	if o.Qty != 10 || e == nil || e.Mode != "sweep" || e.CapPrice != 1000 || e.IOCOrders != 2 ||
		e.RestQty != 5 || e.RestPrice != 1005 || len(e.Fills) != 3 || len(e.OrderIDs) != 3 ||
		e.RefAsk1 != 1000 || e.RefBid1 != 995 || e.Ended != "window" {
		t.Fatalf("원장 %+v exec %+v", o, e)
	}
	if o.BrokerOrderID != e.Fills[0].OrderID {
		t.Fatalf("broker_order_id %q — 첫 체결 조각이어야 한다", o.BrokerOrderID)
	}
	// 업링크 event.order 에도 exec 가 실린다.
	items, _ := h.st.Pending(ctx, 100)
	found := false
	for _, it := range items {
		var ev protocol.EventOrder
		if json.Unmarshal(it.Payload, &ev) == nil && ev.Phase == "filled" && ev.Exec != nil && ev.Exec.IOCOrders == 2 {
			found = true
		}
	}
	if !found {
		t.Fatal("event.order 에 exec 가 없다")
	}
}

// 조각 크기 = min(매도1 잔량, 우리 잔량) · 다 차면 창 끝을 기다리지 않는다.
func TestSweepEntryFillsInsideWindow(t *testing.T) {
	t0 := kst("10:00:00")
	h := newSweepHarness(t, t0)
	h.fb.setBook(995, 100, 990, 50) // cap 1000 아래 · 잔량 넉넉
	h.apply(t, 1, t0, sweepOpen(t0, 1000, 30*time.Second, 35*time.Second, 80))
	h.x.Tick(ctx, t0.Add(time.Second))
	h.run(t0.Add(1500*time.Millisecond), t0.Add(2*time.Second))
	if h.x.HasWorking() || len(h.fb.orders) != 1 || h.fb.orders[0].Qty != 10 || !h.fb.orders[0].IOC {
		t.Fatalf("한 조각에 다 차야 한다: %+v", h.fb.orders)
	}
	o := onlyRow(t, h.ledger(t), "filled")
	if o.Exec.Ended != "all" || o.Exec.RestQty != 0 || o.Price != 995 {
		t.Fatalf("원장 %+v exec %+v", o, o.Exec)
	}
}

// ★ 창 끝 매도1 이 cap + max_slip_bp 를 넘으면 잔량을 포기한다 — 한 주도 못 샀으면 rejected(slip) · 목표 종결.
func TestSweepEntrySlipGivesUp(t *testing.T) {
	t0 := kst("10:00:00")
	h := newSweepHarness(t, t0)
	h.fb.setBook(1100, 100, 1095, 50) // cap 1000 · slip 80bp → 상한 1008 — 창 내내 위
	h.apply(t, 1, t0, sweepOpen(t0, 1000, 30*time.Second, 35*time.Second, 80))
	h.x.Tick(ctx, t0.Add(time.Second))
	h.run(t0.Add(1500*time.Millisecond), t0.Add(31*time.Second))
	if h.x.HasWorking() || len(h.fb.orders) != 0 {
		t.Fatalf("포기해야 한다: %+v", h.fb.orders)
	}
	if ps := h.eng.Positions(); len(ps) != 0 {
		t.Fatalf("로트가 남았다 %+v", ps)
	}
	o := onlyRow(t, h.ledger(t), "rejected")
	if o.Exec == nil || o.Exec.Ended != "slip" {
		t.Fatalf("rejected 행 %+v exec %+v", o, o.Exec)
	}
	// 같은 목표로 다시 사지 않는다.
	h.x.Tick(ctx, t0.Add(32*time.Second))
	if len(h.fb.orders) != 0 {
		t.Fatal("포기한 목표를 다시 샀다")
	}
}

// 진입 하나를 시장가로 만들어 둔다 (청산 테스트용).
func (h *sweepHarness) holdLot(t *testing.T, t0 time.Time) {
	t.Helper()
	h.fb.setBook(1000, 100, 995, 100)
	tg := sweepOpen(t0, 1000, 30*time.Second, 35*time.Second, 80)
	tg["entry"] = map[string]any{"mode": "market", "not_after": t0.Add(time.Minute), "ref_price": 1000}
	h.apply(t, 1, t0, tg)
	h.x.Tick(ctx, t0.Add(time.Second))
	h.run(t0.Add(1500*time.Millisecond), t0.Add(2*time.Second))
	if l := h.lot(t); l.Pending || l.Qty != 10 {
		t.Fatalf("보유 준비 %+v", l)
	}
}

// ★ 청산 sweep — 매수1 ≥ floor 면 IOC 매도, 창 끝 잔량 시장가. 원장엔 청산 한 줄 + 조각 내역.
func TestSweepExitIOCThenRest(t *testing.T) {
	t0 := kst("10:00:00")
	h := newSweepHarness(t, t0)
	h.holdLot(t, t0)
	sold0 := len(h.fb.orders)

	t1 := kst("10:05:00")
	h.fb.setBook(1015, 100, 1010, 4) // 매수1 1010 = floor 에 4주
	h.apply(t, 2, t1, sweepFlat(t1.Add(30*time.Second), 1010))
	h.x.Tick(ctx, t1.Add(time.Second))
	h.fb.setBook(1010, 100, 1005, 100) // floor 아래 — 창 안에서는 팔지 않는다
	h.run(t1.Add(1500*time.Millisecond), t1.Add(29*time.Second))
	if n := len(h.fb.orders) - sold0; n != 1 {
		t.Fatalf("floor 아래에서 팔았다: %+v", h.fb.orders[sold0:])
	}
	h.run(t1.Add(30*time.Second), t1.Add(31*time.Second)) // 창 끝 — 잔량 6주 시장가 @1005
	if h.x.HasWorking() || len(h.eng.Positions()) != 0 {
		t.Fatalf("청산이 안 끝났다 work=%v pos=%+v", h.x.work, h.eng.Positions())
	}
	o := onlyRow(t, h.ledger(t), "exit_filled")
	if o.Qty != 10 || o.ExitReason != "flat" || o.Exec == nil || o.Exec.FloorPrice != 1010 ||
		o.Exec.RestQty != 6 || o.Exec.IOCOrders != 1 || math.Abs(o.Price-(4*1010+6*1005)/10.0) > 1e-9 {
		t.Fatalf("청산 원장 %+v exec %+v", o, o.Exec)
	}
	if h.fb.overlap != 0 {
		t.Fatalf("조각이 겹쳤다 %d", h.fb.overlap)
	}
}

// ★ 청산 sweep 중 매수1 이 stop 에 닿으면 창을 기다리지 않고 잔량을 바로 시장가로.
func TestSweepExitStopDoesNotWait(t *testing.T) {
	t0 := kst("10:00:00")
	h := newSweepHarness(t, t0)
	h.holdLot(t, t0)

	t1 := kst("10:05:00")
	h.fb.setBook(1000, 100, 990, 100) // floor 1010 아래 — IOC 없음
	h.apply(t, 2, t1, sweepFlat(t1.Add(30*time.Second), 1010))
	h.x.Tick(ctx, t1.Add(time.Second))
	h.fb.setBook(850, 100, 849, 100) // stop 850 아래로 급락
	h.run(t1.Add(2*time.Second), t1.Add(4*time.Second))
	if h.x.HasWorking() || len(h.eng.Positions()) != 0 {
		t.Fatalf("stop 에 닿았는데 창을 기다린다 work=%v", h.x.work)
	}
	o := onlyRow(t, h.ledger(t), "exit_filled")
	if o.Exec == nil || o.Exec.Ended != "stop" || o.Exec.RestQty != 10 {
		t.Fatalf("청산 원장 %+v exec %+v", o, o.Exec)
	}
}

// ★ 재시작 — 조각을 보내던 중 죽었다 (접수 기록 없음). 그 조각은 나갔는지 모른다: 더 내지 않고, 확정 체결이
// 없으면 결과 미상 매수와 같이 목표를 종결한다 (재진입 차단).
func TestSweepRestartWithUnconfirmedChild(t *testing.T) {
	t0 := kst("10:00:00")
	h := newSweepHarness(t, t0)
	until := t0.Add(30 * time.Second)
	tg := protocol.Target{IntentID: aid, Slot: "main", Side: "long", Want: protocol.WantOpen, Weight: 0.01,
		Symbol: code, Entry: &protocol.Entry{Mode: "sweep", NotAfter: t0.Add(35 * time.Second), RefPrice: 1000,
			CapPrice: 1000, SweepUntil: &until}, Exit: &protocol.Exit{StopPrice: 850}}
	w := store.WorkingOrder{OrderID: "sweep-entry-x", IntentID: aid, Purpose: "entry", Side: "buy", Symbol: code,
		Qty: 10, RefPrice: 1000, Kid: kid, Target: &tg, PlacedAt: t0,
		Sweep: &store.Sweep{Limit: 1000, Until: until,
			Child: &store.SweepChild{Kind: "ioc", Qty: 10, Price: 1000, PlacedAt: t0, Unconfirmed: true}}}
	if err := h.st.PutWorking(ctx, w); err != nil {
		t.Fatal(err)
	}
	h.fb.setBook(1000, 100, 995, 100)
	res := h.x.Tick(ctx, t0.Add(2*time.Second)) // 재시작 뒤 첫 틱
	h.run(t0.Add(2500*time.Millisecond), t0.Add(40*time.Second))
	if len(h.fb.orders) != 0 || h.x.HasWorking() {
		t.Fatalf("결과 미상인데 더 냈다: %+v", h.fb.orders)
	}
	if len(res.Errors) == 0 {
		t.Fatal("결과 미상을 알리지 않았다")
	}
	term, _ := h.st.TerminalIntents(ctx)
	if _, ok := term[aid]; !ok {
		t.Fatal("목표를 종결하지 않았다 — 재시작 뒤 같은 목표로 또 산다")
	}
}

// 재시작 — 접수된 조각까지 기록돼 있으면 이어서 간다 (그 조각의 체결을 담고 창 끝에 잔량).
func TestSweepRestartResumes(t *testing.T) {
	t0 := kst("10:00:00")
	h := newSweepHarness(t, t0)
	h.fb.setBook(1000, 3, 995, 50)
	h.apply(t, 1, t0, sweepOpen(t0, 1000, 30*time.Second, 35*time.Second, 80))
	h.x.Tick(ctx, t0.Add(time.Second)) // IOC 3주 접수 (체결 확인 전)

	// 새 집행기 = 재시작 (같은 원장·엔진).
	h.x = New(Deps{Broker: h.fb, Store: h.st, Engine: h.eng, Mode: protocol.ModeLive, DaemonSHA: "test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Session: h.x.d.Session, ExitCutoff: "15:15",
		BrokerExchange: "KRX"})
	h.fb.setBook(1005, 100, 1000, 50)
	h.run(t0.Add(2*time.Second), t0.Add(32*time.Second))
	if h.x.HasWorking() {
		t.Fatalf("재시작 뒤 sweep 이 안 끝났다")
	}
	o := onlyRow(t, h.ledger(t), "filled")
	if o.Qty != 10 || o.Exec.IOCOrders != 1 || o.Exec.RestQty != 7 {
		t.Fatalf("원장 %+v exec %+v", o, o.Exec)
	}
}

// 계좌 돈이 모자라 한 주도 못 샀으면 비종결 — E_CAPITAL 로 쉬었다가 같은 목표로 다시 산다 (시장가 진입과 같다).
func TestSweepEntryShortIsNotTerminal(t *testing.T) {
	t0 := kst("10:00:00")
	h := newSweepHarness(t, t0)
	h.fb.setBook(1000, 100, 995, 50)
	h.fb.failBuy = fmt.Errorf("%w: 키움 매수가능수량 0", broker.ErrInsufficient)
	h.apply(t, 1, t0, sweepOpen(t0, 1000, 30*time.Second, 35*time.Second, 80))
	h.x.Tick(ctx, t0.Add(time.Second))
	if h.x.HasWorking() || len(h.eng.Positions()) != 0 {
		t.Fatalf("돈이 없는데 sweep·대기 로트가 남았다 work=%v pos=%+v", h.x.work, h.eng.Positions())
	}
	if term, _ := h.st.TerminalIntents(ctx); len(term) != 0 {
		t.Fatalf("돈 부족을 종결로 처리했다 %v", term)
	}
	h.fb.failBuy = nil
	h.x.Tick(ctx, t0.Add(5*time.Second)) // 쉬는 시간(2초) 뒤 다시
	h.run(t0.Add(5500*time.Millisecond), t0.Add(6*time.Second))
	if l := h.lot(t); l.Pending || l.Qty != 10 {
		t.Fatalf("다시 사지 않았다 %+v", l)
	}
}

// paper — 호가 = 시세 한 값. 시세 ≤ cap 이면 IOC 가 cap 에 다 차고, 창이 끝나도 시세가 cap+slip 위면 잔량 포기.
func TestSweepPaper(t *testing.T) {
	h := newHarness(t) // paper · 시세 1000 고정
	until := base.Add(30 * time.Second)
	sign := func(seq uint64, capPx float64) {
		tgt := map[string]any{
			"intent_id": fmt.Sprintf("01J9Z8QK3M7X2ABCDEFGHJK%03d", seq), "slot": "a", "side": "long", "want": "open",
			"weight": 0.01, "symbol": map[string]any{"exchange": "KRX", "code": "005930"},
			"entry": map[string]any{"mode": "sweep", "not_after": base.Add(40 * time.Second), "ref_price": 1000,
				"cap_price": capPx, "sweep_until": until, "max_slip_bp": 80},
			"exit": map[string]any{"stop_price": 850},
		}
		m := map[string]any{"v": 1, "typ": "intent.target", "id": fmt.Sprintf("01J9Z8QK3M7X2ABCDEFGHJ%04d", seq),
			"acct": "acc_7f3a", "ts": base, "exp": base.Add(time.Minute), "seq": seq,
			"body": map[string]any{"as_of_bar": base, "book_state": "normal", "targets": []any{tgt}}}
		raw, _ := json.Marshal(m)
		signed, _ := protocol.Sign(raw, kid, h.pk)
		if ack := h.eng.Apply(signed, base.Add(time.Second)); ack.Status != "applied" {
			t.Fatalf("%+v", ack)
		}
	}

	sign(1, 1000) // 시세 1000 = cap → IOC 가 cap 에 다 찬다
	h.x.Tick(ctx, base.Add(2*time.Second))
	for now := base.Add(2500 * time.Millisecond); !now.After(base.Add(5 * time.Second)); now = now.Add(500 * time.Millisecond) {
		h.x.PollWorking(ctx, now)
	}
	rows := h.ledger(t)
	o := onlyRow(t, rows, "filled")
	if o.Qty != 10 || o.Price != 1000 || o.Exec == nil || o.Exec.IOCOrders != 1 || o.Exec.Ended != "all" {
		t.Fatalf("paper sweep %+v exec %+v", o, o.Exec)
	}

	sign(2, 990) // cap 990 < 시세 1000 · slip 80bp → 상한 997.9 < 1000 → 창 끝 포기
	for now := base.Add(6 * time.Second); !now.After(base.Add(31 * time.Second)); now = now.Add(500 * time.Millisecond) {
		h.x.Tick(ctx, now)
	}
	var rej []store.Order
	for _, o := range h.ledger(t) {
		if o.Phase == "rejected" {
			rej = append(rej, o)
		}
	}
	if len(rej) != 1 || rej[0].Exec == nil || rej[0].Exec.Ended != "slip" || h.x.HasWorking() {
		t.Fatalf("paper slip 포기 %+v work=%v", rej, h.x.work)
	}
}

// sweep 은 want=flat 전량 청산에만 — 분할매도(reduce)·derisk·stop·시간청산, 창이 지난 flat 은 바로 시장가.
func TestWantsExitSweepOnlyForFlat(t *testing.T) {
	t0 := kst("10:00:00")
	h := newSweepHarness(t, t0)
	pos := protocol.Position{IntentID: aid, Symbol: code, Qty: 10}
	ex := &protocol.ExitExec{Mode: "sweep", FloorPrice: 1000, SweepUntil: t0.Add(30 * time.Second)}
	if !h.x.wantsExitSweep(t0, pos, 10, "flat", ex) {
		t.Fatal("flat 전량인데 sweep 을 안 한다")
	}
	for name, ok := range map[string]bool{
		"reduce":     h.x.wantsExitSweep(t0, pos, 10, "reduce", ex),
		"derisk":     h.x.wantsExitSweep(t0, pos, 10, "derisk", ex),
		"분할":         h.x.wantsExitSweep(t0, pos, 4, "flat", ex),
		"창 지남":       h.x.wantsExitSweep(t0.Add(31*time.Second), pos, 10, "flat", ex),
		"exec 없음":    h.x.wantsExitSweep(t0, pos, 10, "flat", nil),
		"동시호가 15:25": h.x.wantsExitSweep(kst("15:25:00"), pos, 10, "flat", ex),
	} {
		if ok {
			t.Errorf("%s 인데 sweep 한다", name)
		}
	}
}
