package executor

// 재시작 복구 — 데몬이 아무 데서나 죽고 다시 떠도 장부와 실물이 어긋나지 않는다 (2026-10-15 점검).

import (
	"context"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/kiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/paper"
	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/fakekiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
)

// restartPaper — 데몬 재시작 흉내: 새 엔진(원장 복구) · **새 paper 계좌**(원장에서 복원) · 새 집행기.
func (h *harness) restartPaper(t *testing.T, price func(protocol.Symbol) (float64, bool)) *harness {
	t.Helper()
	eng := engine.New(engine.Config{
		Mode: protocol.ModePaper, Policy: h.eng.Policy(), TargetMaxAge: 180 * time.Second, MaxOrders: 5,
		EngineBudget: 1_000_000,
		Price:        func(protocol.Symbol) (float64, bool) { return 1000, true },
		Market:       func(protocol.Symbol) sizing.Market { return sizing.StockMarket() },
		Store:        h.st,
	}, base)
	if err := eng.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	br := paper.New(paper.Config{
		Cash: 1_000_000, FeeBpBuy: 1.5, FeeBpSell: 21.5, SlipBp: 10, Lot: 1,
		Now: func() time.Time { return base }, Price: price,
	})
	ps, err := PaperState(ctx, h.st, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	br.Restore(ps)
	h2 := &harness{st: h.st, eng: eng, br: br, pk: h.pk}
	h2.x = New(Deps{Broker: br, Store: h.st, Engine: eng, Mode: protocol.ModePaper, DaemonSHA: "abc1234",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return h2
}

// ★ paper 를 껐다 켜도 로트·예수금·걸어 둔 TP 가 그대로다. 예전엔 paper 계좌가 메모리라 재시작하면
// 열린 로트가 "콕핏 밖 매도·가격 미상" 으로 닫히고 예수금이 시드로 돌아갔다 (2026-10-15 실측).
func TestPaperRestartKeepsLotsCashAndTP(t *testing.T) {
	h := newHarness(t)
	h.apply(t, h.target(t, 1, protocol.WantOpen, 900, 1100))
	if res := h.x.Tick(ctx, base.Add(time.Second)); res.Entered != 1 {
		t.Fatalf("%+v", res)
	}
	if res := h.x.Tick(ctx, base.Add(2*time.Second)); res.TpPlaced != 1 {
		t.Fatalf("TP 가 안 걸렸다 %+v", res)
	}
	cashBefore, _ := h.br.Cash(ctx)
	holdBefore, _ := h.br.Positions(ctx)

	price := 1000.0
	h2 := h.restartPaper(t, func(protocol.Symbol) (float64, bool) { return price, true })

	cash, _ := h2.br.Cash(ctx)
	hold, _ := h2.br.Positions(ctx)
	if math.Abs(cash.Deposit-cashBefore.Deposit) > 1e-6 {
		t.Fatalf("★ 예수금이 복원되지 않았다: %v → %v", cashBefore.Deposit, cash.Deposit)
	}
	if len(hold) != 1 || hold[0].Qty != holdBefore[0].Qty || math.Abs(hold[0].AvgPrice-holdBefore[0].AvgPrice) > 1e-9 {
		t.Fatalf("보유가 복원되지 않았다: %+v → %+v", holdBefore, hold)
	}
	if h2.br.OpenTPOrders() != 1 || hold[0].Sellable != 0 {
		t.Fatalf("걸어 둔 TP 가 복원되지 않았다 (주문 %d, 매도가능 %v)", h2.br.OpenTPOrders(), hold[0].Sellable)
	}

	res := h2.x.Tick(ctx, base.Add(3*time.Second))
	if res.ClosedByBroker != 0 || len(h2.eng.Positions()) != 1 {
		t.Fatalf("★ 재시작 뒤 로트가 '사라짐' 으로 닫혔다: %+v", res)
	}
	// TP 가 원래 주문번호 그대로 살아 있다 — 가격이 닿으면 TP 로 닫힌다.
	price = 1100
	h2.x.Tick(ctx, base.Add(4*time.Second))
	if r := exitReasons(h2, t); len(r) != 1 || r[0] != "tp" {
		t.Fatalf("복원된 TP 로 닫히지 않았다: %v", r)
	}
}

// ★ paper 로 연 로트가 남은 원장으로 live 를 켜면 거부한다 (실측: paper 500주가 live 계좌 900주 중 500주를 팔았다).
func TestPaperLotsRefuseLiveStart(t *testing.T) {
	h := newHarness(t)
	h.apply(t, h.target(t, 1, protocol.WantOpen, 900, 0))
	h.x.Tick(ctx, base.Add(time.Second))
	live := engine.New(engine.Config{Mode: protocol.ModeLive, Policy: h.eng.Policy(), Store: h.st}, base)
	if err := live.Restore(ctx); err == nil || !strings.Contains(err.Error(), "paper") {
		t.Fatalf("★ paper 로트를 live 가 이어받았다: %v", err)
	}
}

// crashAfterSubmit — 브로커가 주문을 **받은 직후** 데몬이 죽는다 (접수 응답을 원장에 남기기 전).
type crashAfterSubmit struct {
	*kiwoom.Broker
	buy, sell bool
}

func (c crashAfterSubmit) SubmitBuy(ctx context.Context, req broker.OrderRequest) (broker.Submitted, error) {
	s, err := c.Broker.SubmitBuy(ctx, req)
	if c.buy && err == nil {
		panic("crash")
	}
	return s, err
}

func (c crashAfterSubmit) SubmitSell(ctx context.Context, req broker.OrderRequest) (broker.Submitted, error) {
	s, err := c.Broker.SubmitSell(ctx, req)
	if c.sell && err == nil {
		panic("crash")
	}
	return s, err
}

func crashTick(t *testing.T, x *Executor, now time.Time) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("죽지 않았다")
		}
	}()
	x.Tick(ctx, now)
}

