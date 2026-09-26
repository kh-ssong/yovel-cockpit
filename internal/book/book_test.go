package book

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSlotsMapToBooks(t *testing.T) {
	s, err := New([]Book{
		{Name: "d205", Seed: 16_000_000, Slots: []string{"d205", "klev"}},
		{Name: "places", Seed: 5_000_000},
	}, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		name   string
		budget float64
	}{
		"klev":    {"d205", 16_000_000}, // 한 전략의 두 슬롯은 한 장부 — 시드가 곱해지지 않는다
		"d205":    {"d205", 16_000_000},
		"places":  {"places", 5_000_000},
		"unknown": {Default, 1_000_000},
	}
	for slot, want := range cases {
		n, b := s.Of(slot)
		if n != want.name || b != want.budget {
			t.Errorf("%s → %s/%v, 기대 %s/%v", slot, n, b, want.name, want.budget)
		}
	}
	if s.Total() != 22_000_000 {
		t.Fatalf("합계 %v", s.Total())
	}
}

// ★ 한 슬롯이 두 장부에 있으면 어느 돈으로 샀는지 모른다 — 기동을 막는다.
func TestSlotInTwoBooksRejected(t *testing.T) {
	_, err := New([]Book{
		{Name: "a", Seed: 1, Slots: []string{"x"}},
		{Name: "b", Seed: 1, Slots: []string{"x"}},
	}, 0)
	if err == nil {
		t.Fatal("중복 슬롯을 받았다")
	}
}

func TestLoadMissingFileIsDefaultOnly(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "none.json"), 7)
	if err != nil {
		t.Fatal(err)
	}
	if n, b := s.Of("any"); n != Default || b != 7 || len(s.Books()) != 0 {
		t.Fatalf("%s %v", n, b)
	}
}

func TestLoadFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "books.json")
	os.WriteFile(p, []byte(`{"books":[{"name":"d205","seed":16000000}]}`), 0o600)
	s, err := Load(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n, b := s.Of("d205"); n != "d205" || b != 16_000_000 {
		t.Fatalf("%s %v", n, b)
	}
	// 기본 예산 0 → 장부 밖 슬롯은 분모 0 = 거절 대상.
	if _, b := s.Of("other"); b != 0 {
		t.Fatalf("장부 밖 슬롯 예산 %v", b)
	}
}
