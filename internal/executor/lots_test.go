package executor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/paper"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// tpBroker — 실브로커처럼 TP 체결을 주문번호로만 알려준다 (paper 의 자체 정산은 가격으로 막아 둔다).
type tpBroker struct {
	*paper.Broker
	filled map[string]broker.LimitStatus
}

func (b *tpBroker) LimitStatus(_ context.Context, _ protocol.Symbol, id string) (broker.LimitStatus, error) {
	if st, ok := b.filled[id]; ok {
		return st, nil
	}
	return broker.LimitStatus{Open: true}, nil
}

func (h *harness) applyTargets(t *testing.T, seq uint64, tgts ...map[string]any) {
	t.Helper()
	arr := make([]any, len(tgts))
	for i, x := range tgts {
		arr[i] = x
	}
	m := map[string]any{
		"v": 1, "typ": "intent.target", "id": "01J9Z8QK3M7X2ABCDEFGHJKMNP",
		"acct": "acc_7f3a", "ts": base, "exp": base.Add(time.Minute), "nonce": "n1", "seq": seq,
		"body": map[string]any{"as_of_bar": base, "book_state": "normal", "targets": arr},
	}
	raw, _ := json.Marshal(m)
	signed, err := protocol.Sign(raw, kid, h.pk)
	if err != nil {
		t.Fatal(err)
	}
	h.apply(t, signed)
}

func lot(id string, ref, tp float64) map[string]any {
	ex := map[string]any{"stop_price": 500}
	if tp > 0 {
		ex["tp_price"], ex["tp_delegate"] = tp, true
	}
	return map[string]any{
		"intent_id": id, "slot": "main", "side": "long", "want": "open", "weight": 0.1,
		"symbol": map[string]any{"exchange": "KRX", "code": "005930"},
		"entry":  map[string]any{"mode": "market", "not_after": base.Add(time.Minute), "ref_price": ref},
		"exit":   ex,
	}
}

// ★ 같은 종목 로트 둘 — ① 진입가가 브로커 평단으로 섞이지 않고 ② 한 로트의 TP 체결을 알아챈다.
func TestTwoLotsSameSymbol(t *testing.T) {
	h := newHarness(t)
	price := 1000.0
	pb := paper.New(paper.Config{Cash: 10_000_000, Lot: 1, Now: func() time.Time { return base },
		Price: func(protocol.Symbol) (float64, bool) { return price, true }})
	tb := &tpBroker{Broker: pb, filled: map[string]broker.LimitStatus{}}
	h.br = pb
	h.rewire(tb)
	now := base.Add(time.Second)

	const A, B = "01J9Z8QK3M7X2ABCDEFGHJKMA1", "01J9Z8QK3M7X2ABCDEFGHJKMB1"
	h.applyTargets(t, 1, lot(A, 1000, 0))
	h.x.Tick(ctx, now)
	price = 1100
	h.applyTargets(t, 2, lot(A, 1000, 1200), lot(B, 1100, 0))
	h.x.Tick(ctx, now) // B 진입 + A 의 TP 위임
	h.x.Tick(ctx, now)

	lots := map[string]protocol.Position{}
	for _, p := range h.eng.Positions() {
		lots[p.IntentID] = p
	}
	if len(lots) != 2 || lots[A].TpOrderID == "" {
		t.Fatalf("로트 준비 실패: %+v", lots)
	}
	if lots[A].AvgEntryPrice != 1000 || lots[B].AvgEntryPrice != 1100 {
		t.Fatalf("★ 로트 진입가가 브로커 평단으로 섞였다: A=%v B=%v", lots[A].AvgEntryPrice, lots[B].AvgEntryPrice)
	}

	// 거래소에서 A 의 TP 가 체결됐다 — 계좌엔 B 가 남아 있어 "종목이 사라졌나" 로는 안 보인다.
	qa := lots[A].Qty
	pb.CancelOrder(ctx, lots[A].Symbol, lots[A].TpOrderID)
	if _, err := pb.Sell(ctx, broker.OrderRequest{Symbol: lots[A].Symbol, Qty: qa, LimitPrice: 1200}); err != nil {
		t.Fatal(err)
	}
	tb.filled[lots[A].TpOrderID] = broker.LimitStatus{FilledQty: qa, AvgPrice: 1200, FilledAt: now}

	res := h.x.Tick(ctx, now.Add(time.Minute))
	if res.Exited != 1 || len(res.Mismatch) != 0 {
		t.Fatalf("A 의 TP 체결을 못 봤다: %+v", res)
	}
	left := h.eng.Positions()
	if len(left) != 1 || left[0].IntentID != B || left[0].AvgEntryPrice != 1100 {
		t.Fatalf("남은 로트 %+v", left)
	}
	for _, o := range h.ledger(t) {
		if o.Phase == "exit_filled" {
			if o.IntentID != A || o.ExitReason != "tp" || o.Price != 1200 || o.RealizedPct < 0.19 {
				t.Fatalf("TP 원장 %+v", o)
			}
		}
	}
}

// ★ e2e 발견 재현: TP 가 체결돼 종목이 통째로 사라졌는데, 주문번호 확인(10초 간격)보다 "사라짐" 을
// 먼저 봤다 → 예전엔 체결가 미상으로 닫혀 TP 수익이 성과에서 빠졌다. 이제 사라짐 경로도 주문번호로 확인한다.
func TestVanishedLotWithTPGetsFillPrice(t *testing.T) {
	h := newHarness(t)
	price := 1000.0
	pb := paper.New(paper.Config{Cash: 10_000_000, Lot: 1, Now: func() time.Time { return base },
		Price: func(protocol.Symbol) (float64, bool) { return price, true }})
	tb := &tpBroker{Broker: pb, filled: map[string]broker.LimitStatus{}}
	h.br = pb
	h.rewire(tb)
	now := base.Add(time.Second)

	const A = "01J9Z8QK3M7X2ABCDEFGHJKMA1"
	h.applyTargets(t, 1, lot(A, 1000, 1200))
	h.x.Tick(ctx, now)
	h.x.Tick(ctx, now) // TP 위임 + 첫 주문번호 확인(미체결) → 10초간 다시 안 묻는다
	p := h.eng.Positions()[0]

	pb.CancelOrder(ctx, p.Symbol, p.TpOrderID)
	if _, err := pb.Sell(ctx, broker.OrderRequest{Symbol: p.Symbol, Qty: p.Qty, LimitPrice: 1200}); err != nil {
		t.Fatal(err)
	}
	tb.filled[p.TpOrderID] = broker.LimitStatus{FilledQty: p.Qty, AvgPrice: 1200, FilledAt: now}

	h.x.Tick(ctx, now.Add(2*time.Second)) // 10초 안 — 사라짐 경로가 먼저 본다
	for _, o := range h.ledger(t) {
		if o.Phase == "exit_filled" {
			if o.Price != 1200 || o.ExitReason != "tp" || o.Detail != "" {
				t.Fatalf("★ 체결가 미상으로 닫혔다: %+v", o)
			}
			return
		}
	}
	t.Fatal("청산 기록 없음")
}
