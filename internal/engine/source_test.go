package engine

import (
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
)

// 두 발행자 — flat6 와 otto.
type signer struct {
	kid  string
	priv ed25519.PrivateKey
}

func mkSigner(kid string, b byte) signer {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i) + b
	}
	return signer{kid: kid, priv: ed25519.NewKeyFromSeed(seed)}
}

func (s signer) pub() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

func multiEngine(t *testing.T, books *book.Set, ss ...signer) *Engine {
	t.Helper()
	p := protocol.DefaultPolicy()
	p.TrustedKeys = map[string]ed25519.PublicKey{}
	for _, s := range ss {
		p.TrustedKeys[s.kid] = s.pub()
	}
	return New(Config{
		Mode: protocol.ModePaper, Policy: p, TargetMaxAge: 180 * time.Second, MaxOrders: 10,
		EngineBudget: 1_000_000, Books: books,
		Price:  func(protocol.Symbol) (float64, bool) { return 1000, true },
		Market: func(protocol.Symbol) sizing.Market { return sizing.StockMarket() },
	}, base)
}

// tgt — 목표 하나. want=open 이면 weight 0.5.
func tgt(id, code, want string) map[string]any {
	m := map[string]any{
		"intent_id": id, "slot": "main",
		"symbol": map[string]any{"exchange": "KRX", "code": code}, "side": "long", "want": want,
	}
	if want == "open" {
		m["weight"] = 0.5
		m["entry"] = map[string]any{"mode": "market", "not_after": base.Add(time.Minute)}
		m["exit"] = map[string]any{"stop_price": 900}
	}
	return m
}

