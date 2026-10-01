package executor

import (
	"testing"
	"time"
)

// ★ 발행자가 상태 조회 사이에 열리고 닫힌 로트를 볼 수 있어야 한다 (flat6 dummy 통합 테스트: 1.4초 왕복).
func TestRecentClosesVisibleInState(t *testing.T) {
	h := newHarness(t)
	now := base.Add(time.Second)
	id := "01J9Z8QK3M7X2ABCDEFGHJKMNQ"
	sym := map[string]any{"exchange": "KRX", "code": "005930"}
	h.applyTarget(t, 1, map[string]any{
		"intent_id": id, "slot": "d205", "symbol": sym, "side": "long", "want": "open", "weight": 0.5,
		"entry": map[string]any{"mode": "market", "not_after": base.Add(time.Minute), "ref_price": 1000},
		"exit":  map[string]any{"stop_price": 900},
	})
	h.x.Tick(ctx, now)
	h.applyTarget(t, 2, map[string]any{"intent_id": id, "slot": "d205", "symbol": sym, "side": "long", "want": "flat"})
	h.x.Tick(ctx, now)

	snap := h.eng.SnapshotAt(now)
	if len(snap.Positions) != 0 || len(snap.RecentCloses) != 1 {
		t.Fatalf("positions=%d recent=%+v", len(snap.Positions), snap.RecentCloses)
	}
	c := snap.RecentCloses[0]
	if c.IntentID != id || c.Reason != "flat" || c.Price <= 0 || c.Symbol.Code != "005930" {
		t.Fatalf("%+v", c)
	}
}
