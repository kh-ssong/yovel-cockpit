package reconcile

import (
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
)

var now = time.Date(2026, 8, 15, 0, 5, 0, 0, time.UTC)

func sym(code string) protocol.Symbol { return protocol.Symbol{Exchange: "KRX", Code: code} }

func openTarget(id, code string) protocol.Target { return openTargetW(id, code, 0.5) }

func openTargetW(id, code string, weight float64) protocol.Target {
	na := now.Add(time.Minute)
	return protocol.Target{
		IntentID: id, Slot: "s1", Symbol: sym(code), Side: "long", Want: protocol.WantOpen,
		Weight: weight,
		Entry:  &protocol.Entry{Mode: "market", NotAfter: na},
		Exit:   &protocol.Exit{StopPrice: 900, TpPrice: 1200, TpDelegate: true},
	}
}

func flatTarget(id, code string) protocol.Target {
	return protocol.Target{
		IntentID: id, Slot: "s1", Symbol: sym(code), Side: "long", Want: protocol.WantFlat,
	}
}

func held(id, code string, stop float64) protocol.Position {
	return protocol.Position{
		IntentID: id, Slot: "s1", Symbol: sym(code), Qty: 10, AvgEntryPrice: 1000, StopArmed: stop,
	}
}

func opts() Options {
	return Options{
		Now:          now,
		EntryAllowed: true,
		MaxOrders:    5,
		Budget:       1_000_000,
		Price:        func(protocol.Symbol) (float64, bool) { return 1000, true },
		Market:       func(protocol.Symbol) sizing.Market { return sizing.StockMarket() },
	}
}

func book(targets ...protocol.Target) protocol.IntentTarget {
	return protocol.IntentTarget{AsOfBar: now, BookState: protocol.BookNormal, Targets: targets}
}

func ackFor(p Plan, id string) (protocol.AckIntent, bool) {
	for _, a := range p.Acks {
		if a.IntentID == id {
			return a, true
		}
	}
	return protocol.AckIntent{}, false
}

func TestEnterWhenMissing(t *testing.T) {
	p := Build(book(openTarget("a", "005930")), nil, opts())
	if len(p.Enters) != 1 {
		t.Fatalf("enters=%+v", p.Enters)
	}
	// 예산 50만원 / 1000원 = 500주
	if p.Enters[0].Qty != 500 {
		t.Fatalf("qty=%v", p.Enters[0].Qty)
	}
	if a, _ := ackFor(p, "a"); a.Status != "applied" {
		t.Fatalf("ack=%+v", a)
	}
}

