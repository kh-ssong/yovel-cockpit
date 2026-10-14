package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// ★ 재시작해도 옛 seq 는 받지 않는다 — 시계 오차 허용(2분) 안의 옛 서명 메시지가 옛 stop·TP 를 되돌리지 못하게.
// 단, 마지막으로 받은 것과 **같은** seq 는 받는다 (재접속 때 발행자가 그대로 다시 보낸다).
func TestOldSeqIgnoredAfterRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "cockpit.db")
	now := base.Add(time.Second)
	msg := func(seq uint64, priv []byte) []byte {
		return signEnv(t, targetMsg(seq, base, base.Add(time.Minute), base.Add(time.Minute), base), priv)
	}

	e1, priv, st1 := newPersistentEngine(t, db)
	if ack := e1.Apply(msg(5, priv), now); ack.Status != "applied" {
		t.Fatalf("%+v", ack)
	}
	st1.Close()

	e2, priv2, st2 := newPersistentEngine(t, db)
	defer st2.Close()
	if ack := e2.Apply(msg(3, priv2), now); ack.Status != "ignored" {
		t.Fatalf("★ 재시작 뒤 옛 seq 3 을 받았다: %+v", ack)
	}
	if ack := e2.Apply(msg(5, priv2), now); ack.Status != "applied" {
		t.Fatalf("재접속 재전송(같은 seq)을 버렸다 — 변경 시에만 발행하는 발행자의 목표가 빈다: %+v", ack)
	}
	if ack := e2.Apply(msg(5, priv2), now); ack.Status != "ignored" {
		t.Fatalf("같은 seq 를 두 번 받았다: %+v", ack)
	}
}

// ★ 받아들인 de-risk 의 nonce 는 재시작해도 기억한다 — resume 뒤 재시작했는데 옛 pause 가 다시 걸리면 안 된다.
func TestDeriskReplayBlockedAfterRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "cockpit.db")
	now := base.Add(time.Second)
	pause := deriskMsg("n-pause", "pause", base, base.Add(5*time.Minute))

	e1, _, st1 := newPersistentEngine(t, db)
	e1.Apply(pause, now)
	e1.Apply(deriskMsg("n-resume", "resume", base, base.Add(5*time.Minute)), now)
	if e1.Snapshot().Guards.Paused {
		t.Fatal("resume 이 안 먹었다")
	}
	st1.Close()

	e2, _, st2 := newPersistentEngine(t, db)
	defer st2.Close()
	ack := e2.Apply(pause, now.Add(time.Second))
	if ack.Status != "rejected" || len(ack.Codes) == 0 || ack.Codes[0] != protocol.CodeReplay {
		t.Fatalf("★ 재시작 뒤 옛 pause 를 다시 받았다: %+v", ack)
	}
	if e2.Snapshot().Guards.Paused {
		t.Fatal("옛 pause 가 다시 걸렸다")
	}
}

// ★ 재시작 직전에 끝난 로트도 recent_closes 에 보인다 — 발행자가 그 결말을 놓치지 않게.
func TestRecentClosesSurviveRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "cockpit.db")
	ctx := context.Background()
	e1, _, st := newPersistentEngine(t, db)
	_ = e1
	sam := protocol.Symbol{Exchange: "KRX", Code: "005930"}
	closeLot := func(id string, at time.Time, price float64) {
		if err := st.UpsertIntent(ctx, store.Intent{IntentID: id, Slot: "s", Kid: "flat6-1", Scope: "intraday/d205",
			Symbol: sam, Side: "long", Qty: 10, AvgEntryPrice: 1000, Mode: protocol.ModePaper}); err != nil {
			t.Fatal(err)
		}
		if price > 0 {
			if err := st.RecordOrder(ctx, store.Order{ID: "o-" + id, IntentID: id, Phase: "exit_filled", Symbol: sam,
				Side: "sell", Qty: 10, Price: price, ExitReason: "tp", Mode: protocol.ModePaper, Source: store.SourceBot}); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.CloseIntent(ctx, id, "tp", at); err != nil {
			t.Fatal(err)
		}
	}
	closeLot("RECENT", base.Add(-5*time.Minute), 1020)
	closeLot("UNKNOWN", base.Add(-time.Minute), 0)
	closeLot("OLD", base.Add(-40*time.Minute), 990)
	st.Close()

	e2, _, st2 := newPersistentEngine(t, db)
	defer st2.Close()
	cs := e2.SnapshotAt(base).RecentCloses
	if len(cs) != 2 || cs[0].IntentID != "RECENT" || cs[1].IntentID != "UNKNOWN" {
		t.Fatalf("최근 종결 %+v", cs)
	}
	if cs[0].Price != 1020 || cs[0].Reason != "tp" || cs[0].Scope != "intraday/d205" ||
		cs[0].RealizedPct < 0.0199 || cs[0].RealizedPct > 0.0201 {
		t.Fatalf("결말이 틀렸다 %+v", cs[0])
	}
	if cs[1].Price != 0 || cs[1].RealizedPct != 0 {
		t.Fatalf("가격 미상을 0%% 로 지어냈다 %+v", cs[1])
	}
}

// ★ 다른 모드로 연 로트가 열려 있으면 기동하지 않는다 — paper 로트가 live 계좌의 진짜 주식을 판다.
func TestRestoreRefusesOtherModeLots(t *testing.T) {
	db := filepath.Join(t.TempDir(), "cockpit.db")
	_, _, st := newPersistentEngine(t, db) // paper
	if err := st.UpsertIntent(context.Background(), store.Intent{IntentID: "P1", Slot: "s",
		Symbol: protocol.Symbol{Exchange: "KRX", Code: "005930"}, Side: "long", Qty: 500, AvgEntryPrice: 1000,
		Mode: protocol.ModePaper}); err != nil {
		t.Fatal(err)
	}
	live := New(Config{Mode: protocol.ModeLive, Policy: protocol.DefaultPolicy(), Store: st}, base)
	err := live.Restore(context.Background())
	if err == nil || !strings.Contains(err.Error(), "paper") {
		t.Fatalf("★ paper 로트가 열린 원장으로 live 가 떴다: %v", err)
	}
	// 같은 모드면 뜬다.
	again := New(Config{Mode: protocol.ModePaper, Policy: protocol.DefaultPolicy(), Store: st}, base)
	if err := again.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.Close()
}
