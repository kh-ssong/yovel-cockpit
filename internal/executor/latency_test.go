package executor

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/kiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// spyBroker — 키움 드라이버를 그대로 쓰되 부른 순서를 남긴다 (지연 경로 검증용).
type spyBroker struct {
	*kiwoom.Broker
	mu    sync.Mutex
	calls []string
}

func (s *spyBroker) note(c string) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}

func (s *spyBroker) count(c string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, x := range s.calls {
		if x == c {
			n++
		}
	}
	return n
}

func (s *spyBroker) Positions(ctx context.Context) ([]broker.Holding, error) {
	s.note("positions")
	return s.Broker.Positions(ctx)
}

func (s *spyBroker) SubmitBuy(ctx context.Context, req broker.OrderRequest) (broker.Submitted, error) {
	s.note("buy:" + req.Scope)
	return s.Broker.SubmitBuy(ctx, req)
}

func (s *spyBroker) SubmitSell(ctx context.Context, req broker.OrderRequest) (broker.Submitted, error) {
	s.note("sell:" + req.Scope)
	return s.Broker.SubmitSell(ctx, req)
}

func (s *spyBroker) LimitStatus(ctx context.Context, sym protocol.Symbol, id string) (broker.LimitStatus, error) {
	s.note("status")
	return s.Broker.LimitStatus(ctx, sym, id)
}

// withSpy — 하네스의 집행기를 spy 브로커로 다시 만든다.
func withSpy(t *testing.T, h *kHarness) *spyBroker {
	t.Helper()
	kb, ok := h.x.d.Broker.(*kiwoom.Broker)
	if !ok {
		t.Fatalf("키움 하네스가 아니다: %T", h.x.d.Broker)
	}
	spy := &spyBroker{Broker: kb}
	d := h.x.d
	d.Broker = spy
	h.x = New(d)
	return spy
}

// signScope — scope 를 실은 봉투 (장부별 소스). seq 는 소스마다 따로 센다.
func (h *kHarness) signScope(t *testing.T, scope string, seq uint64, t0 time.Time, tgts ...map[string]any) {
	t.Helper()
	m := map[string]any{
		"v": 1, "typ": "intent.target", "id": "01J9Z8QK3M7X2ABCDEFGHJKMNP",
		"acct": "acc_7f3a", "ts": t0, "exp": t0.Add(time.Minute), "seq": seq,
		"body": map[string]any{"scope": scope, "as_of_bar": t0, "book_state": "normal", "targets": tgts},
	}
	raw, _ := json.Marshal(m)
	signed, err := protocol.Sign(raw, kid, h.pk)
	if err != nil {
		t.Fatal(err)
	}
	if ack := h.eng.Apply(signed, t0.Add(time.Second)); ack.Status != "applied" {
		t.Fatalf("목표 %s %+v", scope, ack)
	}
}

const bid = "01J9Z8QK3M7X2ABCDEFGHJKMB2"

// withIntent — 같은 모양의 목표를 다른 intent 로.
func withIntent(m map[string]any, id string) map[string]any {
	m["intent_id"] = id
	return m
}

// ★ 목표가 깨운 틱은 잔고(kt00005)를 묻지 않는다 — 주기 틱만 대조한다 (2026-10-20: 깨움마다 묻다 주문이 밀림).
func TestWokenTickSkipsPositionSync(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	spy := withSpy(t, h)

	h.x.TickWoken(ctx, t0.Add(time.Second))
	if n := spy.count("positions"); n != 0 {
		t.Fatalf("깨운 틱이 잔고를 %d번 물었다", n)
	}
	h.x.Tick(ctx, t0.Add(2*time.Second))
	if n := spy.count("positions"); n != 1 {
		t.Fatalf("주기 틱의 잔고 조회 %d번 (1번이어야)", n)
	}
}

// ★ 깨운 틱에서도 진입은 그대로 난다 — 대조만 건너뛴다.
func TestWokenTickStillEnters(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	spy := withSpy(t, h)
	h.sign(t, 1, t0, openT(t0, nil))

	res := h.x.TickWoken(ctx, t0.Add(time.Second))
	if res.Entered != 1 || len(h.eng.Positions()) != 1 {
		t.Fatalf("깨운 틱 진입 %+v", res)
	}
	if spy.count("positions") != 0 {
		t.Fatal("깨운 틱이 잔고를 물었다")
	}
}

// ★ 장부 둘이 같은 틱에 사면 둘 다 **먼저 내고** 체결은 나중에 묻는다 — 둘째가 첫째의 체결 확인을 기다리지 않게.
// 체결 확인은 여전히 같은 틱 안에서 끝난다 (Entered 2).
func TestTwoBooksSubmitBeforePolling(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	spy := withSpy(t, h)
	h.signScope(t, "kiwoom/a", 1, t0, openT(t0, nil))
	h.signScope(t, "kiwoom/b", 1, t0, withIntent(openT(t0, nil), bid))

	res := h.x.TickWoken(ctx, t0.Add(time.Second))
	if res.Entered != 2 {
		t.Fatalf("진입 %+v", res)
	}
	var order []string
	for _, c := range spy.calls {
		if c == "buy:kiwoom/a" || c == "buy:kiwoom/b" || c == "status" {
			order = append(order, c)
		}
	}
	if len(order) < 4 || order[0][:4] != "buy:" || order[1][:4] != "buy:" {
		t.Fatalf("두 매수가 체결 조회보다 먼저 나가야 한다: %v", order)
	}
}

// ★ 같은 계좌에서 두 장부가 같은 종목을 들고 있다가 한 장부만 청산 — 다른 장부 로트는 그대로,
// 대조 불일치도 «브로커에서 사라짐» 판정도 없어야 한다 (069500 을 push10x 둘 + oldhigh 가 같이 든다).
func TestOneBookExitLeavesSiblingLot(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	h.signScope(t, "kiwoom/a", 1, t0, openT(t0, nil))
	h.signScope(t, "kiwoom/b", 1, t0, withIntent(openT(t0, nil), bid))
	if res := h.x.Tick(ctx, t0.Add(time.Second)); res.Entered != 2 {
		t.Fatalf("진입 %+v", res)
	}
	var qtyB float64
	for _, p := range h.eng.Positions() {
		if p.Scope == "kiwoom/b" {
			qtyB = p.Qty
		}
	}

	t1 := t0.Add(10 * time.Second)
	flat := flatT()
	h.signScope(t, "kiwoom/a", 2, t1, flat)
	res := h.x.Tick(ctx, t1.Add(time.Second))
	if res.Exited != 1 || res.ClosedByBroker != 0 || len(res.Mismatch) != 0 {
		t.Fatalf("한 장부 청산 %+v", res)
	}
	// 대조를 한 번 더 — 남은 로트와 실물이 맞아야 한다.
	res = h.x.Tick(ctx, t1.Add(6*time.Second))
	if res.ClosedByBroker != 0 || len(res.Mismatch) != 0 {
		t.Fatalf("청산 뒤 대조 %+v", res)
	}
	pos := h.eng.Positions()
	if len(pos) != 1 || pos[0].Scope != "kiwoom/b" || pos[0].Qty != qtyB {
		t.Fatalf("형제 로트가 바뀌었다 (b %v주였음): %+v", qtyB, pos)
	}
	for _, o := range h.ledger(t) {
		if o.Phase == "exit_filled" && o.Scope != "kiwoom/a" {
			t.Fatalf("다른 장부에 청산이 기록됐다 %+v", o)
		}
	}
}
