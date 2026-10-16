package executor

// 몰림 대비 (몰림 재생 2026-10-15) — 늦은 계획을 내지 않고, 계좌 0주는 발행자에게 알리고, 신호가
// 검증은 엔진 락 밖에서 한다.

import (
	"strings"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/fakekiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/quotes"
	"github.com/kh-ssong/yovel-cockpit/internal/reconcile"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

func planAck(p reconcile.Plan, id string) (protocol.AckIntent, bool) {
	for _, a := range p.Acks {
		if a.IntentID == id {
			return a, true
		}
	}
	return protocol.AckIntent{}, false
}

// ★ 틱 시작 때 세운 계획이 내는 순간 진입 마감을 넘겼으면 내지 않는다. 그대로 내면 발행자는
// «만료» 로 아는데 콕핏은 산다 (몰림 재생: 신호 + 39초에 체결).
func TestEntryNotSentPastNotAfter(t *testing.T) {
	t0 := kst("09:00:00")
	h := newKiwoomHarness(t, t0)
	// 주문 직전 조회(신호가 검증)가 시간을 먹는다 — 그사이 마감이 지난다.
	h.x.d.RefMaxDev = 0.5
	h.x.d.RefCheck = func(protocol.Symbol) (float64, bool) { time.Sleep(300 * time.Millisecond); return 1000, true }

	tgt := openT(t0, nil)
	tgt["entry"] = map[string]any{"mode": "market", "not_after": t0.Add(1200 * time.Millisecond), "ref_price": 1000}
	h.sign(t, 1, t0, tgt)

	res := h.x.Tick(ctx, t0.Add(1100*time.Millisecond))
	if res.Expired != 1 || h.fk.State().Calls["kt10000"] != 0 || len(h.eng.Positions()) != 0 {
		t.Fatalf("마감 지난 진입을 냈다: %+v 주문 %d", res, h.fk.State().Calls["kt10000"])
	}
}

// ★ 만료는 원장에 rejected 행으로 **한 번만** 남는다 — 매 틱·재시작해도 한 행. 예전엔 로그뿐이라
// 대조하는 쪽이 «콕핏 행 없음» 을 «미집행» 으로 추정해야 했다.
func TestExpiredEntryLeavesOneLedgerRow(t *testing.T) {
	t0 := kst("09:00:00")
	h := newKiwoomHarness(t, t0)
	notAfter := t0.Add(500 * time.Millisecond)
	tgt := openT(t0, nil)
	tgt["entry"] = map[string]any{"mode": "market", "not_after": notAfter, "ref_price": 1000}
	h.sign(t, 1, t0, tgt)

	rejected := func() []store.Order {
		rows, err := h.st.Ledger(ctx, store.LedgerQuery{Mode: protocol.ModeLive})
		if err != nil {
			t.Fatal(err)
		}
		var out []store.Order
		for _, r := range rows {
			if r.Phase == "rejected" {
				out = append(out, r)
			}
		}
		return out
	}

	// 엔진은 계획을 세웠는데(시각 판정은 executor 가 한다) 내는 순간 마감이 지났다.
	h.x.entryExpired(ctx, protocol.Target{IntentID: aid, Symbol: protocol.Symbol{Exchange: "KRX", Code: "005930"},
		Entry: &protocol.Entry{NotAfter: notAfter}}, "k1", "intraday/x", t0.Add(time.Second), &Result{})
	h.x.Tick(ctx, t0.Add(6*time.Second))
	// 재시작 — 기억이 비어도 행 id 가 (not_after, intent) 로 고정이라 한 행.
	again := New(h.x.d)
	again.entryExpired(ctx, protocol.Target{IntentID: aid, Symbol: protocol.Symbol{Exchange: "KRX", Code: "005930"},
		Entry: &protocol.Entry{NotAfter: notAfter}}, "k1", "intraday/x", t0.Add(9*time.Second), &Result{})

	rows := rejected()
	if len(rows) != 1 {
		t.Fatalf("만료 행 %d개, 기대 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.IntentID != aid || r.Side != "buy" || r.Kid != "k1" || r.Scope != "intraday/x" ||
		!strings.HasPrefix(r.Detail, "expired") || r.Qty != 0 {
		t.Fatalf("만료 행: %+v", r)
	}
	if h.fk.State().Calls["kt10000"] != 0 {
		t.Fatal("만료된 진입을 냈다")
	}
}

// ★ 계좌가 0주라고 하면 오류가 아니라 E_CAPITAL 로 알리고, 잠깐 쉬었다가 다시 산다(비종결).
// 예전엔 per_intent 가 applied 인 채 보유가 안 생겨, 발행자에겐 «진입 만료» 로만 보였다.
func TestAccountShortAcksCapitalThenRetries(t *testing.T) {
	t0 := kst("09:00:00")
	h := newKiwoomHarness(t, t0)
	h.fk.AddFault(fakekiwoom.Fault{API: "kt10000", ReturnCode: 20,
		Msg: "[2000](855056:매수증거금이 부족합니다. 0주 매수가능)", Count: 1})
	h.sign(t, 1, t0, openT(t0, nil))

	at := t0.Add(time.Second)
	res := h.x.Tick(ctx, at)
	if res.CapitalShort != 1 || len(res.Errors) != 0 || len(h.eng.Positions()) != 0 {
		t.Fatalf("0주를 오류로 올렸거나 샀다: %+v", res)
	}
	if a, _ := planAck(h.eng.Plan(at.Add(500*time.Millisecond)), aid); a.Status != "rejected" ||
		len(a.Codes) != 1 || a.Codes[0] != protocol.CodeCapital {
		t.Fatalf("발행자에게 E_CAPITAL 로 안 알렸다: %+v", a)
	}

	// 쉬는 동안은 다시 묻지 않는다 (주문 상한을 다른 신호에 내준다).
	h.x.Tick(ctx, at.Add(time.Second))
	if n := h.fk.State().Calls["kt10000"]; n != 1 {
		t.Fatalf("쉬는 동안 또 냈다: 주문 %d 번", n)
	}
	// 쉬는 시간이 지나면 다시 산다 — 그사이 돈이 돌아왔다 (장애 1회 소진).
	res = h.x.Tick(ctx, at.Add(shortRetry+time.Second))
	if res.Entered != 1 || len(h.eng.Positions()) != 1 {
		t.Fatalf("쉬고 나서 다시 안 샀다: %+v", res)
	}
}

// ★ 신호가 검증은 엔진 락 밖에서 한다 — 엔진은 캐시만 보고, 처음 보는 종목은 집행기가 주문 직전에
// 조회해서 막는다. 조회 결과가 캐시에 남아 다음 판정부터는 엔진이 E_LOCAL_GUARD 로 답한다.
func TestRefGuardAtSubmitWhenEngineHasNoQuote(t *testing.T) {
	t0 := kst("09:00:00")
	h := newKiwoomHarness(t, t0)
	n := 0
	ref := quotes.NewRef(func(protocol.Symbol) (float64, bool) { n++; return 4000, true }, time.Minute,
		func() time.Time { return t0 })
	h.eng = engine.New(engine.Config{
		Mode: protocol.ModeLive, Policy: h.eng.Policy(), TargetMaxAge: 180 * time.Second, MaxOrders: 10,
		EngineBudget: 1_000_000, RefCheckPrice: ref.Cached, RefMaxDev: 0.15,
		Price:  func(protocol.Symbol) (float64, bool) { return 1000, true },
		Market: func(protocol.Symbol) sizing.Market { return sizing.StockMarket() },
		Store:  h.st,
	}, t0)
	h.x.d.Engine = h.eng
	h.x.d.RefCheck, h.x.d.RefMaxDev = ref.Fetch, 0.15

	h.sign(t, 1, t0, openT(t0, nil)) // 신호가 1000 — 시세 4000 과 −75% (엔진은 아직 시세를 모른다)
	if n != 0 {
		t.Fatalf("엔진이 판정 중에 시세를 조회했다 (%d 번) — 락 안 네트워크", n)
	}
	res := h.x.Tick(ctx, t0.Add(time.Second))
	if h.fk.State().Calls["kt10000"] != 0 || len(res.Errors) == 0 {
		t.Fatalf("어긋난 신호가로 샀다: %+v", res)
	}
	if a, _ := planAck(h.eng.Plan(t0.Add(2*time.Second)), aid); a.Status != "rejected" ||
		len(a.Codes) != 1 || a.Codes[0] != protocol.CodeLocalGuard {
		t.Fatalf("다음 판정이 E_LOCAL_GUARD 로 답하지 않았다: %+v", a)
	}
}
