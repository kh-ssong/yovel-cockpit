package executor

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/paper"
	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
)

// restartHandsOff — 데몬 재시작 흉내 (restartPaper 와 같다) + 그 소스의 장부를 «꺼짐 · exits manual» 로.
func (h *harness) restartHandsOff(t *testing.T, price *float64) *harness {
	t.Helper()
	f := false
	books, err := book.New([]book.Book{{Name: "swing_etf", Kid: kid, Scope: "", Seed: 1_000_000,
		Enabled: &f, Exits: book.ExitsManual}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.New(engine.Config{
		Mode: protocol.ModePaper, Policy: h.eng.Policy(), TargetMaxAge: 180 * time.Second, MaxOrders: 5,
		Books:  books,
		Price:  func(protocol.Symbol) (float64, bool) { return *price, true },
		Market: func(protocol.Symbol) sizing.Market { return sizing.StockMarket() },
		Store:  h.st,
	}, base)
	if err := eng.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	br := paper.New(paper.Config{
		Cash: 1_000_000, Lot: 1, Now: func() time.Time { return base },
		Price: func(protocol.Symbol) (float64, bool) { return *price, true },
	})
	ps, err := PaperState(ctx, h.st, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	br.Restore(ps)
	h2 := &harness{st: h.st, eng: eng, br: br, pk: h.pk}
	h2.x = New(Deps{Broker: br, Store: h.st, Engine: eng, Mode: protocol.ModePaper, DaemonSHA: "abc1234",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Quote: func(_ context.Context, _ protocol.Symbol) (float64, time.Time, bool) {
			return *price, base.Add(time.Minute), true
		},
		MaxPriceAge: time.Hour,
	})
	return h2
}

// ★ 장부를 «꺼짐 · exits manual» 로 다시 띄우면 남은 로트는 stop·시간청산·발행자 flat 어느 것으로도
// 팔리지 않고 (user 2026-11-05 — 꺼 둔 swing_etf_60m 로트 둘이 stop 으로 팔렸다), 사용자가 HTS 로
// 팔면 지금처럼 «수동 매도» 로 닫힌다.
func TestHandsOffLotIsNeverSoldButManualSellCloses(t *testing.T) {
	h := newHarness(t)
	exitAt := base.Add(10 * time.Minute)
	h.openWith(t, 1, 850, exitAt)
	if res := h.x.Tick(ctx, base.Add(time.Second)); res.Entered != 1 {
		t.Fatalf("진입 %+v", res)
	}

	price := 800.0 // stop 850 아래
	h2 := h.restartHandsOff(t, &price)

	if res := h2.x.Tick(ctx, base.Add(2*time.Second)); res.Exited != 0 || len(res.Blind) != 0 {
		t.Fatalf("★ stop 아래인데 맡아 간 로트를 팔았거나 Blind 로 올렸다: %+v", res)
	}
	if res := h2.x.Tick(ctx, exitAt.Add(time.Second)); res.Exited != 0 {
		t.Fatalf("★ 시간청산으로 맡아 간 로트를 팔았다: %+v", res)
	}
	h2.applyTarget(t, 2, map[string]any{
		"intent_id": lxID, "slot": "d205", "symbol": lxSym, "side": "long", "want": "flat",
	})
	if res := h2.x.Tick(ctx, exitAt.Add(2*time.Second)); res.Exited != 0 {
		t.Fatalf("★ 발행자 flat 으로 맡아 간 로트를 팔았다: %+v", res)
	}
	if r := exitReasons(h2, t); len(r) != 0 {
		t.Fatalf("청산 기록이 생겼다: %v", r)
	}

	// 사용자가 HTS 로 전량 매도.
	held, _ := h2.br.Positions(ctx)
	if len(held) != 1 {
		t.Fatalf("보유 %+v", held)
	}
	if _, err := h2.br.Sell(ctx, broker.OrderRequest{Symbol: held[0].Symbol, Qty: held[0].Qty}); err != nil {
		t.Fatal(err)
	}
	if res := h2.x.Tick(ctx, exitAt.Add(3*time.Second)); res.ClosedByBroker != 1 {
		t.Fatalf("HTS 매도를 못 잡았다: %+v", res)
	}
	if r := exitReasons(h2, t); len(r) != 1 || r[0] != "manual" {
		t.Fatalf("수동 매도로 안 닫혔다: %v", r)
	}
	if ps := h2.eng.Positions(); len(ps) != 0 {
		t.Fatalf("로트가 남았다: %+v", ps)
	}
}
