package book

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// "capital": "account" — 맡길 돈을 계좌에서 읽고, 장부마다 계좌의 몇 %인지 적는다 (user 2026-10-21).
func TestLoadAccountPct(t *testing.T) {
	p := filepath.Join(t.TempDir(), "books.json")
	os.WriteFile(p, []byte(`{"capital": "account", "capital_pct": 90, "capital_max": 5000000,
	 "books":[
	  {"name":"s60","kid":"flat6-1","scope":"kiwoom/swing_60m","hold":"carry","pct":23},
	  {"name":"gap","kid":"flat6-1","scope":"kiwoom/gapdown_main","hold":"day","pct":25},
	  {"name":"old","kid":"flat6-1","scope":"kiwoom/klev_push10_f60","seed":300000,"enabled":false}
	 ]}`), 0o600)
	s, err := Load(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Alloc()
	if !a.On() || !a.FromAccount || a.Capital != 0 || a.CapitalPct != 90 || a.CapitalMax != 5_000_000 ||
		a.OvernightPct != DefaultOvernightPct {
		t.Fatalf("%+v", a)
	}
	if s.Total() != 0 {
		t.Fatalf("account 는 정해 둔 숫자가 없다 — Total %v", s.Total())
	}
	got := map[string]Book{}
	for _, b := range s.Books() {
		got[b.Name] = b
	}
	if got["s60"].Pct == nil || *got["s60"].Pct != 23 || got["s60"].Fixed() {
		t.Fatalf("s60 %+v", got["s60"])
	}
}

// capital_pct 를 생략하면 100 — 계좌 전부.
func TestLoadAccountDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "books.json")
	os.WriteFile(p, []byte(`{"capital": "account", "books":[{"name":"s","scope":"kiwoom/swing_60m"}]}`), 0o600)
	s, err := Load(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a := s.Alloc(); !a.FromAccount || a.CapitalPct != 100 || a.CapitalMax != 0 {
		t.Fatalf("%+v", a)
	}
}

// 숫자 capital 에도 pct 를 쓸 수 있다 (몫 = 적은 숫자 × pct%).
func TestLoadFixedCapitalPct(t *testing.T) {
	p := filepath.Join(t.TempDir(), "books.json")
	os.WriteFile(p, []byte(`{"capital": 1000000, "books":[{"name":"s","scope":"a","hold":"day","pct":40}]}`), 0o600)
	s, err := Load(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a := s.Alloc(); a.FromAccount || a.Capital != 1_000_000 || s.Total() != 1_000_000 {
		t.Fatalf("%+v total %v", a, s.Total())
	}
}

func TestLoadAccountPctRejects(t *testing.T) {
	cases := map[string]string{
		"capital 오타":               `{"capital": "acount", "books":[{"name":"s","scope":"a"}]}`,
		"capital 이상한 값":            `{"capital": true, "books":[{"name":"s","scope":"a"}]}`,
		"숫자 capital 에 capital_pct": `{"capital": 1000, "capital_pct": 50, "books":[{"name":"s","scope":"a"}]}`,
		"capital 없이 capital_max":   `{"capital_max": 1000, "books":[{"name":"s","scope":"a","seed":1}]}`,
		"capital_pct 0":            `{"capital": "account", "capital_pct": 0, "books":[{"name":"s","scope":"a"}]}`,
		"capital_pct 120":          `{"capital": "account", "capital_pct": 120, "books":[{"name":"s","scope":"a"}]}`,
		"capital_max 0":            `{"capital": "account", "capital_max": 0, "books":[{"name":"s","scope":"a"}]}`,
		"capital 없이 pct":           `{"books":[{"name":"s","scope":"a","seed":1,"pct":10}]}`,
		"pct 0":                    `{"capital": "account", "books":[{"name":"s","scope":"a","pct":0}]}`,
		"pct 101":                  `{"capital": "account", "books":[{"name":"s","scope":"a","hold":"day","pct":101}]}`,
		"pct + seed":               `{"capital": "account", "books":[{"name":"s","scope":"a","seed":1,"pct":10}]}`,
		"pct + share":              `{"capital": "account", "books":[{"name":"s","scope":"a","share":1,"pct":10}]}`,
		"pct 와 share 섞기": `{"capital": "account", "books":[{"name":"s","scope":"a","pct":10},
			{"name":"t","scope":"b","share":1}]}`,
		"pct 와 생략 섞기": `{"capital": "account", "books":[{"name":"s","scope":"a","pct":10},
			{"name":"t","scope":"b"}]}`,
		"밤 pct 합 > overnight_pct": `{"capital": "account", "overnight_pct": 50, "books":[
			{"name":"s","scope":"a","pct":30}, {"name":"t","scope":"b","hold":"carry","pct":30}]}`,
	}
	for name, raw := range cases {
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

// 낮 장부 pct 합은 100 을 넘어도 된다 (낮엔 장부끼리 계좌를 선착순으로 나눠 쓴다). 꺼진 밤 장부는 밤 합에 안 센다.
func TestLoadPctDayOverAndDisabledNight(t *testing.T) {
	p := filepath.Join(t.TempDir(), "books.json")
	os.WriteFile(p, []byte(`{"capital": "account", "overnight_pct": 50, "books":[
	  {"name":"d1","scope":"a","hold":"day","pct":80},
	  {"name":"d2","scope":"b","hold":"day","pct":70},
	  {"name":"n1","scope":"c","pct":50},
	  {"name":"n2","scope":"d","pct":30,"enabled":false}]}`), 0o600)
	if _, err := Load(p, 0); err != nil {
		t.Fatal(err)
	}
}
