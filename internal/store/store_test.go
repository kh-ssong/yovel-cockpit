package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

var ctx = context.Background()

func open(t *testing.T) *Store {
	t.Helper()
	s, err := OpenPath(filepath.Join(t.TempDir(), "cockpit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sym(code string) protocol.Symbol { return protocol.Symbol{Exchange: "KRX", Code: code} }

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cockpit.db")
	for i := 0; i < 2; i++ {
		s, err := OpenPath(p)
		if err != nil {
			t.Fatalf("%d번째 열기 실패: %v", i, err)
		}
		s.Close()
	}
}

// ★ intent_id ↔ 브로커 포지션 매핑. 브로커는 "005930 14주"만 알려주지,
// 그게 어느 목표에서 나왔고 stop 이 얼마였는지는 우리만 안다.
func TestIntentLifecycle(t *testing.T) {
	s := open(t)
	entry := time.Now().UTC().Add(-time.Hour)

	in := Intent{
		IntentID: "i1", Slot: "gapdown_a", Symbol: sym("005930"), Side: "long",
		Qty: 14, AvgEntryPrice: 72100, StopArmed: 71200, EntryAt: &entry,
	}
	if err := s.UpsertIntent(ctx, in); err != nil {
		t.Fatal(err)
	}

	open1, err := s.OpenIntents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open1) != 1 || open1[0].StopArmed != 71200 || open1[0].Qty != 14 {
		t.Fatalf("복구된 포지션이 다르다: %+v", open1)
	}
	if open1[0].EntryAt == nil || !open1[0].EntryAt.Equal(entry) {
		t.Fatalf("entry_at 이 유실됐다: %+v", open1[0].EntryAt)
	}

	// stop 만 갱신 — entry_at 은 덮어쓰지 않아야 한다.
	in.StopArmed = 73000
	in.EntryAt = nil
	if err := s.UpsertIntent(ctx, in); err != nil {
		t.Fatal(err)
	}
	open2, _ := s.OpenIntents(ctx)
	if open2[0].StopArmed != 73000 {
		t.Fatalf("stop 갱신 안 됨: %v", open2[0].StopArmed)
	}
	if open2[0].EntryAt == nil {
		t.Fatal("갱신하면서 entry_at 을 지웠다")
	}

	if err := s.CloseIntent(ctx, "i1", "stop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if open3, _ := s.OpenIntents(ctx); len(open3) != 0 {
		t.Fatalf("종결됐는데 열린 채로 남았다: %+v", open3)
	}
	term, _ := s.TerminalIntents(ctx)
	if _, ok := term["i1"]; !ok {
		t.Fatal("종결 집합에 없다 — 이게 없으면 같은 목표로 재진입한다")
	}
}

func TestCloseUnknownIntentErrors(t *testing.T) {
	s := open(t)
	if err := s.CloseIntent(ctx, "없는id", "stop", time.Now()); err == nil {
		t.Fatal("모르는 intent 를 조용히 종결시켰다")
	}
}

// ★ 원장은 mode 없이 못 쓴다. "전체 보기"가 기본이면 실계좌 손실이 수익으로 보인다.
func TestModeIsRequired(t *testing.T) {
	s := open(t)

	err := s.RecordOrder(ctx, Order{ID: "o1", IntentID: "i1", Phase: "filled",
		Symbol: sym("005930"), Side: "buy"})
	if !errors.Is(err, ErrModeRequired) {
		t.Fatalf("mode 없이 기록됐다: %v", err)
	}

	if _, err := s.Ledger(ctx, LedgerQuery{}); !errors.Is(err, ErrModeRequired) {
		t.Fatalf("mode 없이 조회됐다: %v", err)
	}
}

// 실제로 당했던 사고의 재현: 합산하면 "+3만 수익"인데 live 만 보면 손실이다.
func TestLedgerNeverMixesPaperAndLive(t *testing.T) {
	s := open(t)

	rec := func(id string, mode protocol.Mode, pnl float64) {
		t.Helper()
		if err := s.RecordOrder(ctx, Order{
			ID: id, IntentID: "i-" + id, Phase: "exit_filled", Symbol: sym("005930"),
			Side: "sell", RealizedPct: pnl, Mode: mode, Source: SourceBot,
		}); err != nil {
			t.Fatal(err)
		}
	}
	rec("L1", protocol.ModeLive, -0.012)
	rec("L2", protocol.ModeLive, -0.008)
	rec("P1", protocol.ModePaper, 0.05)
	rec("P2", protocol.ModePaper, 0.04)

	live, err := s.Ledger(ctx, LedgerQuery{Mode: protocol.ModeLive})
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 {
		t.Fatalf("live %d건", len(live))
	}
	var sum float64
	for _, o := range live {
		sum += o.RealizedPct
		if o.Mode != protocol.ModeLive {
			t.Fatalf("paper 가 섞였다: %+v", o)
		}
	}
	if sum >= 0 {
		t.Fatalf("live 합이 %v — 손실이어야 한다 (paper 가 섞였다는 뜻)", sum)
	}

	paper, _ := s.Ledger(ctx, LedgerQuery{Mode: protocol.ModePaper})
	if len(paper) != 2 {
		t.Fatalf("paper %d건", len(paper))
	}
}

// QoS1 은 at-least-once 라 같은 체결이 두 번 올 수 있다.
func TestRecordOrderIsIdempotent(t *testing.T) {
	s := open(t)
	o := Order{ID: "same", IntentID: "i1", Phase: "filled", Symbol: sym("005930"),
		Side: "buy", Qty: 14, Price: 72100, Mode: protocol.ModeLive}

	for i := 0; i < 3; i++ {
		if err := s.RecordOrder(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := s.Ledger(ctx, LedgerQuery{Mode: protocol.ModeLive})
	if len(rows) != 1 {
		t.Fatalf("%d건 — 같은 id 가 중복 기록됐다", len(rows))
	}
}

// ★ 사용자가 HTS 로 직접 판 것을 봇 청산으로 오인하면 형제 레그까지 잘못 청산된다.
func TestSourceIsPreserved(t *testing.T) {
	s := open(t)
	if err := s.RecordOrder(ctx, Order{ID: "m1", IntentID: "i1", Phase: "exit_filled",
		Symbol: sym("005930"), Side: "sell", Mode: protocol.ModeLive, Source: SourceManual}); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Ledger(ctx, LedgerQuery{Mode: protocol.ModeLive})
	if rows[0].Source != SourceManual {
		t.Fatalf("source=%q", rows[0].Source)
	}
}

// 지연 3종이 왕복해야 "신호 → 체결" 을 잴 수 있다.
func TestLatencyTimestampsRoundTrip(t *testing.T) {
	s := open(t)
	sig := time.Now().UTC().Add(-90 * time.Second).Truncate(time.Millisecond)
	sub := sig.Add(30 * time.Second)
	fil := sig.Add(49 * time.Second)

	if err := s.RecordOrder(ctx, Order{ID: "o1", IntentID: "i1", Phase: "filled",
		Symbol: sym("005930"), Side: "buy", Mode: protocol.ModeLive,
		SignalTS: &sig, SubmittedAt: &sub, FilledAt: &fil, SlippageBp: 12.5}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Ledger(ctx, LedgerQuery{Mode: protocol.ModeLive})
	if got[0].SignalTS == nil || got[0].FilledAt == nil {
		t.Fatal("지연 타임스탬프가 유실됐다")
	}
	if d := got[0].FilledAt.Sub(*got[0].SignalTS); d != 49*time.Second {
		t.Fatalf("신호→체결 %v", d)
	}
	if got[0].SlippageBp != 12.5 {
		t.Fatalf("slippage=%v", got[0].SlippageBp)
	}
}

// ★ 재시작하면 풀리는 일시정지는 안전장치가 아니다.
func TestGuardsSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cockpit.db")
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)

	s1, err := OpenPath(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.SaveGuards(ctx, GuardState{
		Paused: true, BlockEntryUntil: &until, LiquidateAll: true, Reason: "operator",
	}); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := OpenPath(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	g, err := s2.LoadGuards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Paused || !g.LiquidateAll {
		t.Fatalf("재시작에 가드가 풀렸다: %+v", g)
	}
	if g.BlockEntryUntil == nil || !g.BlockEntryUntil.Equal(until) {
		t.Fatalf("block_entry_until 유실: %v", g.BlockEntryUntil)
	}
}

func TestFreshGuardsAreEmpty(t *testing.T) {
	g, err := open(t).LoadGuards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g.Paused || g.LiquidateAll || g.BlockEntryUntil != nil {
		t.Fatalf("처음 기동인데 가드가 걸려 있다: %+v", g)
	}
}

func TestOutbox(t *testing.T) {
	s := open(t)

	for _, id := range []string{"m1", "m2", "m3"} {
		if err := s.Enqueue(ctx, id, protocol.TypeEventOrder, []byte(`{"x":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	// 같은 id 재삽입은 무시 (at-least-once 대비)
	if err := s.Enqueue(ctx, "m1", protocol.TypeEventOrder, []byte(`{"x":9}`)); err != nil {
		t.Fatal(err)
	}

	pending, err := s.Pending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("대기 %d건", len(pending))
	}
	if pending[0].ID != "m1" || pending[2].ID != "m3" {
		t.Fatalf("순서가 깨졌다: %+v", pending)
	}

	if err := s.MarkSent(ctx, "m1", "m2"); err != nil {
		t.Fatal(err)
	}
	rest, _ := s.Pending(ctx, 10)
	if len(rest) != 1 || rest[0].ID != "m3" {
		t.Fatalf("전송 표시 후 %+v", rest)
	}

	// 큐 정리는 원장을 건드리지 않는다.
	n, err := s.PruneSent(ctx, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("정리 %d건", n)
	}
	if last, _ := s.Pending(ctx, 10); len(last) != 1 {
		t.Fatal("미전송분까지 지웠다")
	}
}
