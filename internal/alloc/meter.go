package alloc

import (
	"math"
	"sort"
	"sync"
	"time"
)

// maxGap — 샘플 사이가 이보다 벌어지면(데몬이 멈췄다 등) 그 구간은 세지 않는다. 멈춘 동안의 보유를 마지막 값으로
// 채우면 가동률이 지어낸 숫자가 된다.
const maxGap = 5 * time.Minute

// Count — 건수와 금액.
type Count struct {
	N   int     `json:"n"`
	KRW float64 `json:"krw"`
}

// Report — 하루의 자본 효율 (docs/allocation.md §4). 가동률은 capital 대비 비율(0~1).
type Report struct {
	Day     string  `json:"day"`
	Capital float64 `json:"capital"`
	// DayUtilAvg · DayUtilMax — 장중(09:00~15:30 KST) 시간가중 평균·최대 보유 ÷ capital.
	DayUtilAvg float64 `json:"day_util_avg"`
	DayUtilMax float64 `json:"day_util_max"`
	// MarketSeconds — 가동률을 잰 장중 시간 (데몬이 멈춘 구간은 빠진다).
	MarketSeconds float64 `json:"market_seconds"`
	// NightUtil — 15:30 뒤 첫 보유 ÷ capital (그날 밤 들고 가는 몫). 아직 안 쟀으면 -1.
	NightUtil float64 `json:"night_util"`
	// UsageKRWDays — 장부별 «보유 원가 × 시간» (원·일). 수익률을 «돈 × 시간» 당으로 볼 수 있다.
	UsageKRWDays map[string]float64 `json:"usage_krw_days"`
	// Missed · Shrunk — 장부 → 사유 → 건수·금액. 같은 목표는 사유마다 한 번만 센다.
	Missed map[string]map[string]Count `json:"missed"`
	Shrunk map[string]map[string]Count `json:"shrunk"`
	// HoldMismatch — 장부 hold 와 목표의 청산 기한이 어긋난 장부 → 목표로 본 반.
	HoldMismatch map[string]string `json:"hold_mismatch,omitempty"`
}

// Meter — 가동률·놓친 신호를 하루 단위로 잰다. 재시작하면 그날 값은 처음부터다 (원장이 SSOT 이고 이건 계기판).
type Meter struct {
	mu   sync.Mutex
	cur  *dayStat
	prev *Report
}

type dayStat struct {
	day       string
	lastAt    time.Time
	lastHeld  float64
	lastBooks map[string]float64

	mktSec, mktInt, mktMax float64
	night                  float64
	nightSet               bool

	usage  map[string]float64 // 원·초
	missed map[string]map[string]Count
	shrunk map[string]map[string]Count
	seen   map[string]bool
	hold   map[string]string
}

func NewMeter() *Meter { return &Meter{} }

func newDay(day string) *dayStat {
	return &dayStat{day: day, lastBooks: map[string]float64{}, usage: map[string]float64{},
		missed: map[string]map[string]Count{}, shrunk: map[string]map[string]Count{},
		seen: map[string]bool{}, hold: map[string]string{}}
}

// rollLocked — 날이 바뀌었으면 어제 것을 prev 로 넘기고 새로 연다.
func (m *Meter) rollLocked(now time.Time, capital float64) *dayStat {
	d := Day(now)
	if m.cur == nil {
		m.cur = newDay(d)
	} else if m.cur.day != d {
		r := m.cur.report(capital)
		m.prev = &r
		m.cur = newDay(d)
	}
	return m.cur
}

// Sample — 지금 보유를 적는다 (집행 틱마다). held 는 Gate 와 같은 원가 기준.
func (m *Meter) Sample(now time.Time, capital float64, held []Held) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.rollLocked(now, capital)
	var total float64
	books := map[string]float64{}
	for _, h := range held {
		total += h.Value
		books[h.Book] += h.Value
	}
	if !s.lastAt.IsZero() {
		if dt := now.Sub(s.lastAt); dt > 0 && dt <= maxGap {
			sec := dt.Seconds()
			if inMarket(s.lastAt) {
				s.mktSec += sec
				s.mktInt += s.lastHeld * sec
			}
			for b, v := range s.lastBooks {
				s.usage[b] += v * sec
			}
		}
	}
	if inMarket(now) {
		s.mktMax = math.Max(s.mktMax, total)
	}
	if !s.nightSet && afterClose(now) {
		s.night, s.nightSet = total, true
	}
	s.lastAt, s.lastHeld, s.lastBooks = now, total, books
}

// Record — 한도에 걸린 진입들. 같은 목표·같은 사유는 그날 한 번만 센다 (계획은 틱마다 다시 돈다).
func (m *Meter) Record(now time.Time, capital float64, evs []Event) {
	if len(evs) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.rollLocked(now, capital)
	for _, e := range evs {
		key := e.IntentID + "|" + e.Reason
		if e.Shrunk {
			key += "|shrunk"
		}
		if s.seen[key] {
			continue
		}
		s.seen[key] = true
		into := s.missed
		if e.Shrunk {
			into = s.shrunk
		}
		if into[e.Book] == nil {
			into[e.Book] = map[string]Count{}
		}
		c := into[e.Book][e.Reason]
		c.N++
		c.KRW += e.KRW
		into[e.Book][e.Reason] = c
	}
}

// NoteHold — 장부 hold 와 목표가 어긋났다. 그날 처음이면 true (알릴 것).
func (m *Meter) NoteHold(now time.Time, capital float64, bookName, observed string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.rollLocked(now, capital)
	if _, ok := s.hold[bookName]; ok {
		return false
	}
	s.hold[bookName] = observed
	return true
}

// Report — 오늘 것과 (있으면) 어제 것.
func (m *Meter) Report(now time.Time, capital float64) (Report, *Report) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.rollLocked(now, capital)
	return s.report(capital), m.prev
}

func (s *dayStat) report(capital float64) Report {
	r := Report{Day: s.day, Capital: capital, MarketSeconds: s.mktSec, NightUtil: -1,
		UsageKRWDays: map[string]float64{}, Missed: copyCounts(s.missed), Shrunk: copyCounts(s.shrunk)}
	if capital > 0 {
		if s.mktSec > 0 {
			r.DayUtilAvg = s.mktInt / s.mktSec / capital
		}
		r.DayUtilMax = s.mktMax / capital
		if s.nightSet {
			r.NightUtil = s.night / capital
		}
	}
	for b, v := range s.usage {
		r.UsageKRWDays[b] = v / 86400
	}
	if len(s.hold) > 0 {
		r.HoldMismatch = map[string]string{}
		for b, h := range s.hold {
			r.HoldMismatch[b] = h
		}
	}
	return r
}

func copyCounts(in map[string]map[string]Count) map[string]map[string]Count {
	out := map[string]map[string]Count{}
	for b, rs := range in {
		out[b] = map[string]Count{}
		for r, c := range rs {
			out[b][r] = c
		}
	}
	return out
}

// Totals — 사유별 합 (요약 로그용).
func Totals(in map[string]map[string]Count) map[string]Count {
	out := map[string]Count{}
	for _, rs := range in {
		for r, c := range rs {
			t := out[r]
			t.N += c.N
			t.KRW += c.KRW
			out[r] = t
		}
	}
	return out
}

// SortedBooks — 맵 키를 이름순으로 (로그를 매번 같은 순서로).
func SortedBooks[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
