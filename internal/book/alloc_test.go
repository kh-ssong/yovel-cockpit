package book

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 자동 배분 (docs/allocation.md) — capital 이 있으면 seed 를 생략한 장부가 몫을 받는다.
func TestLoadAlloc(t *testing.T) {
	p := filepath.Join(t.TempDir(), "books.json")
	os.WriteFile(p, []byte(`{"capital": 1460000,
	 "books":[
	  {"name":"s60","kid":"flat6-1","scope":"kiwoom/swing_60m","hold":"carry"},
	  {"name":"etf","kid":"flat6-1","scope":"kiwoom/swing_etf_60m","share":0.5},
	  {"name":"f60","kid":"flat6-1","scope":"kiwoom/klev_push10_f60","seed":300000,"max_qty":1,"hold":"day"}
	 ]}`), 0o600)
	s, err := Load(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Alloc()
	if !a.On() || a.Capital != 1460000 || a.OvernightPct != DefaultOvernightPct || a.BorrowX != DefaultBorrowX ||
		a.ReserveTrades != DefaultReserveTrades {
		t.Fatalf("기본값 %+v", a)
	}
	if s.Total() != 1460000 {
		t.Fatalf("자동 배분의 Total 은 capital — %v", s.Total())
	}
	got := map[string]Book{}
	for _, b := range s.Books() {
		got[b.Name] = b
	}
	if got["s60"].Fixed() || got["s60"].ShareOr1() != 1 || got["s60"].HoldOrCarry() != HoldCarry {
		t.Fatalf("s60 %+v", got["s60"])
	}
	if got["etf"].ShareOr1() != 0.5 || got["etf"].HoldOrCarry() != HoldCarry {
		t.Fatalf("hold 생략 = carry, share 0.5: %+v", got["etf"])
	}
	if !got["f60"].Fixed() || got["f60"].HoldOrCarry() != HoldDay {
		t.Fatalf("seed 를 적은 장부는 고정 장부: %+v", got["f60"])
	}
}

func TestLoadAllocOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "books.json")
	os.WriteFile(p, []byte(`{"capital": 1000000, "overnight_pct": 50, "borrow_x": 1, "reserve_trades": 0,
	 "books":[{"name":"s","scope":"kiwoom/swing_60m"}]}`), 0o600)
	s, err := Load(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a := s.Alloc(); a.OvernightPct != 50 || a.BorrowX != 1 || a.ReserveTrades != 0 {
		t.Fatalf("%+v", a)
	}
}

// 잘못 적은 자동 배분은 기동 거부 — 조용히 옛 방식으로 돌면 사용자는 켠 줄 안다.
func TestLoadAllocRejects(t *testing.T) {
	cases := map[string]string{
		"capital 없이 배분 설정":    `{"overnight_pct": 50, "books":[{"name":"s","scope":"a","seed":1}]}`,
		"capital 없이 seed 생략":  `{"books":[{"name":"s","scope":"a"}]}`,
		"capital 없이 share":    `{"books":[{"name":"s","scope":"a","seed":1,"share":2}]}`,
		"share 0":             `{"capital": 1, "books":[{"name":"s","scope":"a","share":0}]}`,
		"고정 장부에 share":        `{"capital": 1, "books":[{"name":"s","scope":"a","seed":1,"share":2}]}`,
		"hold 오타":             `{"capital": 1, "books":[{"name":"s","scope":"a","hold":"swing"}]}`,
		"borrow_x < 1":        `{"capital": 1, "borrow_x": 0.5, "books":[{"name":"s","scope":"a"}]}`,
		"overnight_pct 0":     `{"capital": 1, "overnight_pct": 0, "books":[{"name":"s","scope":"a"}]}`,
		"overnight_pct > 100": `{"capital": 1, "overnight_pct": 120, "books":[{"name":"s","scope":"a"}]}`,
		"reserve 음수":          `{"capital": 1, "reserve_trades": -1, "books":[{"name":"s","scope":"a"}]}`,
		"capital 음수":          `{"capital": -1, "books":[{"name":"s","scope":"a","seed":1}]}`,
	}
	for name, raw := range cases {
		// 파일 이름엔 사례 이름을 안 쓴다 — Windows 는 '<' '>' 를 못 써서, 쓰기가 실패하면 읽기 오류로
		// 엉뚱하게 «거부» 가 통과한다.
		p := filepath.Join(t.TempDir(), "books.json")
		if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p, 0); err == nil {
			t.Errorf("%s: 기동 거부해야 한다", name)
		} else if strings.Contains(err.Error(), "cannot find") || strings.Contains(err.Error(), "syntax is incorrect") {
			t.Errorf("%s: 설정이 아니라 파일 오류로 거부됐다: %v", name, err)
		}
	}
}
