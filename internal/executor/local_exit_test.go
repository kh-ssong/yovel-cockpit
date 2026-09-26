package executor

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

const lxID = "01J9Z8QK3M7X2ABCDEFGHJKMNQ"

var lxSym = map[string]any{"exchange": "KRX", "code": "005930"}

// openWith — stop·시간청산이 걸린 진입 목표.
func (h *harness) openWith(t *testing.T, seq uint64, stop float64, timeExit time.Time) {
	t.Helper()
	h.applyTarget(t, seq, map[string]any{
		"intent_id": lxID, "slot": "d205", "symbol": lxSym, "side": "long", "want": "open",
		"weight": 0.5,
		"entry":  map[string]any{"mode": "market", "not_after": base.Add(time.Minute), "ref_price": 1000},
		"exit":   map[string]any{"stop_price": stop, "time_exit_at": timeExit},
	})
}

func (h *harness) withQuote(q func(context.Context, protocol.Symbol) (float64, time.Time, bool)) {
	h.x = New(Deps{
		Broker: h.br, Store: h.st, Engine: h.eng, Mode: protocol.ModePaper, DaemonSHA: "abc1234",
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Quote: q, MaxPriceAge: time.Minute,
	})
}

func exitReasons(h *harness, t *testing.T) []string {
	var out []string
	for _, o := range h.ledger(t) {
		if o.Phase == "exit_filled" {
			out = append(out, o.ExitReason)
		}
	}
	return out
}

// ★ 신호원이 죽어도 15:20 에는 닫힌다 — 시세 없이, 시계만으로.
func TestTimeExitFiresWithoutSignalOrQuote(t *testing.T) {
	h := newHarness(t)
	h.withQuote(nil)
	exitAt := base.Add(10 * time.Minute)
	h.openWith(t, 1, 850, exitAt)
	if res := h.x.Tick(ctx, base.Add(time.Second)); res.Entered != 1 {
		t.Fatalf("%+v", res)
	}
	if res := h.x.Tick(ctx, exitAt.Add(-time.Second)); res.Exited != 0 {
		t.Fatalf("시간 전에 팔았다: %+v", res)
	}
	// 목표는 여전히 want=open — 판단자는 아무것도 안 보냈다.
	if res := h.x.Tick(ctx, exitAt); res.Exited != 1 {
		t.Fatalf("★ 시간청산이 발동하지 않았다: %+v", res)
	}
	if r := exitReasons(h, t); len(r) != 1 || r[0] != "time" {
		t.Fatalf("청산 사유 %v", r)
	}
	// 종결됐으므로 같은 목표로 재진입하지 않는다.
	if res := h.x.Tick(ctx, exitAt.Add(time.Second)); res.Entered != 0 {
		t.Fatalf("시간청산 뒤 재진입했다: %+v", res)
	}
}

// ★ 재시작해도 시간청산이 살아 있다 — 원장에서 복원된다.
func TestTimeExitSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.withQuote(nil)
	exitAt := base.Add(10 * time.Minute)
	h.openWith(t, 1, 850, exitAt)
	h.x.Tick(ctx, base.Add(time.Second))

	open, err := h.st.OpenIntents(ctx)
	if err != nil || len(open) != 1 || open[0].TimeExitAt == nil || !open[0].TimeExitAt.Equal(exitAt) {
		t.Fatalf("원장에 시간청산이 없다: %+v %v", open, err)
	}
}

func TestStopFiresOnFreshPrice(t *testing.T) {
	h := newHarness(t)
	price := 1000.0
	h.withQuote(func(_ context.Context, _ protocol.Symbol) (float64, time.Time, bool) {
		return price, base.Add(time.Second), true
	})
	h.openWith(t, 1, 850, base.Add(time.Hour))
	h.x.Tick(ctx, base.Add(time.Second))

	price = 849
	res := h.x.Tick(ctx, base.Add(2*time.Second))
	if res.Exited != 1 {
		t.Fatalf("stop 이 안 걸렸다: %+v", res)
	}
	if r := exitReasons(h, t); len(r) != 1 || r[0] != "stop" {
		t.Fatalf("청산 사유 %v", r)
	}
}

// ★ 늙은 시세로는 팔지 않는다 — 대신 Blind 로 올린다.
func TestStaleQuoteIsBlindNotSold(t *testing.T) {
	h := newHarness(t)
	h.withQuote(func(_ context.Context, _ protocol.Symbol) (float64, time.Time, bool) {
		return 500, base.Add(-time.Hour), true // stop 아래지만 한 시간 전 값
	})
	h.openWith(t, 1, 850, base.Add(time.Hour))
	h.x.Tick(ctx, base.Add(time.Second))

	res := h.x.Tick(ctx, base.Add(2*time.Second))
	if res.Exited != 0 || len(res.Blind) != 1 {
		t.Fatalf("늙은 시세로 팔았거나 Blind 를 안 올렸다: %+v", res)
	}
}

// 같은 틱에 판단자 flat 과 시간청산이 겹쳐도 한 번만 판다.
func TestLocalExitAndFlatSameTickSellOnce(t *testing.T) {
	h := newHarness(t)
	h.withQuote(nil)
	exitAt := base.Add(10 * time.Minute)
	h.openWith(t, 1, 850, exitAt)
	h.x.Tick(ctx, base.Add(time.Second))

	h.applyTarget(t, 2, map[string]any{
		"intent_id": lxID, "slot": "d205", "symbol": lxSym, "side": "long", "want": "flat",
	})
	res := h.x.Tick(ctx, exitAt)
	if res.Exited != 1 || len(res.Errors) != 0 {
		t.Fatalf("%+v", res)
	}
	if n := len(exitReasons(h, t)); n != 1 {
		t.Fatalf("청산 %d 번", n)
	}
}
