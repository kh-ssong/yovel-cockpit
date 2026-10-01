package executor

// 실브로커 경로 — 주문을 내고 바로 돌아오고, 체결은 진행 중 주문으로 추적한다.
// 콕핏의 **실제 키움 드라이버**를 스펙 기반 가짜 키움에 붙여 검증한다.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker/kiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/fakekiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
)

// 2026-10-01(목) KST
func kst(hm string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-10-01 "+hm, session.KST)
	return t.UTC()
}

type kHarness struct {
	*harness
	fk  *fakekiwoom.Server
	cal *session.Calendar
}

func newKiwoomHarness(t *testing.T, t0 time.Time) *kHarness {
	t.Helper()
	h := newHarness(t)
	// 엔진을 t0 기준으로 다시 만든다 (목표 나이 판정이 t0 를 본다).
	h.eng = engine.New(engine.Config{
		Mode: protocol.ModeLive, Policy: h.eng.Policy(), TargetMaxAge: 180 * time.Second, MaxOrders: 10,
		EngineBudget: 1_000_000,
		Price:        func(protocol.Symbol) (float64, bool) { return 1000, true },
		Market:       func(protocol.Symbol) sizing.Market { return sizing.StockMarket() },
		Store:        h.st,
	}, t0)

	spec, err := fakekiwoom.LoadSpec("../fakekiwoom/testdata/spec_subset.json")
	if err != nil {
		t.Fatal(err)
	}
	fk := fakekiwoom.New(fakekiwoom.Config{Spec: spec, Cash: 10_000_000, Prices: map[string]float64{"005930": 1000}})
	srv := httptest.NewServer(fk)
	t.Cleanup(srv.Close)
	kb, err := kiwoom.New(kiwoom.Config{AppKey: "fake", SecretKey: "fake", DataDir: t.TempDir(), APIURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cal, _ := session.NewCalendar(session.DefaultKRXHolidays)
	h.x = New(Deps{
		Broker: kb, Store: h.st, Engine: h.eng, Mode: protocol.ModeLive, DaemonSHA: "test",
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Session: cal, ExitCutoff: "15:15", BrokerExchange: "KRX",
	})
	return &kHarness{harness: h, fk: fk, cal: cal}
}

// sign — t0 기준 봉투로 목표를 적용한다.
func (h *kHarness) sign(t *testing.T, seq uint64, t0 time.Time, tgts ...map[string]any) {
	t.Helper()
	arr := make([]any, len(tgts))
	for i, x := range tgts {
		arr[i] = x
	}
	m := map[string]any{
		"v": 1, "typ": "intent.target", "id": "01J9Z8QK3M7X2ABCDEFGHJKMNP",
		"acct": "acc_7f3a", "ts": t0, "exp": t0.Add(time.Minute), "seq": seq,
		"body": map[string]any{"as_of_bar": t0, "book_state": "normal", "targets": arr},
	}
	raw, _ := json.Marshal(m)
	signed, err := protocol.Sign(raw, kid, h.pk)
	if err != nil {
		t.Fatal(err)
	}
	if ack := h.eng.Apply(signed, t0.Add(time.Second)); ack.Status != "applied" {
		t.Fatalf("목표 %+v", ack)
	}
}

const aid = "01J9Z8QK3M7X2ABCDEFGHJKMA1"

func openT(t0 time.Time, timeExit *time.Time) map[string]any {
	ex := map[string]any{"stop_price": 500}
	if timeExit != nil {
		ex["time_exit_at"] = *timeExit
	}
	return map[string]any{
		"intent_id": aid, "slot": "main", "side": "long", "want": "open", "weight": 0.1,
		"symbol": map[string]any{"exchange": "KRX", "code": "005930"},
		"entry":  map[string]any{"mode": "market", "not_after": t0.Add(time.Minute), "ref_price": 1000},
		"exit":   ex,
	}
}

func flatT() map[string]any {
	return map[string]any{"intent_id": aid, "slot": "main", "side": "long", "want": "flat",
		"symbol": map[string]any{"exchange": "KRX", "code": "005930"}}
}

func (h *kHarness) lot(t *testing.T) protocol.Position {
	t.Helper()
	ps := h.eng.Positions()
	if len(ps) != 1 {
		t.Fatalf("로트 %d 개: %+v", len(ps), ps)
	}
	return ps[0]
}

// ★ 체결이 늦어도 루프는 멈추지 않는다 — 접수 즉시 돌아오고, 진행 중 주문으로 확정한다.
func TestAsyncEntryReturnsImmediatelyAndFinalizesLater(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	h.fk.SetScenario(fakekiwoom.Scenario{Fill: "stall", StallFrac: 0}) // 체결이 안 온다 (VI 같은)
	h.sign(t, 1, t0, openT(t0, nil))

	start := time.Now()
	res := h.x.Tick(ctx, t0.Add(time.Second))
	if time.Since(start) > 3*time.Second || res.Entered != 0 || !h.x.HasWorking() {
		t.Fatalf("체결을 기다리며 막혔거나 추적을 안 한다: %+v (%v)", res, time.Since(start))
	}
	if l := h.lot(t); !l.Pending || l.Qty != 100 {
		t.Fatalf("대기 로트 %+v", l)
	}
	// 다음 틱에 같은 목표로 또 사지 않는다.
	h.x.Tick(ctx, t0.Add(2*time.Second))
	if n := len(h.fk.State().Orders); n != 1 {
		t.Fatalf("주문 %d 건 — 대기 중에 또 샀다", n)
	}

	h.fk.Match() // VI 해제 — 단일가 체결
	res = h.x.PollWorking(ctx, t0.Add(30*time.Second))
	if res.Entered != 1 || h.x.HasWorking() {
		t.Fatalf("체결 확정 안 됨: %+v", res)
	}
	if l := h.lot(t); l.Pending || l.Qty != 100 || l.EntryQty != 100 {
		t.Fatalf("확정 로트 %+v", l)
	}
}

// ★ 15:19 에 낸 청산이 장마감 동시호가에 걸려도 **취소하지 않고** 15:30 단일가 체결을 기다린다.
func TestExitInClosingAuctionIsNotCancelled(t *testing.T) {
	t0 := kst("15:10:00")
	h := newKiwoomHarness(t, t0)
	h.sign(t, 1, t0, openT(t0, nil))
	h.x.Tick(ctx, t0.Add(time.Second))
	if l := h.lot(t); l.Pending {
		t.Fatalf("진입 확정 안 됨 %+v", l)
	}

	h.fk.SetScenario(fakekiwoom.Scenario{Fill: "stall", StallFrac: 0})
	h.sign(t, 2, kst("15:19:00"), flatT())
	h.x.Tick(ctx, kst("15:19:01"))
	for _, now := range []string{"15:21:00", "15:25:00", "15:29:50"} {
		h.x.PollWorking(ctx, kst(now))
	}
	for _, o := range h.fk.State().Orders {
		if o.Status == "cancelled" {
			t.Fatalf("★ 동시호가 중에 청산을 취소했다: %+v", o)
		}
	}
	h.fk.Match() // 15:30 단일가
	res := h.x.PollWorking(ctx, kst("15:30:05"))
	if res.Exited != 1 || len(h.eng.Positions()) != 0 {
		t.Fatalf("15:30 체결 확정 안 됨: %+v", res)
	}
}

// 진입은 늦게 차면 다른 가격이다 — 마감을 넘으면 잔량 취소, 찬 만큼만 로트.
func TestEntryDeadlineCancelsRemainder(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	h.fk.SetScenario(fakekiwoom.Scenario{Fill: "stall", StallFrac: 0.5})
	h.sign(t, 1, t0, openT(t0, nil))
	h.x.Tick(ctx, t0.Add(time.Second))
	h.x.PollWorking(ctx, t0.Add(30*time.Second))
	if !h.x.HasWorking() {
		t.Fatal("마감 전에 끝냈다")
	}
	res := h.x.PollWorking(ctx, t0.Add(2*time.Minute))
	if res.Entered != 1 || h.x.HasWorking() {
		t.Fatalf("%+v", res)
	}
	if l := h.lot(t); l.Qty != 50 || l.Pending {
		t.Fatalf("부분 로트 %+v", l)
	}
	st := h.fk.State()
	if st.Orders[0].Status != "cancelled" {
		t.Fatalf("잔량이 살아 있다 %+v", st.Orders)
	}
}

// 하나도 안 찼으면 진입 포기 — 목표를 종결해 같은 신호로 늦게 다시 사지 않는다.
func TestEntryUnfilledGivesUp(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	h.fk.SetScenario(fakekiwoom.Scenario{Fill: "stall", StallFrac: 0})
	h.sign(t, 1, t0, openT(t0, nil))
	h.x.Tick(ctx, t0.Add(time.Second))
	h.x.PollWorking(ctx, t0.Add(2*time.Minute))
	if len(h.eng.Positions()) != 0 {
		t.Fatalf("미체결 진입이 로트로 남았다 %+v", h.eng.Positions())
	}
	h.x.Tick(ctx, t0.Add(2*time.Minute+time.Second))
	if n := len(h.fk.State().Orders); n != 1 {
		t.Fatalf("포기한 진입을 다시 냈다 (주문 %d)", n)
	}
}

// ★ 체결 대기 중에 데몬이 죽어도, 재시작한 콕핏이 그 주문을 이어서 추적한다 — 같은 목표로 또 사지 않는다.
func TestRestartResumesWorkingOrder(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	h.fk.SetScenario(fakekiwoom.Scenario{Fill: "stall", StallFrac: 0})
	h.sign(t, 1, t0, openT(t0, nil))
	h.x.Tick(ctx, t0.Add(time.Second))

	// "재시작" — 새 엔진·새 집행기, 같은 원장·같은 브로커.
	h2 := &kHarness{harness: &harness{st: h.st, pk: h.pk}, fk: h.fk, cal: h.cal}
	h2.eng = engine.New(engine.Config{
		Mode: protocol.ModeLive, Policy: h.eng.Policy(), TargetMaxAge: 180 * time.Second, MaxOrders: 10,
		EngineBudget: 1_000_000,
		Price:        func(protocol.Symbol) (float64, bool) { return 1000, true },
		Market:       func(protocol.Symbol) sizing.Market { return sizing.StockMarket() },
		Store:        h.st,
	}, t0)
	if err := h2.eng.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	h2.x = New(Deps{Broker: h.x.d.Broker, Store: h.st, Engine: h2.eng, Mode: protocol.ModeLive,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Session: h.cal, BrokerExchange: "KRX"})
	h2.sign(t, 2, t0.Add(5*time.Second), openT(t0, nil)) // 같은 목표가 재발행된다

	h2.x.Tick(ctx, t0.Add(6*time.Second))
	if n := len(h.fk.State().Orders); n != 1 {
		t.Fatalf("★ 재시작 뒤 같은 목표로 또 샀다 (주문 %d)", n)
	}
	h.fk.Match()
	if res := h2.x.PollWorking(ctx, t0.Add(10*time.Second)); res.Entered != 1 {
		t.Fatalf("이어받은 주문을 확정 못 했다 %+v", res)
	}
}

// 장 밖엔 KRX 주문을 내지 않는다 — 밤새 거부만 반복하지 않는다.
func TestNoKRXOrdersOutsideSession(t *testing.T) {
	t0 := kst("15:00:00")
	h := newKiwoomHarness(t, t0)
	h.sign(t, 1, t0, openT(t0, nil))
	h.x.Tick(ctx, t0.Add(time.Second))
	night := kst("21:00:00")
	h.sign(t, 2, night, flatT())
	res := h.x.Tick(ctx, night.Add(time.Second))
	if res.Deferred != 1 || len(h.fk.State().Orders) != 1 {
		t.Fatalf("장 밖에 청산을 냈다: %+v %+v", res, h.fk.State().Orders)
	}
	// 다음 날 08:59 — 장전 동시호가로 낸다.
	next := time.Date(2026, 10, 2, 8, 59, 10, 0, session.KST).UTC()
	h.sign(t, 3, next, flatT())
	h.x.Tick(ctx, next.Add(time.Second))
	if n := len(h.fk.State().Orders); n != 2 {
		t.Fatalf("08:59 청산이 안 나갔다 (주문 %d)", n)
	}
}

// 시간청산이 15:20(동시호가)로 잡혀 와도 15:15 에 장중 매도한다.
func TestTimeExitPulledToCutoff(t *testing.T) {
	t0 := kst("15:00:00")
	h := newKiwoomHarness(t, t0)
	te := kst("15:20:00")
	h.sign(t, 1, t0, openT(t0, &te))
	h.x.Tick(ctx, t0.Add(time.Second))
	if res := h.x.Tick(ctx, kst("15:14:50")); res.Exited != 0 {
		t.Fatal("15:15 전에 팔았다")
	}
	if res := h.x.Tick(ctx, kst("15:15:00")); res.Exited != 1 {
		t.Fatalf("15:15 장중 매도가 안 됐다: %+v", res)
	}
}
