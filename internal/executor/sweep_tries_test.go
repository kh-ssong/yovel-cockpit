package executor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
)

// scriptBroker — n 번째 IOC 마다 정해 둔 일이 일어난다 (폴링 위상·시각과 무관하게).
//   - vanish: 호가를 읽은 뒤 주문이 닿기 전에 최우선 호가가 한 틱 물러난다 (11/05 c23: IOC 8번 중 1번 체결) — 주문 뒤엔 돌아온다.
//   - fail: 거부.
//   - after: 주문 뒤 호가를 바꾼다.
type scriptBroker struct {
	*sweepBroker
	n      int
	script func(n int) (vanish bool, fail error, after func())
}

func (b *scriptBroker) run(side string, req broker.OrderRequest) (broker.Submitted, error) {
	if !req.IOC || b.script == nil {
		return b.sweepBroker.submit(side, req)
	}
	b.n++
	vanish, fail, after := b.script(b.n)
	if fail != nil {
		return broker.Submitted{SubmittedAt: time.Now()}, fail
	}
	var saved broker.Book
	if vanish {
		b.mu.Lock()
		saved = b.book
		if side == "buy" {
			b.book.Ask1 += 5
		} else {
			b.book.Bid1 -= 5
		}
		b.mu.Unlock()
	}
	s, err := b.sweepBroker.submit(side, req)
	if vanish {
		b.mu.Lock()
		b.book = saved
		b.mu.Unlock()
	}
	if after != nil {
		after()
	}
	return s, err
}

func (b *scriptBroker) SubmitBuy(_ context.Context, req broker.OrderRequest) (broker.Submitted, error) {
	return b.run("buy", req)
}
func (b *scriptBroker) SubmitSell(_ context.Context, req broker.OrderRequest) (broker.Submitted, error) {
	return b.run("sell", req)
}

func (b *scriptBroker) setBookAt(ask, askQty, bid, bidQty float64, asOf time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.book = broker.Book{Ask1: ask, Ask1Qty: askQty, Bid1: bid, Bid1Qty: bidQty, AsOf: asOf}
}

func newScriptHarness(t *testing.T, t0 time.Time) (*sweepHarness, *scriptBroker, *bytes.Buffer) {
	t.Helper()
	h := newSweepHarness(t, t0)
	sb := &scriptBroker{sweepBroker: h.fb}
	var logs bytes.Buffer
	cal, _ := session.NewCalendar(session.DefaultKRXHolidays)
	h.x = New(Deps{
		Broker: sb, Store: h.st, Engine: h.eng, Mode: protocol.ModeLive, DaemonSHA: "test",
		Log:     slog.New(slog.NewJSONHandler(&logs, nil)),
		Session: cal, ExitCutoff: "15:15", BrokerExchange: "KRX",
	})
	return h, sb, &logs
}

func checkTimes(t *testing.T, i int, tr protocol.SweepTry) {
	t.Helper()
	b := tr.Book
	if b == nil || b.ReqAt.IsZero() || b.RecvAt.Before(b.ReqAt) || tr.SentAt.Before(b.RecvAt) {
		t.Fatalf("조각 %d 시각 순서 (조회 → 받음 → 보냄): %+v book %+v", i, tr, b)
	}
	if tr.Error == "" && (tr.AckAt == nil || tr.AckAt.Before(tr.SentAt)) {
		t.Fatalf("조각 %d 주문번호 받은 시각: %+v", i, tr)
	}
}

