package executor

// 날 넘겨 드는 로트(스윙) — 장 밖 Blind · KRX 당일 TP 소멸 · 보유 중 시간청산 갱신 (2026-10-17).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
)

// kstOn — 날짜를 고를 수 있는 kst. 2026-10-01(목)·10-02(금)은 둘 다 거래일이다.
func kstOn(day, hm string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04:05", day+" "+hm, session.KST)
	return t.UTC()
}

func openTP(t0 time.Time, tp float64) map[string]any {
	o := openT(t0, nil)
	o["exit"] = map[string]any{"stop_price": 500, "tp_price": tp, "tp_delegate": true}
	return o
}

// ★ 장 밖엔 stop 을 판정하지 않는다 — 팔 수도 없고 시세도 안 오는데 Blind 로 세면 밤새 경보가 쌓인다.
// 장중에 시세가 끊긴 건 여전히 Blind 다.
func TestStopNotBlindOutsideMarketHours(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	h.x.d.Quote = func(context.Context, protocol.Symbol) (float64, time.Time, bool) { return 0, time.Time{}, false }
	h.sign(t, 1, t0, openT(t0, nil))
	if res := h.x.Tick(ctx, t0.Add(time.Second)); res.Entered != 1 {
		t.Fatalf("진입 %+v", res)
	}
	if res := h.x.Tick(ctx, kst("10:05:00")); len(res.Blind) != 1 {
		t.Fatalf("장중 시세 끊김을 Blind 로 안 올렸다: %+v", res)
	}
	for _, at := range []time.Time{kst("15:25:00"), kst("20:00:00"), kstOn("2026-10-02", "08:40:00")} {
		if res := h.x.Tick(ctx, at); len(res.Blind) != 0 {
			t.Fatalf("%v 장 밖인데 Blind: %+v", at, res)
		}
	}
	if res := h.x.Tick(ctx, kstOn("2026-10-02", "09:10:00")); len(res.Blind) != 1 {
		t.Fatalf("다음 장에 다시 안 본다: %+v", res)
	}
}

// ★ KRX TP 는 당일 주문이다. 장이 끝나면 지우고, 밤엔 다시 걸지 않고, 다음 장 접속매매에 다시 건다.
func TestDayTPExpiresAtCloseAndIsReplacedNextSession(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	h.sign(t, 1, t0, openTP(t0, 1100))
	h.x.Tick(ctx, t0.Add(time.Second))
	if res := h.x.Tick(ctx, t0.Add(10*time.Second)); res.TpPlaced != 1 {
		t.Fatalf("TP 안 걸림 %+v", res)
	}
	first := h.lot(t).TpOrderID

	if res := h.x.Tick(ctx, kst("15:31:00")); len(res.Errors) != 0 {
		t.Fatalf("%+v", res)
	}
	if l := h.lot(t); l.TpOrderID != "" || l.Qty != 100 {
		t.Fatalf("장 마감 뒤에도 지난 장 TP 를 들고 있다: %+v", l)
	}
	if open, _ := h.st.OpenIntents(ctx); len(open) != 1 || open[0].TpOrderID != "" {
		t.Fatalf("원장에 TP 가 남았다: %+v", open)
	}
	if res := h.x.Tick(ctx, kst("20:00:00")); res.TpPlaced != 0 {
		t.Fatalf("밤에 TP 를 걸었다: %+v", res)
	}
	h.fk.EndDay()
	if res := h.x.Tick(ctx, kstOn("2026-10-02", "08:59:30")); res.TpPlaced != 0 {
		t.Fatalf("장전 동시호가에 TP 를 걸었다: %+v", res)
	}
	if res := h.x.Tick(ctx, kstOn("2026-10-02", "09:00:05")); res.TpPlaced != 1 || len(res.Errors) != 0 {
		t.Fatalf("다음 장에 TP 를 다시 안 걸었다: %+v", res)
	}
	if l := h.lot(t); l.TpOrderID == "" || l.TpOrderID == first || l.TpArmed != 1100 {
		t.Fatalf("새 TP %+v (지난 장 %s)", l, first)
	}
}

