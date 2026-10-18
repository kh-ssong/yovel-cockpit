package engine

import (
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/alloc"
	"github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// 자동 배분 (docs/allocation.md) — 엔진이 몫으로 사이징하고, 장부 상한·밤 한도·맡긴 돈을 소스를 가로질러 건다.

// atgt — weight 와 청산 기한(오늘이면 낮 진입)을 고른 진입 목표. base = 2026-08-15 09:05 KST.
func atgt(id, code string, w float64, exitToday bool) map[string]any {
	m := tgt(id, code, "open")
	m["weight"] = w
	ex := map[string]any{"stop_price": 900}
	if exitToday {
		ex["time_exit_at"] = base.Add(6 * time.Hour) // 15:05 KST 같은 날
	}
	m["exit"] = ex
	return m
}

func allocEngine(t *testing.T, a book.Alloc, books ...book.Book) (*Engine, signer) {
	t.Helper()
	set, err := book.NewAlloc(books, 0, a)
	if err != nil {
		t.Fatal(err)
	}
	flat6 := mkSigner("flat6-1", 3)
	e := multiEngine(t, set, flat6)
	e.SetAlloc(alloc.Compute(set, base))
	return e, flat6
}

func TestAllocSizingAndCrossSourceLimits(t *testing.T) {
	e, flat6 := allocEngine(t, book.Alloc{Capital: 1_000_000, OvernightPct: 50, BorrowX: 2, ReserveTrades: 0},
		book.Book{Name: "swing_a", Kid: "flat6-1", Scope: "kiwoom/swing_a"},
		book.Book{Name: "swing_b", Kid: "flat6-1", Scope: "kiwoom/swing_b"},
		book.Book{Name: "day", Kid: "flat6-1", Scope: "kiwoom/day", Hold: book.HoldDay},
	)
	// swing_a 가 이미 49만을 밤으로 들고 있다 (몫 25만 × 2 = 상한 50만 · 밤 한도 50만).
	e.SetPositions([]protocol.Position{{IntentID: "01J9Z8QK3M7X2ABCDEFGHJKMA0", Kid: "flat6-1",
		Scope: "kiwoom/swing_a", Symbol: protocol.Symbol{Exchange: "KRX", Code: "000001"}, Qty: 490, AvgEntryPrice: 1000}})

	held := atgt("01J9Z8QK3M7X2ABCDEFGHJKMA0", "000001", 0.1, false)
	applied(t, e, flat6.snap(t, 1, "kiwoom/swing_a", held, atgt("01J9Z8QK3M7X2ABCDEFGHJKMA1", "005930", 0.1, false)))
	applied(t, e, flat6.snap(t, 1, "kiwoom/swing_b", atgt("01J9Z8QK3M7X2ABCDEFGHJKMB1", "000660", 0.1, false)))
	applied(t, e, flat6.snap(t, 1, "kiwoom/day", atgt("01J9Z8QK3M7X2ABCDEFGHJKMC1", "035420", 0.5, true)))

	plan := e.Plan(base.Add(time.Second))
	// day: 몫 100만 × 0.5 = 50만 → 500주. 맡긴 돈 남은 51만 안이라 그대로.
	if len(plan.Enters) != 1 || plan.Enters[0].Scope != "kiwoom/day" || plan.Enters[0].Qty != 500 {
		t.Fatalf("enters %+v", plan.Enters)
	}
	why := map[string]string{}
	for _, ev := range plan.Alloc {
		why[ev.Book] = ev.Reason
	}
	// swing_a: 건당 25만 × 0.1 = 2.5만 > 상한 남은 1만 → 장부 상한. swing_b: 밤 한도 남은 1만 < 2.5만.
	if why["swing_a"] != alloc.ReasonBookCap || why["swing_b"] != alloc.ReasonOvernightCap {
		t.Fatalf("사유 %v (events %+v)", why, plan.Alloc)
	}
	codes := map[string]protocol.RejectCode{}
	for _, a := range plan.Acks {
		if len(a.Codes) > 0 {
			codes[a.IntentID] = a.Codes[0]
		}
	}
	if codes["01J9Z8QK3M7X2ABCDEFGHJKMA1"] != protocol.CodeCapital || codes["01J9Z8QK3M7X2ABCDEFGHJKMB1"] != protocol.CodeCapital {
		t.Fatalf("★ 와이어 코드는 전부 E_CAPITAL 이어야 한다 (flat6 계약 불변): %v", codes)
	}

	// 계측 — 집행 틱이 넘긴 것만 센다. 미리보기 Plan 은 안 센다.
	now := base.Add(2 * time.Second)
	e.RecordAlloc(now, plan.Alloc)
	e.RecordAlloc(now, e.Plan(now).Alloc) // 다음 틱 — 같은 목표는 다시 안 센다
	r := e.AllocReport(now)
	if r == nil || r.Today.Missed["swing_a"][alloc.ReasonBookCap].N != 1 || r.Today.Missed["swing_b"][alloc.ReasonOvernightCap].N != 1 {
		t.Fatalf("report %+v", r)
	}
	if r.Free != 510_000 || r.NightLeft != 10_000 {
		t.Fatalf("free=%v night_left=%v", r.Free, r.NightLeft)
	}
}

// 자동 배분 장부의 성적 분모는 오늘 몫이다 (seed 를 안 적는다).
func TestAllocBookStatsUseBase(t *testing.T) {
	e, _ := allocEngine(t, book.Alloc{Capital: 1_000_000, OvernightPct: 70, BorrowX: 2, ReserveTrades: 10},
		book.Book{Name: "swing", Kid: "flat6-1", Scope: "kiwoom/swing"},
		book.Book{Name: "f60", Kid: "flat6-1", Scope: "kiwoom/f60", Seed: 300_000, MaxQty: 1},
	)
	stats, err := e.BookStats(t.Context(), protocol.ModePaper)
	if err != nil {
		t.Fatal(err)
	}
	seed := map[string]float64{}
	for _, s := range stats {
		seed[s.Name] = s.Seed
	}
	if seed["swing"] != 700_000 || seed["f60"] != 300_000 {
		t.Fatalf("seeds %v", seed)
	}
}

// 고정 장부도 맡긴 돈 안에서 산다 — 남은 돈만큼 줄여 산다 (계좌 층).
func TestAllocFixedBookStillUnderCapital(t *testing.T) {
	e, flat6 := allocEngine(t, book.Alloc{Capital: 100_000, OvernightPct: 70, BorrowX: 2, ReserveTrades: 0},
		book.Book{Name: "f60", Kid: "flat6-1", Scope: "kiwoom/f60", Seed: 300_000, Hold: book.HoldDay},
	)
	applied(t, e, flat6.snap(t, 1, "kiwoom/f60", atgt("01J9Z8QK3M7X2ABCDEFGHJKMD1", "122630", 1.0, true)))
	plan := e.Plan(base.Add(time.Second))
	// seed 30만 × 1.0 = 300주인데 맡긴 돈 10만 → 100주.
	if len(plan.Enters) != 1 || plan.Enters[0].Qty != 100 {
		t.Fatalf("enters %+v", plan.Enters)
	}
	if len(plan.Alloc) != 1 || !plan.Alloc[0].Shrunk || plan.Alloc[0].Reason != alloc.ReasonCapital {
		t.Fatalf("events %+v", plan.Alloc)
	}
}

// 장부 hold 와 목표가 어긋나면 하루 한 번 알린다.
func TestAllocHoldMismatchWarnsOnce(t *testing.T) {
	set, _ := book.NewAlloc([]book.Book{{Name: "intraday", Kid: "flat6-1", Scope: "kiwoom/intraday"}}, 0,
		book.Alloc{Capital: 1_000_000, OvernightPct: 70, BorrowX: 2, ReserveTrades: 10})
	flat6 := mkSigner("flat6-1", 3)
	e := multiEngine(t, set, flat6)
	warned := make(chan string, 4)
	e.cfg.OnAllocWarn = func(m string) { warned <- m }
	e.SetAlloc(alloc.Compute(set, base))

	applied(t, e, flat6.snap(t, 1, "kiwoom/intraday", atgt("01J9Z8QK3M7X2ABCDEFGHJKME1", "005930", 0.1, true)))
	e.Plan(base.Add(time.Second))
	e.Plan(base.Add(2 * time.Second))
	select {
	case <-warned:
	case <-time.After(2 * time.Second):
		t.Fatal("hold=carry(생략) 장부에 당일 청산 목표 — 알려야 한다")
	}
	select {
	case m := <-warned:
		t.Fatalf("두 번 알렸다: %s", m)
	case <-time.After(100 * time.Millisecond):
	}
	if r := e.AllocReport(base.Add(3 * time.Second)); r.Today.HoldMismatch["intraday"] != "day" {
		t.Fatalf("%+v", r.Today.HoldMismatch)
	}
}

// 자동 배분이 꺼져 있으면 아무것도 안 바뀐다.
func TestAllocOffIsLegacy(t *testing.T) {
	books, _ := book.New([]book.Book{{Name: "d205", Kid: "flat6-1", Scope: "intraday/d205", Seed: 1_000_000}}, 0)
	flat6 := mkSigner("flat6-1", 3)
	e := multiEngine(t, books, flat6)
	applied(t, e, flat6.snap(t, 1, "intraday/d205", tgt("01J9Z8QK3M7X2ABCDEFGHJKMF1", "005930", "open")))
	plan := e.Plan(base.Add(time.Second))
	if len(plan.Enters) != 1 || plan.Enters[0].Qty != 500 || len(plan.Alloc) != 0 || e.AllocReport(base) != nil {
		t.Fatalf("plan %+v", plan)
	}
}
