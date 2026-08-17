package engine

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

const kid = "pw-test"

var base = time.Date(2026, 8, 15, 0, 5, 0, 0, time.UTC)

func keys(t *testing.T) (ed25519.PrivateKey, map[string]ed25519.PublicKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 7)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv, map[string]ed25519.PublicKey{kid: priv.Public().(ed25519.PublicKey)}
}

func newEngine(t *testing.T) (*Engine, ed25519.PrivateKey) {
	t.Helper()
	priv, pub := keys(t)
	p := protocol.DefaultPolicy()
	p.TrustedKeys = pub
	return New(Config{
		Mode:         protocol.ModePaper,
		Policy:       p,
		TargetMaxAge: 180 * time.Second,
		MaxOrders:    5,
		EngineBudget: 1_000_000,
		Price:        func(protocol.Symbol) (float64, bool) { return 1000, true },
		Market:       func(protocol.Symbol) sizing.Market { return sizing.StockMarket() },
	}, base), priv
}

func signEnv(t *testing.T, m map[string]any, priv ed25519.PrivateKey) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	out, err := protocol.Sign(raw, kid, priv)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func targetMsg(seq uint64, ts, exp, notAfter, asOfBar time.Time) map[string]any {
	return map[string]any{
		"v": 1, "typ": "intent.target", "id": "01J9Z8QK3M7X2ABCDEFGHJKMNP",
		"acct": "acc_7f3a", "ts": ts, "exp": exp, "nonce": "n" + exp.Format("150405"), "seq": seq,
		"body": map[string]any{
			"as_of_bar": asOfBar, "book_state": "normal",
			"targets": []any{map[string]any{
				"intent_id": "01J9Z8QK3M7X2ABCDEFGHJKMNQ",
				"slot":      "gapdown_a",
				"symbol":    map[string]any{"exchange": "KRX", "code": "005930"},
				"side":      "long", "want": "open", "weight": 0.5,
				"entry": map[string]any{"mode": "market", "not_after": notAfter},
				"exit":  map[string]any{"stop_price": 900},
			}},
		},
	}
}

func deriskMsg(nonce, action string, ts, exp time.Time) []byte {
	m := map[string]any{
		"v": 1, "typ": "cmd.derisk", "id": "01J9Z8QK3M7X2ABCDEFGHJKMNS",
		"acct": "acc_7f3a", "ts": ts, "exp": exp, "nonce": nonce,
		"body": map[string]any{"action": action, "scope": "all"},
	}
	b, _ := json.Marshal(m)
	return b
}

// 계약 한 바퀴: 서명된 목표를 넣으면 계획이 나오고, ack 가 per_intent 까지 채운다.
func TestApplyTargetProducesPlan(t *testing.T) {
	e, priv := newEngine(t)
	now := base.Add(time.Second)

	ack := e.Apply(signEnv(t, targetMsg(1, base, base.Add(time.Minute), base.Add(time.Minute), base), priv), now)
	if ack.Status != "applied" {
		t.Fatalf("ack=%+v", ack)
	}
	if len(ack.PerIntent) != 1 || ack.PerIntent[0].Status != "applied" {
		t.Fatalf("per_intent=%+v", ack.PerIntent)
	}

	plan := e.Plan(now)
	if len(plan.Enters) != 1 || plan.Enters[0].Qty != 500 {
		t.Fatalf("plan=%+v", plan)
	}
	if snap := e.Snapshot(); snap.AppliedSeq != 1 {
		t.Fatalf("applied_seq=%d", snap.AppliedSeq)
	}
}

func TestUnsignedTargetIsRejectedEndToEnd(t *testing.T) {
	e, _ := newEngine(t)
	raw, _ := json.Marshal(targetMsg(1, base, base.Add(time.Minute), base.Add(time.Minute), base))

	ack := e.Apply(raw, base.Add(time.Second))
	if ack.Status != "rejected" || ack.Codes[0] != protocol.CodeSig {
		t.Fatalf("ack=%+v", ack)
	}
	if len(e.Plan(base.Add(time.Second)).Enters) != 0 {
		t.Fatal("거절된 목표로 계획이 나왔다")
	}
}

