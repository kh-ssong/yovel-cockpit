// Package session 은 거래소가 **지금 무엇을 받아 주는지** 안다 — 장 시간·동시호가·휴장일·점검.
//
// ★ 왜 필요한가 (reflex 실전 교훈, 2026-10-01 분석):
//   - 동시호가(장전 08:30~09:00 · 장마감 15:20~15:30) 중 시장가는 **단일가 시각에 한꺼번에** 체결된다.
//     체결 대기 시간이 짧으면 콕핏이 자기 주문을 취소하고 다시 내기를 반복하다 장이 끝난다.
//   - 장 밖에서 낸 주문은 전부 거부된다 — 밤새 매 틱 거부만 반복하고 알림이 쌓인다.
//   - 휴장일 파일이 낡으면 그날 모든 주문이 "장 운영시간 아님" 으로 막힌다 (reflex 2026-07-17).
//   - 키움 점검(평일 05:00~06:00)에는 토큰 발급이 엉뚱한 페이지로 넘어간다.
//
// 코인(UPBIT)은 24시간이라 항상 열려 있다.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"
)

var KST = time.FixedZone("KST", 9*3600)

type Phase string

const (
	Closed         Phase = "closed"          // 휴장·주말·장외
	Maintenance    Phase = "maintenance"     // 브로커 점검 — API 호출 자체를 쉰다
	PreOpen        Phase = "preopen_auction" // 08:30~09:00 장전 동시호가 (시장가는 09:00 단일가)
	Continuous     Phase = "continuous"      // 09:00~15:20 접속매매
	ClosingAuction Phase = "closing_auction" // 15:20~15:30 장마감 동시호가 (시장가는 15:30 단일가)
	Open24h        Phase = "open_24h"        // 코인
)

// Auction — 단일가 매매 구간인가. 이 동안 시장가는 바로 체결되지 않는 게 정상이다.
func (p Phase) Auction() bool { return p == PreOpen || p == ClosingAuction }

// Calendar — KRX 휴장일.
type Calendar struct {
	holidays map[string]bool
	last     time.Time // 목록의 마지막 날 — 이 뒤로는 휴장일을 모른다
}

// DefaultKRXHolidays — reflex-agent config/holidays_krx.json (2026) 에서 옮겼다.
// ★ 사람이 만든 목록이라 빠진 날이 있을 수 있다 — {data-dir}/holidays_krx.json 으로 덮어쓸 것.
// 브로커도 휴장일 주문을 거부하므로 이건 1차 방어선일 뿐이다.
var DefaultKRXHolidays = []string{
	"2026-01-01", "2026-02-16", "2026-02-17", "2026-02-18", "2026-03-02", "2026-05-01",
	"2026-05-05", "2026-05-24", "2026-05-25", "2026-06-03", "2026-07-17", "2026-09-24",
	"2026-09-25", "2026-10-09", "2026-12-25", "2026-12-31",
}

func NewCalendar(days []string) (*Calendar, error) {
	c := &Calendar{holidays: map[string]bool{}}
	for _, d := range days {
		t, err := time.ParseInLocation("2006-01-02", d, KST)
		if err != nil {
			return nil, fmt.Errorf("휴장일 %q: %w", d, err)
		}
		c.holidays[d] = true
		if t.After(c.last) {
			c.last = t
		}
	}
	return c, nil
}

// LoadCalendar — 파일(["2026-01-01", ...])이 있으면 그걸, 없으면 기본 목록.
func LoadCalendar(path string) (*Calendar, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewCalendar(DefaultKRXHolidays)
	}
	if err != nil {
		return nil, err
	}
	var days []string
	if err := json.Unmarshal(raw, &days); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return NewCalendar(days)
}

// CoversUntil — 이 날짜까지 휴장일을 아는가. 목록이 끝나 가면 사람이 갱신해야 한다.
func (c *Calendar) CoversUntil(t time.Time) bool { return !t.In(KST).After(c.last.AddDate(0, 0, 1)) }

func (c *Calendar) Last() time.Time { return c.last }

func (c *Calendar) TradingDay(t time.Time) bool {
	k := t.In(KST)
	if k.Weekday() == time.Saturday || k.Weekday() == time.Sunday {
		return false
	}
	return !c.holidays[k.Format("2006-01-02")]
}

// KRX — 지금 KRX 의 단계.
func (c *Calendar) KRX(t time.Time) Phase {
	k := t.In(KST)
	m := k.Hour()*60 + k.Minute()
	if !c.TradingDay(k) {
		return Closed
	}
	switch {
	case m >= 5*60 && m < 6*60:
		return Maintenance
	case m >= 8*60+30 && m < 9*60:
		return PreOpen
	case m >= 9*60 && m < 15*60+20:
		return Continuous
	case m >= 15*60+20 && m < 15*60+30:
		return ClosingAuction
	}
	return Closed
}

// Of — 거래소별 단계. KRX·NXT 는 KRX 달력, UPBIT 은 24시간.
func (c *Calendar) Of(exchange string, t time.Time) Phase {
	if exchange == "UPBIT" {
		return Open24h
	}
	return c.KRX(t)
}

// CanEnter — 새 진입을 낼 수 있는가. 접속매매 중에만 (동시호가 진입은 가격을 모른다).
func (c *Calendar) CanEnter(exchange string, t time.Time) bool {
	p := c.Of(exchange, t)
	return p == Continuous || p == Open24h
}

// CanExit — 청산 주문을 낼 수 있는가. 접속매매 + 장전 동시호가 08:59 부터(→ 09:00 단일가로 체결).
//
// ★ 08:59 인 이유 (reflex 2026-06-17): 08:55 에 내면 체결 대기 시간 안에 09:00 이 안 와서 취소·재주문을
// 반복했다. 장마감 동시호가(15:20~)엔 새 청산을 내지 않는다 — 이미 낸 주문은 기다린다.
func (c *Calendar) CanExit(exchange string, t time.Time) bool {
	p := c.Of(exchange, t)
	if p == PreOpen {
		k := t.In(KST)
		return k.Hour() == 8 && k.Minute() >= 59
	}
	return p == Continuous || p == Open24h
}

// ExitCutoff — KRX 시간청산을 장중에 끝내는 상한 (기본 15:15). 시간청산이 이보다 늦게 잡혀 오면
// 이 시각으로 당긴다 — 15:20 이후는 동시호가라 장중 매도가 안 된다 (user 2026-10-01: D-205 는 15:15 장중 매도).
func ExitCutoffOn(day time.Time, hhmm string) (time.Time, error) {
	t, err := time.ParseInLocation("15:04", hhmm, KST)
	if err != nil {
		return time.Time{}, err
	}
	d := day.In(KST)
	return time.Date(d.Year(), d.Month(), d.Day(), t.Hour(), t.Minute(), 0, 0, KST), nil
}

// Days — 디버그·표시용 (정렬된 휴장일).
func (c *Calendar) Days() []string {
	out := make([]string, 0, len(c.holidays))
	for d := range c.holidays {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
