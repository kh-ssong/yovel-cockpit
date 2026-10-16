package book

import (
	"os"
	"path/filepath"
	"testing"
)

func off() *bool { f := false; return &f }

func TestSourcesMapToBooks(t *testing.T) {
	s, err := New([]Book{
		{Name: "d205", Kid: "flat6-1", Scope: "intraday/d205", Seed: 16_000_000},
		{Name: "klev", Kid: "flat6-1", Scope: "intraday/klev", Seed: 3_000_000},
		{Name: "coin", Kid: "flat6-1", Scope: "scalp/coin", Seed: 2_000_000, Enabled: off()},
		{Name: "dev", Scope: "dev/any", Seed: 1_000_000}, // kid 생략 = 아무 kid
	}, 99)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		kid, scope, name string
		budget           float64
		active           bool
	}{
		{"flat6-1", "intraday/d205", "d205", 16_000_000, true},
		{"flat6-1", "scalp/coin", "coin", 2_000_000, false}, // 꺼진 전략
		{"otto-1", "intraday/d205", "", 0, false},           // ★ 다른 발행자는 남의 장부로 못 산다
		{"flat6-1", "swing/unknown", "", 0, false},          // 장부에 없는 전략
		{"dev-1", "dev/any", "dev", 1_000_000, true},
	}
	for _, c := range cases {
		n, b, a := s.Of(c.kid, c.scope)
		if n != c.name || b != c.budget || a != c.active {
			t.Errorf("%s/%s → %s/%v/%v, 기대 %s/%v/%v", c.kid, c.scope, n, b, a, c.name, c.budget, c.active)
		}
	}
	if s.Total() != 20_000_000 { // 꺼진 coin 제외
		t.Fatalf("합계 %v", s.Total())
	}
}

// ★ 한 소스가 두 장부에 걸리면 어느 돈으로 샀는지 모른다 — 기동을 막는다.
func TestSameSourceInTwoBooksRejected(t *testing.T) {
	if _, err := New([]Book{
		{Name: "a", Kid: "k", Scope: "x", Seed: 1},
		{Name: "b", Scope: "x", Seed: 1}, // kid 생략이 k 를 덮는다
	}, 0); err == nil {
		t.Fatal("겹치는 소스를 받았다")
	}
}

// 장부 파일이 없으면 옛 방식 — 모든 소스가 엔진 예산 하나로 돈다.
func TestNoBooksIsLegacy(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "none.json"), 7)
	if err != nil {
		t.Fatal(err)
	}
	if n, b, a := s.Of("any", "whatever"); n != Default || b != 7 || !a {
		t.Fatalf("%s %v %v", n, b, a)
	}
}

func TestLoadFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "books.json")
	os.WriteFile(p, []byte(`{"books":[{"name":"d205","kid":"flat6-1","scope":"intraday/d205","seed":16000000}]}`), 0o600)
	s, err := Load(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n, b, a := s.Of("flat6-1", "intraday/d205"); n != "d205" || b != 16_000_000 || !a {
		t.Fatalf("%s %v %v", n, b, a)
	}
}

// 오버부킹 표시 — 없으면 꺼짐(예산 점검이 경고한다), 있으면 켜짐 (user 2026-10-15).
func TestLoadOverbook(t *testing.T) {
	p := filepath.Join(t.TempDir(), "books.json")
	os.WriteFile(p, []byte(`{"books":[{"name":"m","scope":"kiwoom/gapdown_main","seed":20000000}]}`), 0o600)
	if s, err := Load(p, 0); err != nil || s.Overbook() {
		t.Fatalf("표시가 없는데 켜졌다: %v", err)
	}
	os.WriteFile(p, []byte(`{"books":[{"name":"m","scope":"kiwoom/gapdown_main","seed":20000000}],"overbook":true}`), 0o600)
	if s, err := Load(p, 0); err != nil || !s.Overbook() {
		t.Fatalf("표시를 못 읽었다: %v", err)
	}
}

// max_qty — 진입 한 건 수량 상한 (user 2026-10-16 «1주씩»). 없으면 0, 음수는 기동 거부.
func TestLoadMaxQty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "books.json")
	os.WriteFile(p, []byte(`{"books":[
	  {"name":"f60","kid":"flat6-1","scope":"kiwoom/klev_push10_f60","seed":1000000,"max_qty":1},
	  {"name":"d205","kid":"flat6-1","scope":"intraday/d205","seed":16000000}]}`), 0o600)
	s, err := Load(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if q := s.MaxQty("flat6-1", "kiwoom/klev_push10_f60"); q != 1 {
		t.Fatalf("max_qty=%v", q)
	}
	if q := s.MaxQty("flat6-1", "intraday/d205"); q != 0 {
		t.Fatalf("없는데 %v", q)
	}
	if q := s.MaxQty("otto-1", "kiwoom/klev_push10_f60"); q != 0 {
		t.Fatalf("다른 kid 인데 %v", q)
	}
	if q := (*Set)(nil).MaxQty("flat6-1", "x"); q != 0 {
		t.Fatalf("nil 장부 %v", q)
	}
	if _, err := New([]Book{{Name: "x", Scope: "a", Seed: 1, MaxQty: -1}}, 0); err == nil {
		t.Fatal("음수 max_qty 를 받았다")
	}
}

// paper_fee_bp — 장부별 paper 비용. 생략 = ok false (거래소·기본 요율), 음수는 기동 거부.
func TestLoadPaperFee(t *testing.T) {
	p := filepath.Join(t.TempDir(), "books.json")
	os.WriteFile(p, []byte(`{"books":[
	  {"name":"f60","kid":"flat6-1","scope":"kiwoom/klev_push10_f60","seed":10000000,"paper_fee_bp":{"buy":0,"sell":0}},
	  {"name":"sc","kid":"flat6-1","scope":"kiwoom/surge_chase","seed":20000000,"paper_fee_bp":{"buy":0,"sell":20}},
	  {"name":"gd","kid":"flat6-1","scope":"kiwoom/gapdown_main","seed":20000000}]}`), 0o600)
	s, err := Load(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b, sl, ok := s.PaperFee("flat6-1", "kiwoom/klev_push10_f60"); !ok || b != 0 || sl != 0 {
		t.Fatalf("ETF 장부: %v %v %v", b, sl, ok)
	}
	if b, sl, ok := s.PaperFee("flat6-1", "kiwoom/surge_chase"); !ok || b != 0 || sl != 20 {
		t.Fatalf("주식 장부: %v %v %v", b, sl, ok)
	}
	if _, _, ok := s.PaperFee("flat6-1", "kiwoom/gapdown_main"); ok {
		t.Fatal("정하지 않은 장부가 ok")
	}
	if _, _, ok := (*Set)(nil).PaperFee("flat6-1", "x"); ok {
		t.Fatal("nil 장부가 ok")
	}
	if _, err := New([]Book{{Name: "x", Scope: "a", Seed: 1, PaperFeeBp: &Fee{Buy: 0, Sell: -1}}}, 0); err == nil {
		t.Fatal("음수 비용을 받았다")
	}
}