// ★ 목표 스냅샷이 늙으면 진입은 죽고 청산은 산다.
func TestStaleTargetBlocksEntriesOnly(t *testing.T) {
	e, priv := newEngine(t)
	old := base.Add(-10 * time.Minute) // TargetMaxAge(180s) 를 훨씬 넘김
	now := base.Add(time.Second)

	// 봉투 자체는 신선하되, 계산된 봉(as_of_bar)이 늙은 경우.
	ack := e.Apply(signEnv(t, targetMsg(1, base, base.Add(time.Minute), base.Add(time.Minute), old), priv), now)
	if ack.Status != "applied" {
		t.Fatalf("ack=%+v", ack)
	}
	if len(e.Plan(now).Enters) != 0 {
		t.Fatal("늙은 목표로 진입했다")
	}
	if !e.Snapshot().Guards.TargetStale {
		t.Fatal("스냅샷이 늙음을 보고하지 않았다")
	}

	// 같은 목표로 이미 들고 있는 포지션의 stop 갱신은 계속 나와야 한다.
	e.SetPositions([]protocol.Position{{
		IntentID: "01J9Z8QK3M7X2ABCDEFGHJKMNQ", Slot: "gapdown_a",
		Symbol: protocol.Symbol{Exchange: "KRX", Code: "005930"}, Qty: 10,
		AvgEntryPrice: 1000, StopArmed: 800,
	}})
	if len(e.Plan(now).StopUpdates) != 1 {
		t.Fatal("늙었다고 stop 갱신까지 멈췄다")
	}
}

func TestDeriskPauseBlocksEntries(t *testing.T) {
	e, priv := newEngine(t)
	now := base.Add(time.Second)
	e.Apply(signEnv(t, targetMsg(1, base, base.Add(time.Minute), base.Add(time.Minute), base), priv), now)

	if ack := e.Apply(deriskMsg("n1", "pause", base, base.Add(5*time.Minute)), now); ack.Status != "applied" {
		t.Fatalf("ack=%+v", ack)
	}
	if len(e.Plan(now).Enters) != 0 {
		t.Fatal("pause 인데 진입했다")
	}
	if !e.Snapshot().Guards.Paused {
		t.Fatal("스냅샷이 pause 를 보고하지 않았다")
	}
}

// ★ liquidate 는 목표와 무관하게 전량 청산이고, 새 목표가 와도 유지된다.
// 사람이 명시적으로 resume 하기 전에는 안 풀린다.
func TestDeriskLiquidateOverridesTargets(t *testing.T) {
	e, priv := newEngine(t)
	now := base.Add(time.Second)
	e.SetPositions([]protocol.Position{{
		IntentID: "held-1", Symbol: protocol.Symbol{Exchange: "KRX", Code: "000660"}, Qty: 5, AvgEntryPrice: 100,
	}})

	e.Apply(deriskMsg("n1", "liquidate", base, base.Add(5*time.Minute)), now)

	plan := e.Plan(now)
	if len(plan.Exits) != 1 || plan.Exits[0].Reason != "derisk" {
		t.Fatalf("plan=%+v", plan)
	}

	// 새 목표가 와도 진입은 안 된다.
	e.Apply(signEnv(t, targetMsg(2, base, base.Add(time.Minute), base.Add(time.Minute), base), priv), now)
	if len(e.Plan(now).Enters) != 0 {
		t.Fatal("liquidate 중인데 새 목표로 진입했다")
	}

	// resume 해야 풀린다.
	e.Apply(deriskMsg("n2", "resume", base, base.Add(5*time.Minute)), now)
	if len(e.Plan(now).Enters) != 1 {
		t.Fatalf("resume 후에도 진입이 안 된다: %+v", e.Plan(now))
	}
}

// 재접속 시 retained 로 같은 목표가 다시 오는 건 정상 — 상태가 흔들리면 안 된다.
func TestReplayedRetainedTargetIsIgnoredCleanly(t *testing.T) {
	e, priv := newEngine(t)
	now := base.Add(time.Second)
	msg := signEnv(t, targetMsg(1, base, base.Add(time.Minute), base.Add(time.Minute), base), priv)

	e.Apply(msg, now)
	ack := e.Apply(msg, now.Add(time.Second))
	if ack.Status != "ignored" || len(ack.Codes) != 0 {
		t.Fatalf("ack=%+v — 정상 재수신을 에러로 다뤘다", ack)
	}
	if len(e.Plan(now).Enters) != 1 {
		t.Fatal("재수신이 계획을 망가뜨렸다")
	}
}

// 목표를 한 번도 못 받은 상태는 "정상"이 아니라 "늙음"이다.
func TestFreshEngineReportsStale(t *testing.T) {
	e, _ := newEngine(t)
	snap := e.Snapshot()
	if !snap.Guards.TargetStale {
		t.Fatal("배선 없는 상태가 정상으로 보인다")
	}
	if snap.Mode == "" {
		t.Fatal("mode 가 비었다")
	}
}

