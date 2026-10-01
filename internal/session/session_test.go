package session

import (
	"testing"
	"time"
)

func at(date, hm string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04", date+" "+hm, KST)
	return t
}

func TestKRXPhases(t *testing.T) {
	c, _ := NewCalendar(DefaultKRXHolidays)
	cases := []struct {
		date, hm string
		want     Phase
	}{
		{"2026-10-01", "05:30", Maintenance},
		{"2026-10-01", "08:30", PreOpen},
		{"2026-10-01", "09:00", Continuous},
		{"2026-10-01", "15:19", Continuous},
		{"2026-10-01", "15:20", ClosingAuction},
		{"2026-10-01", "15:30", Closed},
		{"2026-10-03", "10:00", Closed}, // 토요일
		{"2026-10-09", "10:00", Closed}, // 한글날
	}
	for _, x := range cases {
		if got := c.KRX(at(x.date, x.hm)); got != x.want {
			t.Errorf("%s %s → %s, 기대 %s", x.date, x.hm, got, x.want)
		}
	}
	if c.Of("UPBIT", at("2026-10-03", "03:00")) != Open24h {
		t.Fatal("코인은 24시간")
	}
}

// ★ 08:59 부터 청산 가능(→ 09:00 단일가), 장마감 동시호가엔 새 청산 금지, 진입은 접속매매만.
func TestCanEnterExit(t *testing.T) {
	c, _ := NewCalendar(DefaultKRXHolidays)
	if c.CanExit("KRX", at("2026-10-01", "08:55")) || !c.CanExit("KRX", at("2026-10-01", "08:59")) {
		t.Fatal("장전 청산은 08:59 부터")
	}
	if c.CanEnter("KRX", at("2026-10-01", "08:59")) {
		t.Fatal("동시호가 진입")
	}
	if c.CanExit("KRX", at("2026-10-01", "15:22")) || c.CanEnter("KRX", at("2026-10-01", "15:22")) {
		t.Fatal("장마감 동시호가에 새 주문")
	}
	if !c.CanExit("KRX", at("2026-10-01", "15:15")) {
		t.Fatal("15:15 장중 청산이 막혔다")
	}
}

func TestCalendarCoverage(t *testing.T) {
	c, _ := NewCalendar(DefaultKRXHolidays)
	if !c.CoversUntil(at("2026-12-31", "10:00")) || c.CoversUntil(at("2027-01-05", "10:00")) {
		t.Fatalf("목록 끝 %v", c.Last())
	}
}

func TestExitCutoff(t *testing.T) {
	cut, err := ExitCutoffOn(at("2026-10-01", "10:00"), "15:15")
	if err != nil || !cut.Equal(at("2026-10-01", "15:15")) {
		t.Fatalf("%v %v", cut, err)
	}
}
