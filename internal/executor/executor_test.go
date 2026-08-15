package executor

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/paper"
	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

var (
	ctx  = context.Background()
	base = time.Date(2026, 8, 15, 0, 5, 0, 0, time.UTC)
	code = protocol.Symbol{Exchange: "KRX", Code: "005930"}
)

const kid = "pw-test"

type harness struct {
	st  *store.Store
	eng *engine.Engine
	br  *paper.Broker
	x   *Executor
	pk  ed25519.PrivateKey
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	st, err := store.OpenPath(filepath.Join(t.TempDir(), "cockpit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 3)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pol := protocol.DefaultPolicy()
	pol.TrustedKeys = map[string]ed25519.PublicKey{kid: priv.Public().(ed25519.PublicKey)}

	br := paper.New(paper.Config{
		Cash: 1_000_000, FeeBp: 15, SlipBp: 10, Lot: 1,
		Now:   func() time.Time { return base },
		Price: func(protocol.Symbol) (float64, bool) { return 1000, true },
	})

	eng := engine.New(engine.Config{
		Mode: protocol.ModePaper, Policy: pol, TargetMaxAge: 180 * time.Second, MaxOrders: 5,
		SlotCapital: func(string) float64 { return 1_000_000 },
		Price:       func(protocol.Symbol) (float64, bool) { return 1000, true },
		Market:      func(protocol.Symbol) sizing.Market { return sizing.StockMarket() },
		Store:       st,
	}, base)
	if err := eng.Restore(ctx); err != nil {
		t.Fatal(err)
	}

	h := &harness{st: st, eng: eng, br: br, pk: priv}
	h.x = New(Deps{
		Broker: br, Store: st, Engine: eng, Mode: protocol.ModePaper,
		DaemonSHA: "abc1234",
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h
}

// target 은 서명된 intent.target 바이트를 만든다.
func (h *harness) target(t *testing.T, seq uint64, want protocol.Want, stop, tp float64) []byte {
	t.Helper()
	tgt := map[string]any{
		"intent_id": "01J9Z8QK3M7X2ABCDEFGHJKMNQ",
		"slot":      "gapdown_a",
		"symbol":    map[string]any{"exchange": "KRX", "code": "005930"},
		"side":      "long",
		"want":      string(want),
	}
	if want == protocol.WantOpen {
		tgt["weight"] = 0.5
		tgt["entry"] = map[string]any{"mode": "market", "not_after": base.Add(time.Minute)}
		ex := map[string]any{"stop_price": stop}
		if tp > 0 {
			ex["tp_price"] = tp
			ex["tp_delegate"] = true
		}
		tgt["exit"] = ex
	}

	m := map[string]any{
		"v": 1, "typ": "intent.target", "id": "01J9Z8QK3M7X2ABCDEFGHJKMNP",
		"acct": "acc_7f3a", "ts": base, "exp": base.Add(time.Minute),
		"nonce": "n1", "seq": seq,
		"body": map[string]any{
			"as_of_bar": base, "book_state": "normal", "targets": []any{tgt},
		},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := protocol.Sign(raw, kid, h.pk)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func (h *harness) apply(t *testing.T, raw []byte) {
	t.Helper()
	if ack := h.eng.Apply(raw, base.Add(time.Second)); ack.Status != "applied" {
		t.Fatalf("목표가 안 먹었다: %+v", ack)
	}
}

func (h *harness) ledger(t *testing.T) []store.Order {
	t.Helper()
	rows, err := h.st.Ledger(ctx, store.LedgerQuery{Mode: protocol.ModePaper})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// 계약 한 바퀴: 서명된 목표 → 주문 → 원장 → 종결 → 재진입 방지.
func TestFullRoundTrip(t *testing.T) {
	h := newHarness(t)
	now := base.Add(time.Second)

	h.apply(t, h.target(t, 1, protocol.WantOpen, 900, 0))
	res := h.x.Tick(ctx, now)
	if res.Entered != 1 || len(res.Errors) > 0 {
		t.Fatalf("진입 실패: %+v", res)
	}

	rows := h.ledger(t)
	if len(rows) != 1 || rows[0].Side != "buy" {
		t.Fatalf("원장=%+v", rows)
	}
	// 사이징은 체결 **전** 기준가로 한다 (50만 / 1000 = 500주). 체결은 그보다 불리하게 난다.
	if rows[0].Qty != 500 {
		t.Fatalf("qty=%v", rows[0].Qty)
	}
	// ★ 그래서 실제 집행 금액은 예산을 살짝 넘는다 (500주 × 1001원 = 500,500).
	// 라이브에서 이 초과분이 주문가능금액을 넘으면 주문 자체가 거부되므로,
	// 키움 드라이버는 kt00011 사전 조회로 수량을 미리 깎아야 한다.
	if got := rows[0].Qty * rows[0].Price; got <= 500_000 {
		t.Fatalf("집행 금액 %v — 슬립이 반영 안 됐다", got)
	}
	if rows[0].SlippageBp <= 0 {
		t.Fatalf("슬리피지를 안 쟀다: %v", rows[0].SlippageBp)
	}
	if rows[0].FeeKRW <= 0 {
		t.Fatalf("수수료가 0 이다 — 비용 0 시뮬은 손익분기 근처 판정을 뒤집는다")
	}
	if rows[0].DaemonSHA != "abc1234" {
		t.Fatalf("어느 코드가 낸 주문인지 안 남겼다: %q", rows[0].DaemonSHA)
	}

	// 이미 들고 있으면 또 사지 않는다.
	if res2 := h.x.Tick(ctx, now); res2.Entered != 0 {
		t.Fatalf("중복 진입: %+v", res2)
	}

	// 청산 지시.
	h.apply(t, h.target(t, 2, protocol.WantFlat, 0, 0))
	res3 := h.x.Tick(ctx, now)
	if res3.Exited != 1 || len(res3.Errors) > 0 {
		t.Fatalf("청산 실패: %+v", res3)
	}
	rows = h.ledger(t)
	if len(rows) != 2 {
		t.Fatalf("원장 %d건", len(rows))
	}
	if open, _ := h.st.OpenIntents(ctx); len(open) != 0 {
		t.Fatalf("종결 안 됨: %+v", open)
	}

	// ★ 같은 목표가 다시 와도 재진입하지 않는다 (진입 창은 아직 살아 있다).
	h.apply(t, h.target(t, 3, protocol.WantOpen, 900, 0))
	res4 := h.x.Tick(ctx, now)
	if res4.Entered != 0 {
		t.Fatalf("★ 종결된 목표로 재진입했다: %+v", res4)
	}
}

// 업링크 큐에 원장이 그대로 쌓인다 (릴레이가 붙으면 이걸 올린다).
func TestOrdersAreQueuedForUplink(t *testing.T) {
	h := newHarness(t)
	h.apply(t, h.target(t, 1, protocol.WantOpen, 900, 0))
	h.x.Tick(ctx, base.Add(time.Second))

	pending, err := h.st.Pending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Typ != protocol.TypeEventOrder {
		t.Fatalf("큐=%+v", pending)
	}
	var ev protocol.EventOrder
	if err := json.Unmarshal(pending[0].Payload, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Phase != "filled" || ev.Qty <= 0 {
		t.Fatalf("업링크 본문이 비었다: %+v", ev)
	}
}

// ★ TP 를 위임하면 그 수량이 잠긴다. 청산 시 먼저 취소하지 않으면 매도가 거부된다.
func TestExitCancelsDelegatedTPFirst(t *testing.T) {
	h := newHarness(t)
	now := base.Add(time.Second)

	h.apply(t, h.target(t, 1, protocol.WantOpen, 900, 1200))
	if res := h.x.Tick(ctx, now); res.Entered != 1 {
		t.Fatalf("%+v", res)
	}
	// 진입 직후에는 아직 TP 가 없다 — 다음 틱에 위임된다.
	if res := h.x.Tick(ctx, now); res.TpPlaced != 1 {
		t.Fatalf("TP 위임 안 됨: %+v", res)
	}
	if h.br.OpenTPOrders() != 1 {
		t.Fatal("브로커에 TP 가 안 걸렸다")
	}

	h.apply(t, h.target(t, 2, protocol.WantFlat, 0, 0))
	res := h.x.Tick(ctx, now)
	if res.Exited != 1 || len(res.Errors) > 0 {
		t.Fatalf("★ 잠긴 잔고 때문에 청산이 막혔다: %+v", res)
	}
	if h.br.OpenTPOrders() != 0 {
		t.Fatal("TP 주문이 남아 있다")
	}
}

type brokenPositions struct {
	*paper.Broker
	err error
}

func (b brokenPositions) Positions(context.Context) ([]broker.Holding, error) {
	return nil, b.err
}

// ★ 조회 실패를 "포지션 없음" 으로 읽으면 전 포지션을 종결시켜 버린다.
// 그러면 로컬 stop 이 무장 해제되고, 사용자는 봇이 지키고 있다고 믿는다.
func TestPositionQueryFailureDoesNotCloseEverything(t *testing.T) {
	h := newHarness(t)
	now := base.Add(time.Second)

	h.apply(t, h.target(t, 1, protocol.WantOpen, 900, 0))
	if res := h.x.Tick(ctx, now); res.Entered != 1 {
		t.Fatalf("%+v", res)
	}

	h.x.d.Broker = brokenPositions{Broker: h.br, err: errors.New("네트워크 끊김")}
	res := h.x.Tick(ctx, now)

	if res.ClosedByBroker != 0 {
		t.Fatalf("★ 조회 실패로 %d건을 종결시켰다", res.ClosedByBroker)
	}
	if len(res.Errors) == 0 {
		t.Fatal("조회 실패를 조용히 넘겼다")
	}
	if open, _ := h.st.OpenIntents(ctx); len(open) != 1 {
		t.Fatalf("원장이 지워졌다: %+v", open)
	}
}

// ★ 우리가 안 팔았는데 사라진 포지션 = TP 체결 또는 수동 매도.
// 뭉뚱그리면 형제 레그를 잘못 청산하게 되므로 source 로 구분해 남긴다.
func TestVanishedPositionIsClosedAndTagged(t *testing.T) {
	h := newHarness(t)
	now := base.Add(time.Second)

	h.apply(t, h.target(t, 1, protocol.WantOpen, 900, 0))
	h.x.Tick(ctx, now)

	// 사용자가 HTS 로 직접 전량 매도했다고 가정.
	held, _ := h.br.Positions(ctx)
	if _, err := h.br.Sell(ctx, broker.OrderRequest{Symbol: code, Qty: held[0].Qty}); err != nil {
		t.Fatal(err)
	}

	res := h.x.Tick(ctx, now)
	if res.ClosedByBroker != 1 {
		t.Fatalf("사라진 포지션을 못 잡았다: %+v", res)
	}

	rows := h.ledger(t)
	var vanished *store.Order
	for i := range rows {
		if rows[i].Detail != "" {
			vanished = &rows[i]
		}
	}
	if vanished == nil {
		t.Fatalf("사후 감지 기록이 없다: %+v", rows)
	}
	if vanished.Source != store.SourceManual || vanished.ExitReason != "manual" {
		t.Fatalf("수동 매도로 태깅 안 됨: source=%q reason=%q", vanished.Source, vanished.ExitReason)
	}
}

// 한 종목이 실패해도 나머지는 계속 간다.
func TestOneFailureDoesNotBlockTheRest(t *testing.T) {
	h := newHarness(t)
	now := base.Add(time.Second)

	// 자본을 0 으로 만들어 진입만 실패시키고, 청산은 정상 동작하는지 본다.
	h.apply(t, h.target(t, 1, protocol.WantOpen, 900, 0))
	h.x.Tick(ctx, now)

	poor := paper.New(paper.Config{
		Cash: 0, Lot: 1, Now: func() time.Time { return base },
		Price: func(protocol.Symbol) (float64, bool) { return 1000, true },
	})
	// 보유는 그대로 두되 현금만 없는 상태를 흉내내기 위해 기존 브로커를 유지한다.
	_ = poor

	h.apply(t, h.target(t, 2, protocol.WantFlat, 0, 0))
	res := h.x.Tick(ctx, now)
	if res.Exited != 1 {
		t.Fatalf("청산이 안 됐다: %+v", res)
	}
}

// signal_ts 는 "신호가 나온 시각" 이라 체결보다 **앞서야** 한다.
//
// ★ 회귀 가드인 이유: 예전엔 entry.not_after(진입 마감시각, 미래)를 대신 실어서
// filled_at − signal_ts 가 음수로 나왔다. 부호가 뒤집힌 지연은 "지연 없음" 보다 나쁘다 —
// 값이 채워져 있으니 아무도 다시 안 본다. 이 저장소가 존재하는 이유의 절반이 체결 지연 측정이다.
func TestSignalTSPrecedesFill(t *testing.T) {
	h := newHarness(t)
	now := base.Add(time.Second)

	h.apply(t, h.target(t, 1, protocol.WantOpen, 900, 0))
	if res := h.x.Tick(ctx, now); res.Entered != 1 {
		t.Fatalf("진입 실패: %+v", res)
	}

	rows := h.ledger(t)
	if len(rows) != 1 {
		t.Fatalf("원장=%+v", rows)
	}
	o := rows[0]
	if o.SignalTS == nil || o.FilledAt == nil {
		t.Fatalf("시각이 비었다: signal=%v filled=%v", o.SignalTS, o.FilledAt)
	}
	if !o.SignalTS.Equal(base) {
		t.Errorf("signal_ts=%v — 목표 스냅샷의 as_of_bar(%v)여야 한다", o.SignalTS, base)
	}
	if d := o.FilledAt.Sub(*o.SignalTS); d < 0 {
		t.Fatalf("지연이 음수다 (%v) — signal_ts 에 미래 시각이 실렸다", d)
	}
}
