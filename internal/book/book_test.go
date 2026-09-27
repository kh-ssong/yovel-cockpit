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