func (s signer) snap(t *testing.T, seq uint64, scope string, targets ...map[string]any) []byte {
	t.Helper()
	ts := make([]any, len(targets))
	for i, x := range targets {
		ts[i] = x
	}
	m := map[string]any{
		"v": 1, "typ": "intent.target", "id": "01J9Z8QK3M7X2ABCDEFGHJKMNP",
		"acct": "acc_7f3a", "ts": base, "exp": base.Add(time.Minute), "seq": seq,
		"body": map[string]any{"scope": scope, "as_of_bar": base, "book_state": "normal", "targets": ts},
	}
	raw, _ := json.Marshal(m)
	out, err := protocol.Sign(raw, s.kid, s.priv)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func applied(t *testing.T, e *Engine, raw []byte) protocol.Ack {
	t.Helper()
	ack := e.Apply(raw, base.Add(time.Second))
	if ack.Status != "applied" {
		t.Fatalf("ack=%+v", ack)
	}
	return ack
}


// ★ 한 소스의 스냅샷이 다른 소스의 목표·포지션을 지우지 않는다 (pitwall §4 의 유령 사고).
func TestSourcesDoNotEraseEachOther(t *testing.T) {
	flat6, otto := mkSigner("flat6-1", 3), mkSigner("otto-1", 50)
	e := multiEngine(t, nil, flat6, otto)
	now := base.Add(time.Second)

	applied(t, e, flat6.snap(t, 1, "intraday/d205", tgt("01J9Z8QK3M7X2ABCDEFGHJKMA1", "005930", "open")))
	// flat6 의 포지션이 이미 체결돼 있다고 하자.
	e.SetPositions([]protocol.Position{{IntentID: "01J9Z8QK3M7X2ABCDEFGHJKMA1", Kid: "flat6-1",
		Scope: "intraday/d205", Slot: "main", Symbol: protocol.Symbol{Exchange: "KRX", Code: "005930"},
		Qty: 10, AvgEntryPrice: 1000, StopArmed: 900}})

	// otto 가 자기 목표를 보낸다 — flat6 목표는 한 건도 안 실려 있다.
	applied(t, e, otto.snap(t, 1, "swing/rsi", tgt("01J9Z8QK3M7X2ABCDEFGHJKMB1", "000660", "open")))

	plan := e.Plan(now)
	if len(plan.Orphans) != 0 || len(plan.Exits) != 0 {
		t.Fatalf("★ otto 스냅샷이 flat6 포지션을 유령·청산으로 만들었다: orphans=%v exits=%v", plan.Orphans, plan.Exits)
	}
	if len(plan.Enters) != 1 || plan.Enters[0].Kid != "otto-1" || plan.Enters[0].Scope != "swing/rsi" {
		t.Fatalf("otto 진입 %+v", plan.Enters)
	}
}

// ★ seq 는 소스마다 센다 — 다른 발행자의 큰 seq 때문에 내 스냅샷이 조용히 무시되지 않는다.
func TestSeqIsPerSource(t *testing.T) {
	flat6, otto := mkSigner("flat6-1", 3), mkSigner("otto-1", 50)
	e := multiEngine(t, nil, flat6, otto)
	applied(t, e, otto.snap(t, 9000, "swing/rsi", tgt("01J9Z8QK3M7X2ABCDEFGHJKMB1", "000660", "open")))
	applied(t, e, flat6.snap(t, 1, "intraday/d205", tgt("01J9Z8QK3M7X2ABCDEFGHJKMA1", "005930", "open")))
	// 같은 kid 라도 scope 가 다르면 따로 센다.
	applied(t, e, flat6.snap(t, 1, "scalp/coin", tgt("01J9Z8QK3M7X2ABCDEFGHJKMC1", "000100", "open")))
	// 같은 소스의 되감기는 여전히 무시된다.
	if ack := e.Apply(flat6.snap(t, 1, "intraday/d205"), base.Add(time.Second)); ack.Status != "ignored" {
		t.Fatalf("같은 소스의 같은 seq 가 먹혔다: %+v", ack)
	}
}

// 활성화되지 않은 전략(장부에 없음·꺼짐)은 진입만 막히고 청산은 된다.
func TestInactiveSourceBlocksEntryNotExit(t *testing.T) {
	off := false
	books, err := book.New([]book.Book{
		{Name: "d205", Kid: "flat6-1", Scope: "intraday/d205", Seed: 1_000_000},
		{Name: "coin", Kid: "flat6-1", Scope: "scalp/coin", Seed: 1_000_000, Enabled: &off},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	flat6 := mkSigner("flat6-1", 3)
	e := multiEngine(t, books, flat6)

	ack := applied(t, e, flat6.snap(t, 1, "scalp/coin", tgt("01J9Z8QK3M7X2ABCDEFGHJKMC1", "000100", "open")))
	if len(ack.PerIntent) != 1 || ack.PerIntent[0].Codes[0] != protocol.CodeInactive {
		t.Fatalf("꺼진 전략이 진입했다: %+v", ack.PerIntent)
	}
	ack = applied(t, e, flat6.snap(t, 1, "swing/unknown", tgt("01J9Z8QK3M7X2ABCDEFGHJKMD1", "000200", "open")))
	if ack.PerIntent[0].Codes[0] != protocol.CodeInactive {
		t.Fatalf("장부에 없는 전략이 진입했다: %+v", ack.PerIntent)
	}

	// 꺼진 전략이라도 이미 든 포지션의 청산은 나간다.
	e.SetPositions([]protocol.Position{{IntentID: "01J9Z8QK3M7X2ABCDEFGHJKMC2", Kid: "flat6-1",
		Scope: "scalp/coin", Symbol: protocol.Symbol{Exchange: "KRX", Code: "000100"}, Qty: 1, AvgEntryPrice: 1000}})
	applied(t, e, flat6.snap(t, 2, "scalp/coin", tgt("01J9Z8QK3M7X2ABCDEFGHJKMC2", "000100", "flat")))
	if n := len(e.Plan(base.Add(time.Second)).Exits); n != 1 {
		t.Fatalf("꺼진 전략의 청산이 막혔다: %d", n)
	}
}

// 전략마다 예산이 따로다 — 한 전략이 다 써도 다른 전략은 산다.
func TestBudgetIsPerSource(t *testing.T) {
	books, _ := book.New([]book.Book{
		{Name: "d205", Kid: "flat6-1", Scope: "intraday/d205", Seed: 1_000_000},
		{Name: "klev", Kid: "flat6-1", Scope: "intraday/klev", Seed: 1_000_000},
	}, 0)
	flat6 := mkSigner("flat6-1", 3)
	e := multiEngine(t, books, flat6)
	e.SetPositions([]protocol.Position{{IntentID: "01J9Z8QK3M7X2ABCDEFGHJKMA0", Kid: "flat6-1",
		Scope: "intraday/d205", Symbol: protocol.Symbol{Exchange: "KRX", Code: "000001"}, Qty: 1000, AvgEntryPrice: 1000}})

	full := tgt("01J9Z8QK3M7X2ABCDEFGHJKMA0", "000001", "open")
	ack := applied(t, e, flat6.snap(t, 1, "intraday/d205", full, tgt("01J9Z8QK3M7X2ABCDEFGHJKMA1", "005930", "open")))
	for _, a := range ack.PerIntent {
		if a.IntentID == "01J9Z8QK3M7X2ABCDEFGHJKMA1" && (len(a.Codes) == 0 || a.Codes[0] != protocol.CodeCapital) {
			t.Fatalf("다 쓴 d205 가 또 샀다: %+v", a)
		}
	}
	ack = applied(t, e, flat6.snap(t, 1, "intraday/klev", tgt("01J9Z8QK3M7X2ABCDEFGHJKMB1", "122630", "open")))
	if ack.PerIntent[0].Status != "applied" {
		t.Fatalf("d205 가 예산을 다 써서 klev 가 막혔다: %+v", ack.PerIntent)
	}
}
