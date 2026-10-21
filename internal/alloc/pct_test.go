package alloc

import (
	"testing"

	"github.com/kh-ssong/yovel-cockpit/internal/book"
)

// pct 장부 — 몫 = capital × pct% (밤 장부도 capital 기준). 계좌에서 읽은 capital 을 그대로 받는다 (user 2026-10-21).
func TestComputePct(t *testing.T) {
	off := false
	s := set(t, book.Alloc{FromAccount: true, CapitalPct: 100, OvernightPct: 70, BorrowX: 2, ReserveTrades: 10},
		book.Book{Name: "s60", Scope: "kiwoom/swing_60m", Pct: f(23)},
		book.Book{Name: "gap", Scope: "kiwoom/gapdown_main", Hold: book.HoldDay, Pct: f(25)},
		book.Book{Name: "x60", Scope: "kiwoom/klev_push10x_f60", Hold: book.HoldDay, Pct: f(16)},
		book.Book{Name: "off", Scope: "kiwoom/off", Hold: book.HoldDay, Pct: f(30), Enabled: &off},
		book.Book{Name: "fix", Scope: "kiwoom/fixed", Hold: book.HoldDay, Seed: 100_000},
	)
	p := ComputeWith(s, 2_000_000, FromAccount, kst("08:50:00"))
	if p.Capital != 2_000_000 || p.CapitalFrom != FromAccount || !near(p.NightCap, 1_400_000) {
		t.Fatalf("plan %+v", p)
	}
	b := func(n string) Book { x, _ := p.Book(n); return x }
	if s60 := b("s60"); !near(s60.Base, 460_000) || !near(s60.Cap, 920_000) || s60.Pct != 23 || s60.Share != 0 {
		t.Fatalf("밤 pct 장부 %+v", s60)
	}
	if gap := b("gap"); !near(gap.Base, 500_000) || !near(gap.Cap, 1_000_000) {
		t.Fatalf("낮 pct 장부 %+v", gap)
	}
	if off := b("off"); off.Base != 0 || off.Cap != 0 || off.Enabled {
		t.Fatalf("꺼진 장부는 몫이 없다 %+v", off)
	}
	// 낮 합 = (50만 + 32만 + 고정 10만) ÷ 200만 = 46% · 밤 합 = 23%.
	if !near(p.DayPct, 46) || !near(p.NightPct, 23) {
		t.Fatalf("합 day %v night %v", p.DayPct, p.NightPct)
	}
}

// 맡길 돈이 바뀌면 pct 장부 몫이 그만큼 같이 바뀐다 — 입금하면 다음 날 몫이 커진다.
func TestComputePctScalesWithCapital(t *testing.T) {
	s := set(t, book.Alloc{FromAccount: true, CapitalPct: 100, OvernightPct: 70, BorrowX: 2},
		book.Book{Name: "gap", Scope: "kiwoom/gapdown_main", Hold: book.HoldDay, Pct: f(25)})
	a := ComputeWith(s, 1_460_000, FromAccount, kst("08:50:00"))
	b := ComputeWith(s, 1_860_000, FromAccount, kst("08:50:00"))
	x, _ := a.Book("gap")
	y, _ := b.Book("gap")
	if !near(x.Base, 365_000) || !near(y.Base, 465_000) {
		t.Fatalf("%v → %v", x.Base, y.Base)
	}
}

// share 방식은 그대로 — Compute 는 books.json 숫자를 쓴다.
func TestComputeSharesStillUsesBooksCapital(t *testing.T) {
	s := set(t, book.Alloc{Capital: 1_000_000, OvernightPct: 50, BorrowX: 2},
		book.Book{Name: "d", Scope: "a", Hold: book.HoldDay},
		book.Book{Name: "n", Scope: "b"})
	p := Compute(s, kst("08:50:00"))
	if p.CapitalFrom != FromBooks || !near(p.DayPct, 100) || !near(p.NightPct, 50) {
		t.Fatalf("%+v", p)
	}
}
