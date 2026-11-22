package engine

import (
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
)

// handsOffEngine — 소스 (kid, "") 장부는 꺼짐 (manual 이면 + exits manual), (kid, "other") 는 보통 장부.
func handsOffEngine(t *testing.T, manual bool) (*Engine, []byte) {
	t.Helper()
	priv, pub := keys(t)
	p := protocol.DefaultPolicy()
	p.TrustedKeys = pub
	f := false
	exits := ""
	if manual {
		exits = book.ExitsManual
	}
	books, err := book.New([]book.Book{
		{Name: "etf", Kid: kid, Scope: "", Seed: 1_000_000, Enabled: &f, Exits: exits},
		{Name: "other", Kid: kid, Scope: "other", Seed: 1_000_000},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	e := New(Config{
		Mode: protocol.ModePaper, Policy: p, TargetMaxAge: 180 * time.Second, MaxOrders: 5,
		Books:  books,
		Price:  func(protocol.Symbol) (float64, bool) { return 1000, true },
		Market: func(protocol.Symbol) sizing.Market { return sizing.StockMarket() },
	}, base)
	e.SetPositions([]protocol.Position{
		{IntentID: "01J9Z8QK3M7X2ABCDEFGHJKMNQ", Kid: kid, Scope: "", Symbol: protocol.Symbol{Exchange: "KRX", Code: "005930"},
			Qty: 5, AvgEntryPrice: 1000, StopArmed: 900},
		{IntentID: "other-1", Kid: kid, Scope: "other", Symbol: protocol.Symbol{Exchange: "KRX", Code: "000660"},
			Qty: 5, AvgEntryPrice: 100},
	})
	m := targetMsg(1, base, base.Add(time.Minute), base.Add(time.Minute), base)
	tg := m["body"].(map[string]any)["targets"].([]any)[0].(map[string]any)
	tg["want"] = "flat"
	delete(tg, "entry")
	return e, signEnv(t, m, priv)
}

// ★ 발행자가 flat 을 보내도, derisk liquidate 가 와도 맡아 간 로트는 팔지 않는다.
func TestHandsOffBookIsNeverSoldByPlan(t *testing.T) {
	now := base.Add(time.Second)
	// 대조군 — exits 를 안 적은 꺼진 장부는 flat 에 판다 (꺼짐 = 진입만 막음).
	ctl, flat := handsOffEngine(t, false)
	ctl.Apply(flat, now)
	if plan := ctl.Plan(now); len(plan.Exits) != 1 || plan.Exits[0].Reason != "flat" {
		t.Fatalf("대조군: 꺼진 장부의 flat 이 청산을 안 냈다: %+v", plan.Exits)
	}

	e, flat := handsOffEngine(t, true)
	if !e.HandsOff(protocol.Position{Kid: kid, Scope: ""}) || e.HandsOff(protocol.Position{Kid: kid, Scope: "other"}) {
		t.Fatal("HandsOff 판정")
	}
	if ack := e.Apply(flat, now); ack.Status != "applied" {
		t.Fatalf("flat 목표 ack=%+v", ack)
	}
	if plan := e.Plan(now); len(plan.Exits) != 0 || len(plan.StopUpdates) != 0 || len(plan.TimeExitUpdates) != 0 {
		t.Fatalf("★ flat 으로 맡아 간 로트를 팔려 한다: %+v", plan)
	}

	e.Apply(deriskMsg("n1", "liquidate", base, base.Add(5*time.Minute)), now)
	plan := e.Plan(now)
	if len(plan.Exits) != 1 || plan.Exits[0].Position.IntentID != "other-1" || plan.Exits[0].Reason != "derisk" {
		t.Fatalf("liquidate 는 보통 장부 로트만 팔아야 한다: %+v", plan.Exits)
	}
}
