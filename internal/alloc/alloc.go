// Package alloc 는 맡긴 돈 하나를 장부들에 나눈다 — 노는 돈을 줄이는 설계 (docs/allocation.md).
//
// 사용자는 맡길 돈(capital) · 밤 한도(overnight_pct) · 장부 비중(share)만 정한다. 나머지는 여기서 계산한다:
//
//   - 밤 장부(carry — 날을 넘겨 든다)는 capital × overnight_pct 를 비중대로 나눠 갖는다.
//   - 낮 장부(day — 15:15 면 현금으로 돌아온다)는 낮에 capital 전체를 비중대로 나눠 갖는다.
//   - 장부에 pct 를 적었으면 나눠 갖지 않고 몫 = capital × pct% 다 (밤·낮 모두 capital 기준).
//   - capital 은 books.json 숫자이거나, "account" 면 그날 계좌에서 읽은 값이다 (ComputeWith).
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
	Pct     float64 `json:"pct,omitempty"`
	Enabled bool    `json:"enabled"`
	// Base — 몫 = 사이징 분모 (건당 = Base × weight). 고정 장부는 seed.
	Base float64 `json:"base"`
	// Cap — 장부 상한 = 몫 × borrow_x. 고정 장부는 seed.
	Cap float64 `json:"cap"`
	// Weight — 장부 weight (books.json). 0 이면 목표의 weight (flat6) 를 그대로 쓴다.
	Weight float64 `json:"weight,omitempty"`
}

// Plan — 그날의 배분. 계산한 뒤엔 바꾸지 않는다 (엔진이 통째로 갈아 끼운다).
type Plan struct {
	Day     string  `json:"day"` // KST 날짜 — 이 날 동안 고정
	Capital float64 `json:"capital"`
	// CapitalFrom — capital 이 어디서 왔나: "books.json" (적은 숫자) · "account" (계좌에서 읽음) ·
	// "account_saved" (계좌 조회가 실패해 마지막으로 읽은 값).
	CapitalFrom   string  `json:"capital_from"`
	OvernightPct  float64 `json:"overnight_pct"`
	NightCap      float64 `json:"night_cap"` // capital × overnight_pct
	BorrowX       float64 `json:"borrow_x"`
	ReserveTrades int     `json:"reserve_trades"`
	// DayPct · NightPct — 켜진 낮·밤 장부 몫 합 ÷ capital (%, 고정 장부 seed 포함). 낮이 100 을 넘으면
	// 장부끼리 계좌를 선착순으로 나눠 쓴다 (맡긴 돈 층이 막는다).
	DayPct   float64 `json:"day_pct"`
	NightPct float64 `json:"night_pct"`
	Books    []Book  `json:"books"`

	byName map[string]int
}

// capital 출처 (Plan.CapitalFrom).
const (
	FromBooks        = "books.json"
	FromAccount      = "account"
	FromAccountSaved = "account_saved"
)

// Day — KST 날짜 문자열 (몫을 고정하는 단위).
func Day(t time.Time) string { return t.In(session.KST).Format("2006-01-02") }

// Compute — books.json 의 capital 숫자로 계산한다. 자동 배분이 꺼져 있으면 nil.
func Compute(set *book.Set, now time.Time) *Plan {
	return ComputeWith(set, set.Alloc().Capital, FromBooks, now)
}

// ComputeWith — 맡길 돈을 받아 계산한다 ("capital": "account" 면 그날 계좌에서 읽은 값). 자동 배분이 꺼져 있으면 nil.
func ComputeWith(set *book.Set, capital float64, from string, now time.Time) *Plan {
	a := set.Alloc()
	if !a.On() {
		return nil
	}
	p := &Plan{
		Day: Day(now), Capital: capital, CapitalFrom: from, OvernightPct: a.OvernightPct,
		NightCap: capital * a.OvernightPct / 100, BorrowX: a.BorrowX, ReserveTrades: a.ReserveTrades,
		byName: map[string]int{},
	}
	books := set.Books()
	var sumCarry, sumDay float64
	for _, b := range books {
		if b.Fixed() || !b.On() || b.Pct != nil {
			continue
		}
		if b.HoldOrCarry() == book.HoldDay {
			sumDay += b.ShareOr1()
		} else {
			sumCarry += b.ShareOr1()
		}
	}
	var dayBase, nightBase float64
	for _, b := range books {
		ab := Book{Name: b.Name, Hold: b.HoldOrCarry(), Fixed: b.Fixed(), Enabled: b.On(), Weight: b.Weight}
		switch {
		case b.Fixed():
			ab.Base, ab.Cap = b.Seed, b.Seed
		case b.Pct != nil:
			ab.Pct = *b.Pct
			if b.On() {
				ab.Base = capital * ab.Pct / 100
				ab.Cap = ab.Base * a.BorrowX
			}
		case b.On():
			ab.Share = b.ShareOr1()
			if ab.Hold == book.HoldDay {
				ab.Base = capital * ab.Share / sumDay
			} else {
				ab.Base = p.NightCap * ab.Share / sumCarry
			}
			ab.Cap = ab.Base * a.BorrowX
		default:
			ab.Share = b.ShareOr1() // 꺼진 장부는 몫이 없다 — 진입은 E_INACTIVE
		}
		if ab.Enabled {
			if ab.Hold == book.HoldDay {
				dayBase += ab.Base
			} else {
				nightBase += ab.Base
			}
		}
		p.byName[ab.Name] = len(p.Books)
		p.Books = append(p.Books, ab)
	}
	if capital > 0 {
		p.DayPct, p.NightPct = dayBase/capital*100, nightBase/capital*100
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
