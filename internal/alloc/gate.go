package alloc

import (
	"math"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
)

// 거절·축소 사유 (docs/allocation.md §3.3). ★ 와이어로는 전부 E_CAPITAL 이다 — flat6 계약은 그대로고,
// 사유는 콕핏 로그·API(/v1/allocation)에만 남는다.
const (
	ReasonTooSmall     = "too_small"     // 건당(몫 × weight)이 1단위 값보다 작다 — reconcile 이 건다
	ReasonBookCap      = "book_cap"      // ① 장부 상한 (몫 × borrow_x) — reconcile 이 건다
	ReasonOvernightCap = "overnight_cap" // ② 밤 한도
	ReasonReserve      = "reserve"       // ③ 스윙 예비분 (낮 진입만)
	ReasonCapital      = "capital"       // ④ 맡긴 돈 (capital − 콕핏 보유)
	ReasonAccount      = "account"       // ④ 계좌 주문가능금액 (브로커가 0주라고 했다)
)

// defaultCarryWeight — 밤 장부의 weight 를 아직 못 봤을 때의 건당 추정 (몫의 1/20 — flat6 스윙과 같은 값).
const defaultCarryWeight = 0.05

// Held — 지금 콕핏이 든 로트 하나 (원가 기준).
type Held struct {
	Book  string
	Value float64
	// Night — 날을 넘겨 드는 KRX 로트 (청산 기한이 오늘이 아니거나 없다).
	Night bool
}

// Event — 진입 한 건이 한도에 걸렸다. 엔진이 계측기(Meter)로 넘긴다.
type Event struct {
	Book     string  `json:"book"`
	IntentID string  `json:"intent_id"`
	Reason   string  `json:"reason"`
	KRW      float64 `json:"krw"` // 거절이면 원래 금액, 축소면 깎인 금액
	Shrunk   bool    `json:"shrunk,omitempty"`
}

// IsKRX — 장 마감이 있는 거래소라 밤/낮을 가르는가 (업비트는 24시간이라 안 가른다).
func IsKRX(s protocol.Symbol) bool { return s.Exchange == "KRX" }

// NightTarget — 이 진입이 날을 넘길 수 있는가. 청산 기한이 오늘(KST)이면 낮, 아니면(내일 이후·기한 없음) 밤.
// KRX 가 아니면 false (밤 한도·예비분 대상이 아니다).
func NightTarget(t protocol.Target, now time.Time) bool {
	if !IsKRX(t.Symbol) {
		return false
	}
	return t.Exit == nil || !sameDay(t.Exit.TimeExitAt, now)
}

// NightLot — 이 로트가 오늘 밤을 넘기는가 (NightTarget 의 보유판).
func NightLot(p protocol.Position, now time.Time) bool {
	return IsKRX(p.Symbol) && !sameDay(p.TimeExitAt, now)
}

func sameDay(at *time.Time, now time.Time) bool {
	return at != nil && Day(*at) == Day(now)
}

// HoldOf — 목표의 청산 기한으로 본 반 (carry | day). 장부 hold 와 어긋나는지 알리는 데 쓴다.
func HoldOf(t protocol.Target, now time.Time) string {
	if !IsKRX(t.Symbol) || NightTarget(t, now) {
		return "carry"
	}
	return "day"
}

// Gate — 한 번의 계획(모든 소스)에 걸친 밤 한도 · 스윙 예비분 · 맡긴 돈. 엔진이 계획마다 새로 만든다.
//
// ★ 소스마다 따로 계획해도 이 셋은 계좌 전체의 돈이라 한 Gate 를 같이 쓴다 — 앞 소스가 산 만큼 뒤 소스의
// 남은 돈이 준다 (같은 틱 안의 순서 = (kid, scope) 사전순 · targets[] 순서, protocol.md §7.1.2).
type Gate struct {
	p         *Plan
	now       time.Time
	free      float64 // capital − 콕핏 보유 합
	nightLeft float64 // 밤 한도 − 밤 보유 합
	reserve   float64 // 스윙 예비분 — 낮 진입이 못 건드리는 돈
	events    []Event
}

