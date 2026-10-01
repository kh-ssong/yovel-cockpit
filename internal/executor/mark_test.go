package executor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker/paper"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// signed — 목표 하나를 서명해 적용한다 (mark_price 같은 선택 필드를 싣기 위해).
func (h *harness) applyTarget(t *testing.T, seq uint64, tgt map[string]any) {
	t.Helper()
	m := map[string]any{
		"v": 1, "typ": "intent.target", "id": "01J9Z8QK3M7X2ABCDEFGHJKMNP",
		"acct": "acc_7f3a", "ts": base, "exp": base.Add(time.Minute),
		"nonce": "n1", "seq": seq,
		"body": map[string]any{"as_of_bar": base, "book_state": "normal", "targets": []any{tgt}},
	}
	raw, _ := json.Marshal(m)
	signed, err := protocol.Sign(raw, kid, h.pk)
	if err != nil {
		t.Fatal(err)
	}
	h.apply(t, signed)
}

// ★ 키 없는 paper 도 청산이 **판단자가 본 가격**에 체결돼야 한다. 예전엔 평단 근사라
// 손익이 0 근처로 뭉개져 전략 검증이 안 됐다.
func TestPaperExitFillsAtSignalMark(t *testing.T) {
	h := newHarness(t)
	now := base.Add(time.Second)
	// 시세원 = flat6 mark 뿐 (키움 키 없음).
	h.br = paper.New(paper.Config{
		Cash: 1_000_000, Lot: 1, Now: func() time.Time { return base },
		Price: func(s protocol.Symbol) (float64, bool) { return h.eng.Mark(s, now, time.Minute) },
	})
	h.rewire(h.br)

	id := "01J9Z8QK3M7X2ABCDEFGHJKMNQ"
	symbol := map[string]any{"exchange": "KRX", "code": "005930"}
	h.applyTarget(t, 1, map[string]any{
		"intent_id": id, "slot": "d205", "symbol": symbol, "side": "long", "want": "open",
		"weight": 0.5, "mark_price": 1000,
		"entry": map[string]any{"mode": "market", "not_after": base.Add(time.Minute), "ref_price": 1000},
		"exit":  map[string]any{"stop_price": 850},
	})
	if res := h.x.Tick(ctx, now); res.Entered != 1 {
		t.Fatalf("%+v", res)
	}

	h.applyTarget(t, 2, map[string]any{
		"intent_id": id, "slot": "d205", "symbol": symbol, "side": "long", "want": "flat",
		"mark_price": 1070,
	})
	if res := h.x.Tick(ctx, now); res.Exited != 1 {
		t.Fatalf("%+v", res)
	}
	found := false
	for _, o := range h.ledger(t) {
		if o.Phase == "exit_filled" {
			found = true
			if o.Price != 1070 || o.Detail != "" {
				t.Fatalf("청산이 mark 가 아니라 %v 에 체결됐다 (detail=%q)", o.Price, o.Detail)
			}
		}
	}
	if !found {
		t.Fatal("청산 기록이 없다")
	}

	// 장부 성적 — 원장에서 다시 계산된다. 500주 × (1070 − 1000) = 35,000원, 수수료 0.
	stats, err := h.eng.BookStats(ctx, protocol.ModePaper)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Closed != 1 || stats[0].Wins != 1 || stats[0].RealizedKRW != 35_000 {
		t.Fatalf("장부 성적 %+v", stats)
	}
}

// 늙은 mark 는 없는 것이다 — 몇 분 전 가격으로 체결시키면 손익이 조용히 틀린다.
func TestStaleMarkIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.applyTarget(t, 1, map[string]any{
		"intent_id": "01J9Z8QK3M7X2ABCDEFGHJKMNQ", "slot": "d205",
		"symbol": map[string]any{"exchange": "KRX", "code": "005930"}, "side": "long", "want": "flat",
		"mark_price": 1070, "mark_at": base,
	})
	s := protocol.Symbol{Exchange: "KRX", Code: "005930"}
	if _, ok := h.eng.Mark(s, base.Add(30*time.Second), time.Minute); !ok {
		t.Fatal("신선한 mark 를 못 읽었다")
	}
	if _, ok := h.eng.Mark(s, base.Add(5*time.Minute), time.Minute); ok {
		t.Fatal("늙은 mark 를 썼다")
	}
}