func TestFlatMeansExit(t *testing.T) {
	p := Build(book(flatTarget("a", "005930")), []protocol.Position{held("a", "005930", 900)}, opts())
	if len(p.Exits) != 1 || len(p.Enters) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestFlatWithoutPositionIsNoop(t *testing.T) {
	p := Build(book(flatTarget("a", "005930")), nil, opts())
	if len(p.Exits) != 0 {
		t.Fatalf("없는 포지션을 팔려 한다: %+v", p.Exits)
	}
	if a, _ := ackFor(p, "a"); a.Status != "noop" {
		t.Fatalf("ack=%+v", a)
	}
}

func TestStopUpdateWhenHeld(t *testing.T) {
	p := Build(book(openTarget("a", "005930")), []protocol.Position{held("a", "005930", 880)}, opts())
	if len(p.Enters) != 0 {
		t.Fatal("이미 들고 있는데 또 샀다")
	}
	if len(p.StopUpdates) != 1 || p.StopUpdates[0].To != 900 {
		t.Fatalf("%+v", p.StopUpdates)
	}
	if len(p.TpUpdates) != 1 {
		t.Fatalf("tp 위임이 안 걸렸다: %+v", p.TpUpdates)
	}
}

func TestNoStopUpdateWhenUnchanged(t *testing.T) {
	p := Build(book(openTarget("a", "005930")), []protocol.Position{held("a", "005930", 900)}, opts())
	if len(p.StopUpdates) != 0 {
		t.Fatalf("같은 값인데 갱신했다: %+v", p.StopUpdates)
	}
}

// ★ 만료돼도 stop 갱신은 계속 반영한다. 청산 쪽을 막을 이유가 없다.
func TestStopUpdatesSurviveExpiry(t *testing.T) {
	o := opts()
	o.EntryAllowed = false
	p := Build(book(openTarget("a", "005930")), []protocol.Position{held("a", "005930", 880)}, o)
	if len(p.StopUpdates) != 1 {
		t.Fatalf("만료됐다고 stop 갱신을 버렸다: %+v", p)
	}
}

// ★ 목표에 없는 실보유는 자동 청산하지 않는다.
// 서버가 한 번 잘못 계산하거나 스냅샷이 잘리면 사용자의 수동 보유분까지 털린다.
func TestOrphanIsReportedNotSold(t *testing.T) {
	p := Build(book(openTarget("a", "005930")), []protocol.Position{held("z", "000660", 0)}, opts())
	if len(p.Exits) != 0 {
		t.Fatalf("유령 포지션을 팔았다: %+v", p.Exits)
	}
	if len(p.Orphans) != 1 {
		t.Fatalf("유령을 보고하지 않았다: %+v", p.Orphans)
	}
	if a, _ := ackFor(p, "z"); len(a.Codes) == 0 || a.Codes[0] != protocol.CodeOrphan {
		t.Fatalf("ack=%+v", a)
	}
}

func TestExpiredEntryIsNotEntered(t *testing.T) {
	o := opts()
	o.Now = now.Add(2 * time.Minute) // not_after 경과
	p := Build(book(openTarget("a", "005930")), nil, o)
	if len(p.Enters) != 0 {
		t.Fatal("철 지난 진입을 집행했다")
	}
	a, _ := ackFor(p, "a")
	if a.Status != "expired" {
		t.Fatalf("ack=%+v", a)
	}
}

// 일시정지·서킷브레이커·halted 는 진입만 막고 청산은 막지 않는다.
func TestLocalBlocksStopEntriesButNotExits(t *testing.T) {
	until := now.Add(time.Hour)
	cases := []struct {
		name string
		mut  func(*Options)
		bs   protocol.BookState
		code protocol.RejectCode
	}{
		{"paused", func(o *Options) { o.Paused = true }, protocol.BookNormal, protocol.CodePaused},
		{"block_entry", func(o *Options) { o.BlockEntryUntil = &until }, protocol.BookNormal, protocol.CodePaused},
		{"circuit", func(o *Options) { o.CircuitBreaker = true }, protocol.BookNormal, protocol.CodeLocalGuard},
		{"halted", func(o *Options) {}, protocol.BookHalted, protocol.CodeLocalGuard},
		// 모르는 book_state 는 halted 로 읽어야 한다 (안전한 쪽).
		{"모르는 book_state", func(o *Options) {}, protocol.BookState("panic_v2"), protocol.CodeLocalGuard},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := opts()
			c.mut(&o)
			b := book(openTarget("a", "005930"), flatTarget("b", "000660"))
			b.BookState = c.bs

			p := Build(b, []protocol.Position{held("b", "000660", 900)}, o)
			if len(p.Enters) != 0 {
				t.Fatalf("막혔는데 진입했다: %+v", p.Enters)
			}
			if len(p.Exits) != 1 {
				t.Fatalf("청산까지 막혔다: %+v", p.Exits)
			}
			a, _ := ackFor(p, "a")
			if len(a.Codes) == 0 || a.Codes[0] != c.code {
				t.Fatalf("ack=%+v, 기대 %s", a, c.code)
			}
		})
	}
}

// ★ 주문 상한은 폭주를 막는 장치지 청산을 미루는 장치가 아니다.
func TestOrderCapNeverDropsExits(t *testing.T) {
	o := opts()
	o.MaxOrders = 2

	var targets []protocol.Target
	var positions []protocol.Position
	for _, id := range []string{"x1", "x2", "x3"} { // 청산 3건
		targets = append(targets, flatTarget(id, "0006"+id))
		positions = append(positions, held(id, "0006"+id, 900))
	}
	// ★ 진입은 가볍게 잡는다 — 여기서 시험하는 것은 주문 상한이지 예산이 아니다.
	// 무겁게 두면 예산 쪽에서 먼저 걸려(E_CAPITAL) 상한 로직이 실행되지도 않는다.
	for _, id := range []string{"e1", "e2"} { // 진입 2건
		targets = append(targets, openTargetW(id, "0059"+id, 0.1))
	}

	p := Build(book(targets...), positions, o)

	if len(p.Exits) != 3 {
		t.Fatalf("상한 때문에 청산이 잘렸다: %d건", len(p.Exits))
	}
	if len(p.Enters) != 0 {
		t.Fatalf("상한을 넘겨 진입했다: %d건", len(p.Enters))
	}
	if p.DroppedEnters != 2 {
		t.Fatalf("절단을 보고하지 않았다: %d", p.DroppedEnters)
	}
	// 잘린 진입은 침묵이 아니라 코드로 보고한다.
	for _, id := range []string{"e1", "e2"} {
		a, _ := ackFor(p, id)
		if len(a.Codes) == 0 || a.Codes[0] != protocol.CodeRate {
			t.Fatalf("%s ack=%+v", id, a)
		}
	}
}

