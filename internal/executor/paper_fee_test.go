package executor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker/paper"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// ★ 진입·청산 주문이 소스(kid, scope)를 브로커까지 싣는다 — paper 가 장부 요율(ETF = 거래세 없음)을
// 고르려면 주문이 어느 장부 것인지 알아야 한다 (user 2026-10-16 «주식/ETF 구분»).
func TestPaperFeeFollowsBook(t *testing.T) {
	h := newHarness(t)
	const scope = "kiwoom/klev_push10_f60"
	br := paper.New(paper.Config{
		Cash: 1_000_000, FeeBpBuy: 1.5, FeeBpSell: 21.5, SlipBp: 10, Lot: 1,
		Now:   func() time.Time { return base },
		Price: func(protocol.Symbol) (float64, bool) { return 1000, true },
		FeeFor: func(k, s string) (float64, float64, bool) {
			return 0, 0, k == kid && s == scope
		},
	})
	h.br, h.x.d.Broker = br, br

	send := func(seq uint64, want string) {
		tgt := map[string]any{
			"intent_id": "01J9Z8QK3M7X2ABCDEFGHJKMF1", "slot": "main", "side": "long", "want": want,
			"symbol": map[string]any{"exchange": "KRX", "code": "122630"},
		}
		if want == "open" {
			tgt["weight"] = 0.1
			tgt["entry"] = map[string]any{"mode": "market", "not_after": base.Add(time.Minute), "ref_price": 1000}
			tgt["exit"] = map[string]any{"stop_price": 500}
		} else {
			tgt["mark_price"] = 1010.0
		}
		raw, _ := json.Marshal(map[string]any{
			"v": 1, "typ": "intent.target", "id": "01J9Z8QK3M7X2ABCDEFGHJKMNP",
			"acct": "acc_7f3a", "ts": base, "exp": base.Add(time.Minute), "nonce": "n1", "seq": seq,
			"body": map[string]any{"scope": scope, "as_of_bar": base, "book_state": "normal", "targets": []any{tgt}},
		})
		signed, err := protocol.Sign(raw, kid, h.pk)
		if err != nil {
			t.Fatal(err)
		}
		h.apply(t, signed)
	}

	send(1, "open")
	h.x.Tick(ctx, base.Add(time.Second))
	send(2, "flat")
	h.x.Tick(ctx, base.Add(2*time.Second))

	rows := h.ledger(t)
	if len(rows) != 2 {
		t.Fatalf("원장 %d행: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Scope != scope || r.FeeKRW != 0 {
			t.Fatalf("%s: scope=%q fee=%v — ETF 장부 요율(0)이 안 걸렸다", r.Phase, r.Scope, r.FeeKRW)
		}
	}
}
