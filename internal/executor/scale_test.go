package executor

import (
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

func scaleTarget(hold *float64, tp float64) map[string]any {
	ex := map[string]any{"stop_price": 500}
	if hold != nil {
		ex["hold_frac"] = *hold
	}
	if tp > 0 {
		ex["tp_price"], ex["tp_delegate"] = tp, true
	}
	return map[string]any{
		"intent_id": "01J9Z8QK3M7X2ABCDEFGHJKMS1", "group": "d205-005930", "slot": "main",
		"side": "long", "want": "open", "weight": 0.5,
		"symbol": map[string]any{"exchange": "KRX", "code": "005930"},
		"entry":  map[string]any{"mode": "market", "not_after": base.Add(time.Minute), "ref_price": 1000},
		"exit":   ex,
	}
}

func f64(v float64) *float64 { return &v }

// 분할매도 한 바퀴: 50% → (같은 스냅샷 반복) → 20% → TP 는 남은 수량으로 다시 → 성과에 부분 손익.
func TestScaleOutEndToEnd(t *testing.T) {
	h := newHarness(t)
	now := base.Add(time.Second)

	h.applyTargets(t, 1, scaleTarget(nil, 1200))
	h.x.Tick(ctx, now)
	h.x.Tick(ctx, now) // TP 위임
	ps := h.eng.Positions()
	if len(ps) != 1 || ps[0].EntryQty != ps[0].Qty || ps[0].Group != "d205-005930" || ps[0].TpOrderID == "" {
		t.Fatalf("진입 %+v", ps)
	}
	entry := ps[0].EntryQty

	h.applyTargets(t, 2, scaleTarget(f64(0.5), 1200))
	res := h.x.Tick(ctx, now)
	if res.Reduced != 1 || res.Exited != 0 || len(res.Errors) != 0 {
		t.Fatalf("분할매도 %+v", res)
	}
	if q := h.eng.Positions()[0].Qty; q != entry*0.5 {
		t.Fatalf("남은 수량 %v, 기대 %v", q, entry*0.5)
	}

	// 같은 스냅샷이 다시 와도(재발행) 또 팔지 않는다. 그 사이 TP 는 남은 수량으로 다시 걸린다.
	h.applyTargets(t, 3, scaleTarget(f64(0.5), 1200))
	res = h.x.Tick(ctx, now)
	if res.Reduced != 0 || res.TpPlaced != 1 {
		t.Fatalf("재발행에 또 팔았거나 TP 를 다시 안 걸었다: %+v", res)
	}
	if h.br.OpenTPOrders() != 1 {
		t.Fatal("TP 주문 수")
	}

	h.applyTargets(t, 4, scaleTarget(f64(0.2), 1200))
	h.x.Tick(ctx, now)
	lot := h.eng.Positions()[0]
	if lot.Qty != entry*0.2 || lot.EntryQty != entry {
		t.Fatalf("2차 분할매도 %+v (최초 %v 은 그대로여야)", lot, entry)
	}

	// 원장·성과 — 로트는 열려 있어도 판 몫은 실현손익에 잡힌다.
	reduces := 0
	for _, o := range h.ledger(t) {
		if o.Phase == "exit_filled" && o.ExitReason == "reduce" {
			reduces++
		}
	}
	if reduces != 2 {
		t.Fatalf("분할매도 원장 %d 줄", reduces)
	}
	stats, _ := h.eng.BookStats(ctx, protocol.ModePaper)
	if len(stats) != 1 || stats[0].RealizedKRW == 0 || stats[0].Open != 1 || stats[0].Closed != 0 {
		t.Fatalf("성과 %+v", stats)
	}
}