// ── 영속 (store 배선) ───────────────────────────────────────────────────────

func newPersistentEngine(t *testing.T, dbPath string) (*Engine, ed25519.PrivateKey, *store.Store) {
	t.Helper()
	st, err := store.OpenPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	priv, pub := keys(t)
	p := protocol.DefaultPolicy()
	p.TrustedKeys = pub
	e := New(Config{
		Mode: protocol.ModePaper, Policy: p, TargetMaxAge: 180 * time.Second, MaxOrders: 5,
		EngineBudget: 1_000_000,
		Price:        func(protocol.Symbol) (float64, bool) { return 1000, true },
		Market:       func(protocol.Symbol) sizing.Market { return sizing.StockMarket() },
		Store:        st,
	}, base)
	if err := e.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e, priv, st
}

// ★ 재시작하면 풀리는 일시정지는 안전장치가 아니다.
func TestPauseSurvivesRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "cockpit.db")
	now := base.Add(time.Second)

	e1, _, st1 := newPersistentEngine(t, db)
	e1.Apply(deriskMsg("n1", "pause", base, base.Add(5*time.Minute)), now)
	if !e1.Snapshot().Guards.Paused {
		t.Fatal("pause 가 안 걸렸다")
	}
	st1.Close()

	e2, priv, st2 := newPersistentEngine(t, db)
	defer st2.Close()
	if !e2.Snapshot().Guards.Paused {
		t.Fatal("★ 재시작에 pause 가 풀렸다")
	}
	e2.Apply(signEnv(t, targetMsg(1, base, base.Add(time.Minute), base.Add(time.Minute), base), priv), now)
	if len(e2.Plan(now).Enters) != 0 {
		t.Fatal("풀린 pause 로 진입까지 했다")
	}
}

// ★ 종결된 목표로 재진입하지 않는다 — retained 목표가 재접속마다 그대로 다시 오기 때문에.
func TestClosedIntentIsNotReenteredAfterRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "cockpit.db")
	now := base.Add(time.Second)
	const id = "01J9Z8QK3M7X2ABCDEFGHJKMNQ"

	e1, priv, st1 := newPersistentEngine(t, db)
	e1.Apply(signEnv(t, targetMsg(1, base, base.Add(time.Minute), base.Add(time.Minute), base), priv), now)
	if len(e1.Plan(now).Enters) != 1 {
		t.Fatal("첫 진입 계획이 없다")
	}

	// 진입 → 원장 기록 → stop 에 털림 → 종결.
	if err := st1.UpsertIntent(context.Background(), store.Intent{
		IntentID: id, Slot: "gapdown_a",
		Symbol: protocol.Symbol{Exchange: "KRX", Code: "005930"}, Side: "long",
		Qty: 500, AvgEntryPrice: 1000, StopArmed: 900,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st1.CloseIntent(context.Background(), id, "stop", now); err != nil {
		t.Fatal(err)
	}
	st1.Close()

	// 재시작 — 같은 retained 목표가 그대로 다시 온다 (진입 창도 아직 안 지났다).
	e2, priv2, st2 := newPersistentEngine(t, db)
	defer st2.Close()
	e2.Apply(signEnv(t, targetMsg(1, base, base.Add(time.Minute), base.Add(time.Minute), base), priv2), now)

	plan := e2.Plan(now)
	if len(plan.Enters) != 0 {
		t.Fatalf("★ 털린 자리에 같은 목표로 재진입했다: %+v", plan.Enters)
	}
	var found bool
	for _, a := range plan.Acks {
		if a.IntentID == id && len(a.Codes) > 0 && a.Codes[0] == protocol.CodeTerminal {
			found = true
		}
	}
	if !found {
		t.Fatalf("왜 안 샀는지 안 남겼다: %+v", plan.Acks)
	}
}

// store 없이도 엔진은 돈다 (단, 위 두 성질은 사라진다).
func TestEngineWorksWithoutStore(t *testing.T) {
	e, priv := newEngine(t)
	now := base.Add(time.Second)
	if ack := e.Apply(signEnv(t, targetMsg(1, base, base.Add(time.Minute), base.Add(time.Minute), base), priv), now); ack.Status != "applied" {
		t.Fatalf("ack=%+v", ack)
	}
	if _, err := e.Ledger(context.Background(), protocol.ModeLive, 10); err != nil {
		t.Fatalf("store 없을 때 원장 조회가 터졌다: %v", err)
	}
}
