package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// v4 → v5: 옛 로트의 모드를 그 로트의 원장 주문에서 가져온다. 주문이 없는 로트는 빈 값(모름).
func TestMigrateV4FillsIntentModeFromOrders(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cockpit.db")
	s, err := OpenPath(p)
	if err != nil {
		t.Fatal(err)
	}
	// v4 로 되돌린다 — mode 칸이 없고 버전이 4.
	for _, q := range []string{
		`ALTER TABLE intents DROP COLUMN mode`,
		`DELETE FROM schema_version WHERE version = 5`,
		`INSERT INTO schema_version(version) VALUES (4)`,
	} {
		if _, err := s.DB().Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	for _, id := range []string{"LIVE", "PAPER", "NOORDER"} {
		if _, err := s.DB().Exec(`INSERT INTO intents (intent_id, slot, exchange, code, side, updated_at)
			VALUES (?, 's', 'KRX', '005930', 'long', ?)`, id, nowStr()); err != nil {
			t.Fatal(err)
		}
	}
	for id, m := range map[string]protocol.Mode{"LIVE": protocol.ModeLive, "PAPER": protocol.ModePaper} {
		if err := s.RecordOrder(ctx, Order{ID: "o-" + id, IntentID: id, Phase: "filled", Symbol: sym("005930"),
			Side: "buy", Qty: 1, Price: 1000, Mode: m, Source: SourceBot}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	s, err = OpenPath(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	modes, err := s.OpenIntentModes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if modes[protocol.ModeLive] != 1 || modes[protocol.ModePaper] != 1 || modes[""] != 1 {
		t.Fatalf("모드 추론 %+v", modes)
	}
}

// 로트의 모드는 처음 값을 지킨다 — stop 조임·TP 위임은 모드를 모른 채 부른다.
func TestIntentModeIsKept(t *testing.T) {
	s := open(t)
	in := Intent{IntentID: "A", Slot: "s", Symbol: sym("005930"), Side: "long", Qty: 1, Mode: protocol.ModeLive}
	if err := s.UpsertIntent(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Mode, in.StopArmed = "", 900
	if err := s.UpsertIntent(ctx, in); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.OpenIntentModes(ctx); m[protocol.ModeLive] != 1 {
		t.Fatalf("모드가 지워졌다 %+v", m)
	}
}

// 보내기 전 기록 → 접수: 한 트랜잭션으로 바뀐다 (임시는 닫히고 진짜만 열린다).
func TestConfirmWorkingReplacesTemp(t *testing.T) {
	s := open(t)
	temp := WorkingOrder{OrderID: "pre-entry-A-1", IntentID: "A", Purpose: "entry", Unconfirmed: true}
	if err := s.PutWorking(ctx, temp); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmWorking(ctx, temp.OrderID, WorkingOrder{OrderID: "0001234", IntentID: "A", Purpose: "entry"}); err != nil {
		t.Fatal(err)
	}
	ws, err := s.OpenWorking(ctx)
	if err != nil || len(ws) != 1 || ws[0].OrderID != "0001234" || ws[0].Unconfirmed {
		t.Fatalf("%+v %v", ws, err)
	}
}

// paper 현금 흐름 — 매수는 대금+수수료만큼 빠지고 매도는 대금−수수료만큼 들어온다. 가격 미상 행은 무시.
func TestPaperCashFlow(t *testing.T) {
	s := open(t)
	rec := func(id, phase, side string, qty, price, fee float64, oid string, m protocol.Mode) {
		if err := s.RecordOrder(ctx, Order{ID: id, IntentID: "I" + id, Phase: phase, Symbol: sym("005930"), Side: side,
			Qty: qty, Price: price, FeeKRW: fee, BrokerOrderID: oid, Mode: m, Source: SourceBot,
			CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	rec("1", "filled", "buy", 10, 1000, 15, "paper-3", protocol.ModePaper)
	rec("2", "exit_filled", "sell", 10, 1100, 20, "paper-7", protocol.ModePaper)
	rec("3", "exit_filled", "sell", 5, 0, 0, "", protocol.ModePaper) // 사후 감지 — 가격 미상
	rec("4", "filled", "buy", 1, 999999, 0, "LIVE-1", protocol.ModeLive)
	net, seq, err := s.PaperCashFlow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := -(10*1000 + 15.0) + (10*1100 - 20.0); net != want || seq != 7 {
		t.Fatalf("net %v (기대 %v) seq %d", net, want, seq)
	}
}
