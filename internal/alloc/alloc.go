// Package alloc 는 맡긴 돈 하나를 장부들에 나눈다 — 노는 돈을 줄이는 설계 (docs/allocation.md).
//
// 사용자는 맡길 돈(capital) · 밤 한도(overnight_pct) · 장부 비중(share)만 정한다. 나머지는 여기서 계산한다:
//
//   - 밤 장부(carry — 날을 넘겨 든다)는 capital × overnight_pct 를 비중대로 나눠 갖는다.
//   - 낮 장부(day — 15:15 면 현금으로 돌아온다)는 낮에 capital 전체를 비중대로 나눠 갖는다.
//   - 건당 = 몫 × weight (flat6 이 정한 «한 자리 크기 비율»). 장부 상한 = 몫 × borrow_x (남는 돈 빌려 쓰기).
//   - 소스를 가로지르는 한도(밤 한도 · 스윙 예비분 · 맡긴 돈)는 Gate 가 계획마다 건다.
//
// ★ 몫은 하루 동안 고정이다 (08:50 점검·기동 때 계산) — 장중에 건당 크기가 바뀌면 도장 원장과 대조가 안 되고
// «왜 이 크기로 샀는지» 를 원장에서 재현할 수 없다 (protocol.md §7.1.1).
package alloc

import (
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
)

// Book — 장부 하나의 오늘 배분.
type Book struct {
	Name    string  `json:"name"`
	Hold    string  `json:"hold"` // carry | day
	Fixed   bool    `json:"fixed,omitempty"`
	Share   float64 `json:"share,omitempty"`
	Enabled bool    `json:"enabled"`
	// Base — 몫 = 사이징 분모 (건당 = Base × weight). 고정 장부는 seed.
	Base float64 `json:"base"`
	// Cap — 장부 상한 = 몫 × borrow_x. 고정 장부는 seed.
	Cap float64 `json:"cap"`
}

// Plan — 그날의 배분. 계산한 뒤엔 바꾸지 않는다 (엔진이 통째로 갈아 끼운다).
type Plan struct {
	Day           string  `json:"day"` // KST 날짜 — 이 날 동안 고정
	Capital       float64 `json:"capital"`
	OvernightPct  float64 `json:"overnight_pct"`
	NightCap      float64 `json:"night_cap"` // capital × overnight_pct
	BorrowX       float64 `json:"borrow_x"`
	ReserveTrades int     `json:"reserve_trades"`
	Books         []Book  `json:"books"`

	byName map[string]int
}

// Day — KST 날짜 문자열 (몫을 고정하는 단위).
func Day(t time.Time) string { return t.In(session.KST).Format("2006-01-02") }

// Compute — 자동 배분이 꺼져 있으면 nil.
func Compute(set *book.Set, now time.Time) *Plan {
	a := set.Alloc()
	if !a.On() {
		return nil
	}
	p := &Plan{
		Day: Day(now), Capital: a.Capital, OvernightPct: a.OvernightPct,
		NightCap: a.Capital * a.OvernightPct / 100, BorrowX: a.BorrowX, ReserveTrades: a.ReserveTrades,
		byName: map[string]int{},
	}
	books := set.Books()
	var sumCarry, sumDay float64
	for _, b := range books {
		if b.Fixed() || !b.On() {
			continue
		}
		if b.HoldOrCarry() == book.HoldDay {
			sumDay += b.ShareOr1()
		} else {
			sumCarry += b.ShareOr1()
		}
	}
	for _, b := range books {
		ab := Book{Name: b.Name, Hold: b.HoldOrCarry(), Fixed: b.Fixed(), Enabled: b.On()}
		switch {
		case b.Fixed():
			ab.Base, ab.Cap = b.Seed, b.Seed
		case b.On():
			ab.Share = b.ShareOr1()
			if ab.Hold == book.HoldDay {
				ab.Base = a.Capital * ab.Share / sumDay
			} else {
				ab.Base = p.NightCap * ab.Share / sumCarry
			}
			ab.Cap = ab.Base * a.BorrowX
		default:
			ab.Share = b.ShareOr1() // 꺼진 장부는 몫이 없다 — 진입은 E_INACTIVE
		}
		p.byName[ab.Name] = len(p.Books)
		p.Books = append(p.Books, ab)
	}
	return p
}

// Book — 이름으로 찾는다.
func (p *Plan) Book(name string) (Book, bool) {
	if p == nil {
		return Book{}, false
	}
	i, ok := p.byName[name]
	if !ok {
		return Book{}, false
	}
	return p.Books[i], true
}