// ★ 지난 장 TP 가 남아 있으면 다음 날 청산이 죽은 주문 취소에서 막혔다 (키움 800033).
func TestDeadDayTPDoesNotBlockNextDayExit(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	h.sign(t, 1, t0, openTP(t0, 1100))
	h.x.Tick(ctx, t0.Add(time.Second))
	h.x.Tick(ctx, t0.Add(10*time.Second))
	h.x.Tick(ctx, kst("15:31:00"))
	h.fk.EndDay()

	d2 := kstOn("2026-10-02", "09:00:00")
	*h.clock = d2
	h.sign(t, 2, d2, flatT())
	res := h.x.Tick(ctx, d2.Add(2*time.Second))
	if res.Exited != 1 || len(res.Errors) != 0 || len(h.eng.Positions()) != 0 {
		t.Fatalf("다음 날 청산이 막혔다: %+v", res)
	}
}

// 지난 장 TP 가 일부만 찼으면 찬 만큼 tp 로 적고 로트를 줄인다. 남은 수량은 계속 든다.
func TestDayTPPartialFillReducesLot(t *testing.T) {
	t0 := kst("10:00:00")
	h := newKiwoomHarness(t, t0)
	h.sign(t, 1, t0, openTP(t0, 1100))
	h.x.Tick(ctx, t0.Add(time.Second))
	h.x.Tick(ctx, t0.Add(10*time.Second))
	h.fk.FillLimit(h.lot(t).TpOrderID, 40)

	res := h.x.Tick(ctx, kst("15:31:00"))
	if res.Reduced != 1 || len(res.Errors) != 0 || len(res.Mismatch) != 0 {
		t.Fatalf("%+v", res)
	}
	if l := h.lot(t); l.Qty != 60 || l.TpOrderID != "" {
		t.Fatalf("로트 %+v", l)
	}
	n := 0
	for _, o := range h.ledger(t) {
		if o.Phase != "exit_filled" {
			continue
		}
		n++
		if o.ExitReason != "tp" || o.Qty != 40 || o.Price != 1100 || !strings.Contains(o.Detail, "일부") {
			t.Fatalf("원장 %+v", o)
		}
	}
	if n != 1 {
		t.Fatalf("tp 행 %d 개", n)
	}
	// 다음 틱(장 밖 1분 주기)에 같은 체결을 또 적지 않는다.
	h.x.Tick(ctx, kst("15:33:00"))
	if rows := len(h.ledger(t)); rows != 2 {
		t.Fatalf("원장 %d 행 (진입 1 + tp 1 이어야)", rows)
	}
}

// ★ 보유 중 time_exit_at 을 늘리면 옛 기한에 팔지 않고 새 기한에 판다. 목표에서 빠지면 그대로 둔다.
func TestTimeExitCanBeExtendedWhileHeld(t *testing.T) {
	h := newHarness(t)
	h.withQuote(nil)
	exit1, exit2 := base.Add(10*time.Minute), base.Add(72*time.Hour)
	h.openWith(t, 1, 850, exit1)
	h.x.Tick(ctx, base.Add(time.Second))

	h.openWith(t, 2, 850, exit2)
	h.x.Tick(ctx, base.Add(2*time.Second))
	if open, _ := h.st.OpenIntents(ctx); len(open) != 1 || open[0].TimeExitAt == nil || !open[0].TimeExitAt.Equal(exit2) {
		t.Fatalf("원장 기한이 안 바뀌었다: %+v", open)
	}

	h.applyTarget(t, 3, map[string]any{
		"intent_id": lxID, "slot": "d205", "symbol": lxSym, "side": "long", "want": "open", "weight": 0.5,
		"entry": map[string]any{"mode": "market", "not_after": base.Add(time.Minute), "ref_price": 1000},
		"exit":  map[string]any{"stop_price": 850},
	})
	if res := h.x.Tick(ctx, exit1); res.Exited != 0 {
		t.Fatalf("옛 기한에 팔았다: %+v", res)
	}
	if res := h.x.Tick(ctx, exit2); res.Exited != 1 {
		t.Fatalf("새 기한에 안 팔았다: %+v", res)
	}
	if r := exitReasons(h, t); len(r) != 1 || r[0] != "time" {
		t.Fatalf("청산 사유 %v", r)
	}
}
