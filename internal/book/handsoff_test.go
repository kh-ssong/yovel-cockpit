package book

import (
	"os"
	"path/filepath"
	"testing"
)

// exits "manual" — 꺼진 장부의 남은 로트를 사용자가 맡아 간다 (user 2026-11-05). 켜진 장부엔 못 건다.
func TestLoadExitsManual(t *testing.T) {
	p := filepath.Join(t.TempDir(), "books.json")
	os.WriteFile(p, []byte(`{"capital":"account","books":[
	  {"name":"swing_etf_60m","kid":"flat6-1","scope":"kiwoom/swing_etf_60m","hold":"carry","pct":23,"enabled":false,"exits":"manual"},
	  {"name":"swing_60m","kid":"flat6-1","scope":"kiwoom/swing_60m","hold":"carry","pct":34.5}]}`), 0o600)
	s, err := Load(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !s.HandsOff("flat6-1", "kiwoom/swing_etf_60m") {
		t.Fatal("exits manual 장부를 못 읽었다")
	}
	if s.HandsOff("flat6-1", "kiwoom/swing_60m") || s.HandsOff("otto-1", "kiwoom/swing_etf_60m") ||
		s.HandsOff("flat6-1", "kiwoom/unknown") || (*Set)(nil).HandsOff("flat6-1", "x") {
		t.Fatal("손 떼지 않은 소스가 HandsOff")
	}
	if !s.AnyHandsOff() || (*Set)(nil).AnyHandsOff() {
		t.Fatal("AnyHandsOff")
	}
	if _, _, active := s.Of("flat6-1", "kiwoom/swing_etf_60m"); active {
		t.Fatal("꺼진 장부가 진입을 받는다")
	}

	cases := []struct {
		name string
		b    Book
	}{
		{"켜진 장부에 manual", Book{Name: "x", Scope: "a", Seed: 1, Exits: ExitsManual}},
		{"모르는 값", Book{Name: "x", Scope: "a", Seed: 1, Enabled: off(), Exits: "never"}},
	}
	for _, c := range cases {
		if _, err := New([]Book{c.b}, 0); err == nil {
			t.Errorf("%s: 기동을 막지 않았다", c.name)
		}
	}
}