// restart — 데몬 재시작: 새 엔진(원장 복구) · 새 집행기. 원장·가짜 키움·시계는 그대로.
func (h *kHarness) restart(t *testing.T, br broker.Broker, at time.Time) *kHarness {
	t.Helper()
	eng := engine.New(engine.Config{
		Mode: protocol.ModeLive, Policy: h.eng.Policy(), TargetMaxAge: 180 * time.Second, MaxOrders: 10,
		EngineBudget: 1_000_000,
		Price:        func(protocol.Symbol) (float64, bool) { return 1000, true },
		Market:       func(protocol.Symbol) sizing.Market { return sizing.StockMarket() },
		Store:        h.st,
	}, at)
	if err := eng.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	h2 := &kHarness{harness: &harness{st: h.st, pk: h.pk, eng: eng}, fk: h.fk, cal: h.cal, clock: h.clock}
	h2.x = New(Deps{Broker: br, Store: h.st, Engine: eng, Mode: protocol.ModeLive,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Session: h.cal, ExitCutoff: "15:15", BrokerExchange: "KRX"})
	return h2
}

func hasErr(res Result, sub string) bool {
	for _, e := range res.Errors {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}

// ★ 매수가 브로커에 들어간 직후(접수 응답 기록 전) 죽어도, 재시작한 콕핏은 같은 목표로 **또 사지 않는다**.
// 예전엔 보내기 전 기록이 없어 목표가 그대로였다 → 재시작 첫 틱에 한 번 더 샀다.
func TestCrashMidSubmitBuyDoesNotBuyAgain(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	kb := h.x.d.Broker.(*kiwoom.Broker)
	h.x.d.Broker = crashAfterSubmit{Broker: kb, buy: true}
	h.sign(t, 1, t0, openT(t0, nil))
	crashTick(t, h.x, t0.Add(time.Second))
	if n := len(h.fk.State().Orders); n != 1 {
		t.Fatalf("매수가 브로커에 안 갔다 (주문 %d)", n)
	}

	h2 := h.restart(t, kb, t0.Add(5*time.Second))
	h2.sign(t, 1, t0.Add(5*time.Second), openT(t0, nil)) // 재접속 — 같은 목표가 그대로 다시 온다
	res := h2.x.Tick(ctx, t0.Add(6*time.Second))
	if n := len(h.fk.State().Orders); n != 1 {
		t.Fatalf("★ 재시작 뒤 같은 목표로 또 샀다 (주문 %d)", n)
	}
	if !hasErr(res, "매수 결과 미상") || len(h2.eng.Positions()) != 0 {
		t.Fatalf("결과 미상으로 알리지 않았다: %+v / %+v", res, h2.eng.Positions())
	}
	var rejected bool
	for _, o := range h2.ledger(t) {
		rejected = rejected || (o.Phase == "rejected" && strings.Contains(o.Detail, "재시작"))
	}
	if !rejected {
		t.Fatalf("원장에 결과 미상이 없다 %+v", h2.ledger(t))
	}
	if ws, _ := h.st.OpenWorking(ctx); len(ws) != 0 {
		t.Fatalf("보내기 전 기록이 열린 채 남았다 %+v", ws)
	}
	// 다음 틱에도 안 산다 (목표가 종결됐다).
	h2.x.Tick(ctx, t0.Add(10*time.Second))
	if n := len(h.fk.State().Orders); n != 1 {
		t.Fatalf("다음 틱에 또 샀다 (주문 %d)", n)
	}
}

// ★ 매도가 브로커에 들어간 직후 죽어도, 재시작한 콕핏은 곧바로 또 팔지 않는다 — 같은 종목에 다른 로트가
// 있으면 그 주식을 판다. 쉬는 사이 체결되면 콕핏 밖 매도로 귀속돼 체결가로 닫힌다.
func TestCrashMidSubmitSellDoesNotSellAgain(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	kb := h.x.d.Broker.(*kiwoom.Broker)
	h.sign(t, 1, t0, openT(t0, nil))
	h.x.Tick(ctx, t0.Add(time.Second))
	h.lot(t)

	h.fk.SetScenario(fakekiwoom.Scenario{Fill: "stall", StallFrac: 0}) // 매도는 접수만 되고 안 찬다
	h.x.d.Broker = crashAfterSubmit{Broker: kb, sell: true}
	h.sign(t, 2, t0.Add(10*time.Second), flatT())
	crashTick(t, h.x, t0.Add(11*time.Second))
	if n := len(h.fk.State().Orders); n != 2 {
		t.Fatalf("매도가 브로커에 안 갔다 (주문 %d)", n)
	}

	h2 := h.restart(t, kb, t0.Add(15*time.Second))
	h2.sign(t, 2, t0.Add(15*time.Second), flatT())
	res := h2.x.Tick(ctx, t0.Add(16*time.Second))
	if n := len(h.fk.State().Orders); n != 2 {
		t.Fatalf("★ 재시작 뒤 곧바로 또 팔았다 (주문 %d)", n)
	}
	if !hasErr(res, "매도 결과 미상") || len(h2.eng.Positions()) != 1 {
		t.Fatalf("결과 미상으로 알리지 않았다: %+v", res)
	}

	// 쉬는 사이 그 매도가 체결된다 → 보유가 사라져 콕핏 밖 매도로 귀속, 체결가로 닫힌다.
	h.fk.Match()
	h2.x.Tick(ctx, t0.Add(30*time.Second))
	if len(h2.eng.Positions()) != 0 {
		t.Fatalf("체결된 매도를 못 봤다 %+v", h2.eng.Positions())
	}
	var closed bool
	for _, o := range h2.ledger(t) {
		closed = closed || (o.Phase == "exit_filled" && o.Price > 0)
	}
	if !closed || len(h.fk.State().Orders) != 2 {
		t.Fatalf("체결가로 닫히지 않았거나 또 팔았다: %+v", h2.ledger(t))
	}
}