// ★ 조각마다 «그때 본 호가 · 그 호가의 증권사 시각 · 조회/보냄/접수 시각 · 체결» 이 원장 exec.tries 와 로그에 남는다 —
// 0 체결 IOC 와 거부된 IOC 까지. 집행은 그대로다 (같은 수량 · 같은 평단).
func TestSweepTriesRecordWhatEachSliceSaw(t *testing.T) {
	t0 := kst("10:00:00")
	h, sb, logs := newScriptHarness(t, t0)
	asOf := t0.Add(-300 * time.Millisecond).UTC()
	moved := asOf.Add(5 * time.Second)
	sb.setBookAt(1000, 3, 995, 50, asOf) // 매도1 1000 (= cap) 에 3주
	sb.script = func(n int) (bool, error, func()) {
		switch n {
		case 1:
			return true, nil, nil // 닿기 전에 사라진다 → 0 체결
		case 2:
			return false, errors.New("가격제한 거부"), nil
		case 3: // 3주 체결 — 그 뒤 매도1 이 cap 위로 (창 끝까지)
			return false, nil, func() { sb.setBookAt(1005, 100, 1000, 50, moved) }
		}
		t.Errorf("IOC 가 %d 번째 — 3번까지만 나가야 한다", n)
		return false, nil, nil
	}
	h.apply(t, 1, t0, sweepOpen(t0, 1000, 30*time.Second, 35*time.Second, 80))
	h.x.Tick(ctx, t0.Add(time.Second))
	h.run(t0.Add(1500*time.Millisecond), t0.Add(32*time.Second)) // 창 끝 — 잔량 7주 시장가 @1005

	if h.x.HasWorking() {
		t.Fatalf("sweep 이 안 끝났다: %+v", h.x.work)
	}
	o := onlyRow(t, h.ledger(t), "filled")
	e := o.Exec
	if o.Qty != 10 || e == nil || e.IOCOrders != 2 || e.RestQty != 7 || e.RestPrice != 1005 {
		t.Fatalf("집행이 달라졌다: %+v exec %+v", o, e)
	}
	tr := e.Tries
	if len(tr) != 4 {
		t.Fatalf("조각 기록 %d 개 (IOC 0체결 · 거부 · IOC 체결 · 잔량): %+v", len(tr), tr)
	}
	for i, x := range tr {
		checkTimes(t, i, x)
	}
	// ① 0 체결 IOC — 그 IOC 를 내게 한 호가 (1000 · 3주 · 증권사 시각) 가 남는다.
	if a := tr[0]; a.Kind != "ioc" || a.OrderID == "" || a.Price != 1000 || a.Qty != 3 || a.FilledQty != 0 ||
		a.Book.Ask1 != 1000 || a.Book.Ask1Qty != 3 || a.Book.Bid1 != 995 || !a.Book.AsOf.Equal(asOf) || a.FilledAt != nil {
		t.Fatalf("0 체결 IOC 기록 %+v book %+v", a, a.Book)
	}
	// ② 거부 — 주문번호 없이 사유가 남는다.
	if b := tr[1]; b.Kind != "ioc" || b.OrderID != "" || !strings.Contains(b.Error, "가격제한") || b.AckAt != nil {
		t.Fatalf("거부 기록 %+v", b)
	}
	// ③ 체결 IOC.
	if c := tr[2]; c.Kind != "ioc" || c.FilledQty != 3 || c.FillPrice != 1000 || c.OrderID == "" || c.Book.Ask1 != 1000 {
		t.Fatalf("체결 IOC 기록 %+v", c)
	}
	// ④ 창 끝 시장가 — 그 순간 호가 (매수 max_slip_bp 확인으로 새로 읽은 1005).
	if d := tr[3]; d.Kind != "rest" || d.Price != 0 || d.Qty != 7 || d.FilledQty != 7 || d.FillPrice != 1005 ||
		d.Book.Ask1 != 1005 || !d.Book.AsOf.Equal(moved) {
		t.Fatalf("잔량 기록 %+v book %+v", d, d.Book)
	}
	if n := strings.Count(logs.String(), `"msg":"sweep 조각"`); n != 4 {
		t.Fatalf("로그 «sweep 조각» %d 줄", n)
	}
	if !strings.Contains(logs.String(), `"book_to_send_ms"`) {
		t.Fatal("로그에 호가→보냄 간격이 없다")
	}
}

// 청산 — 창 끝 잔량의 호가는 창 안 마지막 조회다 (새로 읽지 않는다 · 매도 잔량을 늦추지 않는다).
func TestSweepExitTriesRestUsesLastBook(t *testing.T) {
	t0 := kst("10:00:00")
	h, sb, _ := newScriptHarness(t, t0)
	h.holdLot(t, t0)

	t1 := kst("10:05:00")
	moved := t1.Add(20 * time.Second).UTC()
	sb.setBookAt(1015, 100, 1010, 4, t1.UTC()) // 매수1 1010 = floor 에 4주
	sb.script = func(n int) (bool, error, func()) {
		if n > 1 {
			t.Errorf("IOC 가 %d 번째", n)
		}
		return false, nil, func() { sb.setBookAt(1010, 100, 1005, 100, moved) } // 그 뒤 floor 아래
	}
	h.apply(t, 2, t1, sweepFlat(t1.Add(30*time.Second), 1010))
	h.x.Tick(ctx, t1.Add(time.Second))
	h.run(t1.Add(1500*time.Millisecond), t1.Add(31*time.Second))

	o := onlyRow(t, h.ledger(t), "exit_filled")
	tr := o.Exec.Tries
	if len(tr) != 2 || tr[0].Kind != "ioc" || tr[0].FilledQty != 4 || tr[0].Book.Bid1 != 1010 || tr[0].Book.Bid1Qty != 4 {
		t.Fatalf("청산 조각 기록 %+v", tr)
	}
	r := tr[1]
	if r.Kind != "rest" || r.Qty != 6 || r.FilledQty != 6 || r.Book == nil || r.Book.Bid1 != 1005 || !r.Book.AsOf.Equal(moved) {
		t.Fatalf("청산 잔량 기록 %+v book %+v", r, r.Book)
	}
	checkTimes(t, 1, r)
}
