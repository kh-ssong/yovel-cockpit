package alloc

import (
	"math"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
)

// 2026-10-19 (월) — KST 시각으로 만든다.
func kst(hm string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-10-19 "+hm, session.KST)
	return t
}

func f(v float64) *float64 { return &v }

func set(t *testing.T, a book.Alloc, books ...book.Book) *book.Set {
	t.Helper()
	s, err := book.NewAlloc(books, 0, a)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// docs/allocation.md §5 의 live 예시 — 밤 장부는 밤 한도를, 낮 장부는 capital 전체를 비중대로 나눈다.
func TestComputeShares(t *testing.T) {
	off := false
	s := set(t, book.Alloc{Capital: 1_460_000, OvernightPct: 70, BorrowX: 2, ReserveTrades: 10},
		book.Book{Name: "s60", Scope: "kiwoom/swing_60m"},
		book.Book{Name: "s30", Scope: "kiwoom/swing_30m", Hold: book.HoldCarry},
		book.Book{Name: "etf", Scope: "kiwoom/swing_etf_60m", Share: f(2)},
		book.Book{Name: "gap", Scope: "kiwoom/gapdown_main", Hold: book.HoldDay},
		book.Book{Name: "off", Scope: "kiwoom/off", Hold: book.HoldDay, Enabled: &off},
		book.Book{Name: "f60", Scope: "kiwoom/klev_push10_f60", Seed: 300_000, MaxQty: 1},
	)
	p := Compute(s, kst("08:50:00"))
	if p.Day != "2026-10-19" || !near(p.NightCap, 1_022_000) {
		t.Fatalf("plan %+v", p)
	}
	b := func(n string) Book { x, _ := p.Book(n); return x }
	// 밤: 102.2만을 1:1:2 로.
	if !near(b("s60").Base, 255_500) || !near(b("etf").Base, 511_000) || !near(b("s60").Cap, 511_000) {
		t.Fatalf("밤 몫 s60=%+v etf=%+v", b("s60"), b("etf"))
	}
	// 낮: 꺼진 장부는 몫을 안 나눠 가진다 → gap 혼자 capital 전체.
	if !near(b("gap").Base, 1_460_000) || b("off").Base != 0 {
		t.Fatalf("낮 몫 gap=%+v off=%+v", b("gap"), b("off"))
	}
	if f60 := b("f60"); !f60.Fixed || f60.Base != 300_000 || f60.Cap != 300_000 {
		t.Fatalf("고정 장부 %+v", f60)
	}
}

func TestComputeOffIsNil(t *testing.T) {
	s, _ := book.New([]book.Book{{Name: "a", Scope: "a", Seed: 1}}, 0)
	if Compute(s, kst("08:50:00")) != nil {
		t.Fatal("capital 이 없으면 nil")
	}
}

func target(id string, timeExit *time.Time) protocol.Target {
	return protocol.Target{IntentID: id, Symbol: protocol.Symbol{Exchange: "KRX", Code: "005930"},
		Want: protocol.WantOpen, Exit: &protocol.Exit{StopPrice: 1, TimeExitAt: timeExit}}
}

func TestNightOrDay(t *testing.T) {
	now := kst("10:00:00")
	today, tomorrow := kst("15:15:00"), kst("15:15:00").AddDate(0, 0, 1)
	if NightTarget(target("a", &today), now) || HoldOf(target("a", &today), now) != "day" {
		t.Fatal("오늘 청산이면 낮")
	}
	if !NightTarget(target("b", &tomorrow), now) || !NightTarget(target("c", nil), now) {
		t.Fatal("내일 이후·기한 없음이면 밤")
	}
	coin := target("d", nil)
	coin.Symbol.Exchange = "UPBIT"
	if NightTarget(coin, now) {
		t.Fatal("업비트는 밤/낮을 안 가른다")
	}
}

func gatePlan(t *testing.T) *Plan {
	return Compute(set(t, book.Alloc{Capital: 1_000_000, OvernightPct: 50, BorrowX: 2, ReserveTrades: 10},
		book.Book{Name: "swing", Scope: "kiwoom/swing"},
		book.Book{Name: "day", Scope: "kiwoom/day", Hold: book.HoldDay},
	), kst("08:50:00"))
}

// ② 밤 한도는 거절 — 밤 장부 전체 합이 capital × overnight_pct 를 못 넘는다.
func TestGateOvernightCapRejects(t *testing.T) {
	p := gatePlan(t)
	now := kst("10:00:00")
	g := NewGate(p, now, []Held{{Book: "swing", Value: 480_000, Night: true}}, nil)
	if q, why := g.Admit("swing", target("n1", nil), 25, 1000, sizing.StockMarket()); q != 0 || why != ReasonOvernightCap {
		t.Fatalf("밤 한도 넘는 밤 진입: q=%v why=%s", q, why)
	}
	today := kst("15:15:00")
	if q, why := g.Admit("day", target("d1", &today), 25, 1000, sizing.StockMarket()); q != 25 || why != "" {
		t.Fatalf("낮 진입은 밤 한도와 무관: q=%v why=%s", q, why)
	}
	if ev := g.Events(); len(ev) != 1 || ev[0].Reason != ReasonOvernightCap || ev[0].KRW != 25_000 {
		t.Fatalf("events %+v", ev)
	}
}

// ③ 낮 진입은 스윙 예비분을 못 건드린다 — 줄여 산다.
func TestGateReserveShrinksDayEntry(t *testing.T) {
	p := gatePlan(t)
	now := kst("10:00:00")
	// 밤 장부 swing 몫 50만 × weight 0.05(모름) = 건당 2.5만 × 10건 = 예비분 25만.
	g := NewGate(p, now, nil, nil)
	if !near(g.Reserve(), 250_000) {
		t.Fatalf("예비분 %v", g.Reserve())
	}
	today := kst("15:15:00")
	q, why := g.Admit("day", target("d1", &today), 900, 1000, sizing.StockMarket())
	if q != 750 || why != ReasonReserve {
		t.Fatalf("낮 진입 q=%v why=%s — 100만 − 예비분 25만 = 750주", q, why)
	}
	// 밤 진입은 예비분을 쓴다.
	if q, why := g.Admit("swing", target("n1", nil), 25, 1000, sizing.StockMarket()); q != 25 || why != "" {
		t.Fatalf("밤 진입 q=%v why=%s", q, why)
	}
	if !near(g.Free(), 225_000) || !near(g.Reserve(), 225_000) {
		t.Fatalf("free=%v reserve=%v", g.Free(), g.Reserve())
	}
	ev := g.Events()
	if len(ev) != 1 || !ev[0].Shrunk || ev[0].Reason != ReasonReserve || ev[0].KRW != 150_000 {
		t.Fatalf("events %+v", ev)
	}
}

// 최근 weight 를 알면 예비분 건당 추정이 그걸 쓴다.
func TestGateReserveUsesSeenWeight(t *testing.T) {
	p := gatePlan(t)
	g := NewGate(p, kst("10:00:00"), nil, func(b string) float64 {
		if b == "swing" {
			return 0.02
		}
		return 0
	})
	if !near(g.Reserve(), 100_000) { // 50만 × 0.02 × 10
		t.Fatalf("예비분 %v", g.Reserve())
	}
}

// ④ 맡긴 돈이 모자라면 줄여 사고, 0 이면 거절.
func TestGateCapitalShrinkThenReject(t *testing.T) {
	p := gatePlan(t)
	now := kst("10:00:00")
	today := kst("15:15:00")
	g := NewGate(p, now, []Held{{Book: "day", Value: 990_000}}, nil)
	if q, why := g.Admit("swing", target("n1", nil), 25, 1000, sizing.StockMarket()); q != 10 || why != ReasonCapital {
		t.Fatalf("q=%v why=%s", q, why)
	}
	if q, why := g.Admit("day", target("d2", &today), 1, 1000, sizing.StockMarket()); q != 0 || why != ReasonCapital {
		t.Fatalf("다 썼으면 거절: q=%v why=%s", q, why)
	}
}

func TestGateNilPlanPasses(t *testing.T) {
	g := NewGate(nil, kst("10:00:00"), nil, nil)
	if q, why := g.Admit("x", target("a", nil), 7, 1000, sizing.StockMarket()); q != 7 || why != "" {
		t.Fatal("자동 배분이 아니면 그대로")
	}
}

// 가동률 — 장중 시간가중 평균·최대, 15:30 뒤 첫 보유 = 밤. 같은 목표는 사유마다 한 번만 센다.
func TestMeter(t *testing.T) {
	m := NewMeter()
	c := 1_000_000.0
	m.Sample(kst("09:00:00"), c, nil)
	m.Sample(kst("09:01:00"), c, []Held{{Book: "a", Value: 500_000}})
	m.Sample(kst("09:02:00"), c, []Held{{Book: "a", Value: 500_000}})
	m.Sample(kst("09:20:00"), c, []Held{{Book: "a", Value: 500_000}}) // 18분 공백 — 안 센다
	m.Sample(kst("15:31:00"), c, []Held{{Book: "a", Value: 300_000}})
	ev := Event{Book: "a", IntentID: "x", Reason: ReasonBookCap, KRW: 25_000}
	m.Record(kst("10:00:00"), c, []Event{ev, ev})
	m.Record(kst("10:00:01"), c, []Event{ev, {Book: "a", IntentID: "x", Reason: ReasonReserve, KRW: 5_000, Shrunk: true}})

	r, prev := m.Report(kst("15:40:00"), c)
	if prev != nil {
		t.Fatal("어제 것이 없다")
	}
	// 09:00~09:01 은 0, 09:01~09:02 는 50만 → 120초 평균 25만 = 25%.
	if !near(r.DayUtilAvg, 0.25) || !near(r.DayUtilMax, 0.5) || r.MarketSeconds != 120 || !near(r.NightUtil, 0.3) {
		t.Fatalf("report %+v", r)
	}
	if c := r.Missed["a"][ReasonBookCap]; c.N != 1 || c.KRW != 25_000 {
		t.Fatalf("같은 목표를 두 번 셌다: %+v", r.Missed)
	}
	if c := r.Shrunk["a"][ReasonReserve]; c.N != 1 {
		t.Fatalf("shrunk %+v", r.Shrunk)
	}

	// 날이 바뀌면 어제 것이 prev 로 넘어간다.
	_, prev = m.Report(kst("10:00:00").AddDate(0, 0, 1), c)
	if prev == nil || prev.Day != "2026-10-19" {
		t.Fatalf("prev %+v", prev)
	}
}