// NewGate — weightOf: 밤 장부의 최근 목표 weight (모르면 0 → 1/20 로 본다). 건당 추정(예비분)에만 쓴다.
func NewGate(p *Plan, now time.Time, held []Held, weightOf func(book string) float64) *Gate {
	g := &Gate{p: p, now: now}
	if p == nil {
		return g
	}
	var total, night float64
	byBook := map[string]float64{}
	for _, h := range held {
		total += h.Value
		byBook[h.Book] += h.Value
		if h.Night {
			night += h.Value
		}
	}
	g.free = p.Capital - total
	g.nightLeft = p.NightCap - night

	// 예비분 = min(reserve_trades × 밤 장부 건당 최대, 밤 한도 남은 돈, 밤 장부 남은 상한 합)
	var perTrade, capLeft float64
	for _, b := range p.Books {
		if b.Hold != "carry" || !b.Enabled {
			continue
		}
		w := defaultCarryWeight
		if weightOf != nil {
			if v := weightOf(b.Name); v > 0 {
				w = v
			}
		}
		perTrade = math.Max(perTrade, b.Base*w)
		capLeft += math.Max(0, b.Cap-byBook[b.Name])
	}
	g.reserve = math.Max(0, math.Min(float64(p.ReserveTrades)*perTrade, math.Min(g.nightLeft, capLeft)))
	return g
}

// Reserve — 지금 스윙 예비분 (API·로그용).
func (g *Gate) Reserve() float64 { return g.reserve }

// Free — 맡긴 돈 − 콕핏 보유 (이번 계획에서 산 만큼 준다).
func (g *Gate) Free() float64 { return g.free }

// NightLeft — 밤 한도 − 밤 보유.
func (g *Gate) NightLeft() float64 { return g.nightLeft }

// Events — 이번 계획에서 걸린 진입들.
func (g *Gate) Events() []Event { return g.events }

// Admit — 장부 상한(reconcile)을 통과한 진입 한 건에 밤 한도 · 스윙 예비분 · 맡긴 돈을 건다.
//
//   - 밤 한도는 사용자가 정한 한도라 **거절**한다 (장부 층과 같다).
//   - 예비분·맡긴 돈은 그 순간의 돈 사정이라 **줄여 산다** (최소 1단위) — 2026-10-15 user «줄여 사는 건 계좌 층만».
//
// 수량을 돌려준다. 0 이면 거절이고 reason 이 사유다. 줄였으면 reason 이 줄인 사유다.
// 통과한 만큼 이번 계획의 남은 돈을 깎는다 (뒤 소스가 같은 돈을 두 번 쓰지 않게).
func (g *Gate) Admit(bookName string, t protocol.Target, qty, price float64, m sizing.Market) (float64, string) {
	if g.p == nil || qty <= 0 || price <= 0 {
		return qty, ""
	}
	notional := qty * price
	night := NightTarget(t, g.now)
	day := IsKRX(t.Symbol) && !night

	if night && notional > g.nightLeft+1e-6 {
		g.events = append(g.events, Event{Book: bookName, IntentID: t.IntentID, Reason: ReasonOvernightCap, KRW: notional})
		return 0, ReasonOvernightCap
	}
	avail, reason := g.free, ReasonCapital
	if day && notional > g.free-g.reserve+1e-6 && notional <= g.free+1e-6 {
		reason = ReasonReserve // 맡긴 돈은 남지만 스윙 몫이다
	}
	if day {
		avail = g.free - g.reserve
	}
	out := qty
	if notional > avail+1e-6 {
		out = fit(avail, price, m)
		if out <= 0 {
			g.events = append(g.events, Event{Book: bookName, IntentID: t.IntentID, Reason: reason, KRW: notional})
			return 0, reason
		}
		g.events = append(g.events, Event{Book: bookName, IntentID: t.IntentID, Reason: reason,
			KRW: notional - out*price, Shrunk: true})
	} else {
		reason = ""
	}
	spent := out * price
	g.free -= spent
	if night {
		g.nightLeft -= spent
		g.reserve = math.Max(0, g.reserve-spent) // 스윙이 예비분을 쓴다
	}
	return out, reason
}

// fit — 돈 budget 으로 살 수 있는 최대 수량 (주문 단위·최소주문금액). 못 사면 0.
func fit(budget, price float64, m sizing.Market) float64 {
	if budget <= 0 {
		return 0
	}
	var q float64
	if m.Fractional {
		q = budget / price
	} else {
		lot := m.LotSize
		if lot <= 0 {
			lot = 1
		}
		q = math.Floor(budget/price/lot) * lot
	}
	if q <= 0 || (m.MinOrderValue > 0 && q*price < m.MinOrderValue) {
		return 0
	}
	return q
}

// inMarket — 가동률의 «장중» (09:00 ≤ t < 15:30 KST, 주말 제외 — 휴장일은 안 본다).
func inMarket(t time.Time) bool {
	k := t.In(session.KST)
	if k.Weekday() == time.Saturday || k.Weekday() == time.Sunday {
		return false
	}
	hm := k.Hour()*60 + k.Minute()
	return hm >= 9*60 && hm < 15*60+30
}

func afterClose(t time.Time) bool {
	k := t.In(session.KST)
	return k.Hour()*60+k.Minute() >= 15*60+30
}
