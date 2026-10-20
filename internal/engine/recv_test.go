package engine

import (
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// ★ 같은 목표가 mark 갱신으로 몇 초마다 다시 와도 «처음 도착» 시각을 유지한다 — 집행 지연(도착 → 주문)의
// 기준점이다. flat 은 flat 이 처음 온 시각을 청산에 싣는다.
func TestRecvAtKeepsFirstArrival(t *testing.T) {
	const id = "01J9Z8QK3M7X2ABCDEFGHJKMA1"
	s := mkSigner("flat6-1", 3)
	e := multiEngine(t, nil, s)

	t1 := base.Add(time.Second)
	if ack := e.Apply(s.snap(t, 1, "kiwoom/x", tgt(id, "005930", "open")), t1); ack.Status != "applied" {
		t.Fatalf("ack=%+v", ack)
	}
	if ack := e.Apply(s.snap(t, 2, "kiwoom/x", tgt(id, "005930", "open")), base.Add(3*time.Second)); ack.Status != "applied" {
		t.Fatalf("ack=%+v", ack)
	}
	plan := e.Plan(base.Add(4 * time.Second))
	if len(plan.Enters) != 1 || !plan.Enters[0].RecvAt.Equal(t1) {
		t.Fatalf("진입 도착 시각 = 처음 도착(%v)이어야: %+v", t1, plan.Enters)
	}

	e.SetPositions([]protocol.Position{{IntentID: id, Kid: "flat6-1", Scope: "kiwoom/x", Slot: "main",
		Symbol: protocol.Symbol{Exchange: "KRX", Code: "005930"}, Qty: 10, AvgEntryPrice: 1000}})
	t3 := base.Add(10 * time.Second)
	if ack := e.Apply(s.snap(t, 3, "kiwoom/x", tgt(id, "005930", "flat")), t3); ack.Status != "applied" {
		t.Fatalf("ack=%+v", ack)
	}
	plan = e.Plan(base.Add(11 * time.Second))
	if len(plan.Exits) != 1 || !plan.Exits[0].RecvAt.Equal(t3) {
		t.Fatalf("청산 도착 시각 = flat 이 처음 온 시각(%v)이어야: %+v", t3, plan.Exits)
	}
}