func TestNoPriceMeansNoEntry(t *testing.T) {
	o := opts()
	o.Price = func(protocol.Symbol) (float64, bool) { return 0, false }
	p := Build(book(openTarget("a", "005930")), nil, o)
	if len(p.Enters) != 0 {
		t.Fatal("가격을 모르는데 샀다")
	}
	if a, _ := ackFor(p, "a"); a.Codes[0] != protocol.CodeSymbol {
		t.Fatalf("ack=%+v", a)
	}
}

func TestZeroSharesRejectsWithCapital(t *testing.T) {
	o := opts()
	o.Budget = 100 // 1주도 못 산다
	p := Build(book(openTarget("a", "005930")), nil, o)
	if len(p.Enters) != 0 {
		t.Fatal("0주인데 주문을 냈다")
	}
	if a, _ := ackFor(p, "a"); a.Codes[0] != protocol.CodeCapital {
		t.Fatalf("ack=%+v", a)
	}
}

func TestLimitEntryUsesLimitPrice(t *testing.T) {
	tg := openTarget("a", "005930")
	tg.Entry = &protocol.Entry{Mode: "limit", LimitPrice: 500, NotAfter: now.Add(time.Minute)}
	p := Build(book(tg), nil, opts())
	if len(p.Enters) != 1 || p.Enters[0].Price != 500 {
		t.Fatalf("%+v", p.Enters)
	}
	if p.Enters[0].Qty != 1000 { // 50만원 / 500원
		t.Fatalf("qty=%v", p.Enters[0].Qty)
	}
}

// ★ 종결된 목표로 재진입하지 않는다.
// retained 목표는 재접속마다 그대로 다시 오므로, 이 가드가 없으면 stop 에 털린 자리에
// 진입 창(not_after)이 남아 있는 동안 같은 목표로 곧바로 재진입한다.
func TestTerminalIntentIsNotReentered(t *testing.T) {
	o := opts()
	o.Terminal = func(id string) bool { return id == "a" }

	p := Build(book(openTarget("a", "005930")), nil, o)
	if len(p.Enters) != 0 {
		t.Fatalf("종결된 목표로 재진입했다: %+v", p.Enters)
	}
	a, _ := ackFor(p, "a")
	if a.Status != "noop" {
		t.Fatalf("ack=%+v — 에러는 아니지만 침묵도 아니어야 한다", a)
	}
	if len(a.Codes) == 0 || a.Codes[0] != protocol.CodeTerminal {
		t.Fatalf("왜 안 샀는지 안 남겼다: %+v", a)
	}
}

// 가드가 꺼져 있으면(nil) 재진입한다 — 그래서 배선을 빠뜨리면 안 된다는 걸 테스트로 고정한다.
func TestWithoutTerminalGuardItReenters(t *testing.T) {
	p := Build(book(openTarget("a", "005930")), nil, opts())
	if len(p.Enters) != 1 {
		t.Fatalf("Terminal 미배선 시 동작이 바뀌었다: %+v", p)
	}
}

// ★ 신호를 낸 쪽이 기준가를 실어 보내면 클라는 시세를 다시 조회하지 않는다.
// 왕복이 줄기도 하지만, 진짜 이유는 **신호를 낸 가격과 사이징한 가격이 갈리지 않는 것**이다.
func TestRefPriceIsUsedInsteadOfQuote(t *testing.T) {
	o := opts()
	o.Price = func(protocol.Symbol) (float64, bool) {
		t.Fatal("ref_price 가 있는데 시세를 조회했다")
		return 0, false
	}
	tg := openTarget("a", "005930")
	tg.Entry.RefPrice = 500 // 신호 시점 가격

	p := Build(book(tg), nil, o)
	if len(p.Enters) != 1 {
		t.Fatalf("%+v", p)
	}
	if p.Enters[0].Price != 500 || p.Enters[0].Qty != 1000 { // 50만 / 500
		t.Fatalf("%+v", p.Enters[0])
	}
}

// ref_price 가 없으면 예전처럼 조회한다 (선택 필드라 옛 발행자도 그대로 동작).
func TestWithoutRefPriceFallsBackToQuote(t *testing.T) {
	p := Build(book(openTarget("a", "005930")), nil, opts())
	if len(p.Enters) != 1 || p.Enters[0].Price != 1000 {
		t.Fatalf("%+v", p.Enters)
	}
}

// ★ 예산의 분모는 엔진 하나다 — 슬롯이 몇 개든 합이 예산을 넘지 않는다 (protocol.md §7.1).
//
// 옛 동작(슬롯당 자본)에서는 이 케이스가 예산의 2배를 샀다. 스키마도 서명도 전부 통과하므로
// 어디에서도 안 걸렸다 — 그래서 이 gate 가 필요하다.
func TestBudgetIsEngineWideNotPerSlot(t *testing.T) {
	a := openTargetW("a", "005930", 0.5)
	a.Slot = "slot_a"
	b := openTargetW("b", "000660", 0.5)
	b.Slot = "slot_b"

	p := Build(book(a, b), nil, opts())
	if len(p.Enters) != 2 {
		t.Fatalf("enters=%d %+v", len(p.Enters), p.Acks)
	}
	var sum float64
	for _, e := range p.Enters {
		sum += e.Notional
	}
	if sum > 1_000_000 {
		t.Fatalf("예산 100만인데 %v 원어치 샀다 (슬롯당으로 계산했다)", sum)
	}
}

// ★ 보유분이 예산을 먹는다. 안 그러면 스냅샷이 재발행될 때마다 예산 전액이 새로 생긴다
// — 재발행은 이 프로토콜의 기본 동작이라 매 봉 반복된다.
func TestHeldPositionsConsumeBudget(t *testing.T) {
	pos := held("old", "000660", 900)
	pos.Qty = 600 // 60만원어치 이미 들고 있다

	// 목표에 그 포지션이 그대로 있고(유지), 새 진입이 50만원어치 온다 → 110만 > 예산 100만
	keep := openTarget("old", "000660")
	p := Build(book(keep, openTarget("new", "005930")), []protocol.Position{pos}, opts())

	if len(p.Enters) != 0 {
		t.Fatalf("예산을 넘겨 샀다: %+v", p.Enters)
	}
	if a, _ := ackFor(p, "new"); len(a.Codes) == 0 || a.Codes[0] != protocol.CodeCapital {
		t.Fatalf("ack=%+v", a)
	}
}

// 평단을 모르는 보유(원장이 아직 못 채운 것)도 예산을 먹는다.
// ★ "모르는 것"을 "없는 것"으로 읽으면 그만큼 더 사게 된다.
func TestUnknownAvgPriceStillConsumesBudget(t *testing.T) {
	pos := held("old", "000660", 900)
	pos.Qty, pos.AvgEntryPrice = 600, 0 // 평단 미상 → 현재가(1000)로 값을 매겨야 한다

	p := Build(book(openTarget("old", "000660"), openTarget("new", "005930")),
		[]protocol.Position{pos}, opts())

	if len(p.Enters) != 0 {
		t.Fatalf("평단 미상을 0원으로 세고 샀다: %+v", p.Enters)
	}
}

// ★ 예산으로 자를 때는 targets[] 배열 순서(= 엔진이 정한 우선순위)를 따른다 (protocol.md §6).
// 도착 순서나 종목 코드로 자르면 엔진이 무엇을 우선하는지 표현할 방법이 사라진다.
func TestBudgetCutFollowsTargetOrder(t *testing.T) {
	first := openTargetW("first", "005930", 0.4)
	second := openTargetW("second", "000660", 0.4)
	third := openTargetW("third", "035720", 0.4) // 합 120% → 하나는 못 산다

	p := Build(book(first, second, third), nil, opts())
	if len(p.Enters) != 2 {
		t.Fatalf("enters=%d", len(p.Enters))
	}
	if p.Enters[0].Target.IntentID != "first" || p.Enters[1].Target.IntentID != "second" {
		t.Fatalf("우선순위를 안 지켰다: %v %v", p.Enters[0].Target.IntentID, p.Enters[1].Target.IntentID)
	}
	if a, _ := ackFor(p, "third"); len(a.Codes) == 0 || a.Codes[0] != protocol.CodeCapital {
		t.Fatalf("잘린 것을 조용히 버렸다: %+v", a)
	}

	// 순서를 뒤집으면 살아남는 쪽도 뒤집힌다 — 종목·금액이 아니라 순서가 기준이라는 증거.
	q := Build(book(third, second, first), nil, opts())
	if q.Enters[0].Target.IntentID != "third" {
		t.Fatalf("배열 순서가 아니라 다른 기준으로 골랐다: %+v", q.Enters[0].Target.IntentID)
	}
}

// 예산을 넘는 진입은 **거절**이지 축소가 아니다. 줄여 사면 엔진이 지시한 적 없는 비중이 된다.
func TestOverBudgetIsRejectedNotShrunk(t *testing.T) {
	pos := held("old", "000660", 900)
	pos.Qty = 900 // 90만 사용 중 → 남은 예산 10만

	p := Build(book(openTarget("old", "000660"), openTargetW("new", "005930", 0.5)),
		[]protocol.Position{pos}, opts())

	for _, e := range p.Enters {
		if e.Target.IntentID == "new" {
			t.Fatalf("남은 예산에 맞춰 크기를 깎아 샀다: qty=%v (지시받은 비중이 아니다)", e.Qty)
		}
	}
}
