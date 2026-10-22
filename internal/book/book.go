// Package book 은 **활성화된 전략별 장부**다.
//
// 전략은 전부 서버 엔진에 있다. 콕핏은 그중 **자기에게 활성화된 것만** 집행한다 (pitwall
// architecture.md §12). 장부 하나 = 소스 하나 = (kid, scope):
//   - kid   — 서명키 = 발행자 (예: flat6-1)
//   - scope — 발행 범위 = `카테고리/playbook` (예: intraday/d205)
//
// 장부가 정하는 것: 이 전략을 켤지(enabled), 얼마를 줄지(seed = 사이징 분모이자 자본 한도).
// slot 은 장부 **안의** 회계 단위(변형)라 여기서 다루지 않는다 — 그 사이 분배는 weight 가 한다.
//
// ★ 과금·권한 차단은 여기가 아니다. 구독하지 않은 전략의 목표는 릴레이가 **아예 전달하지 않는다**
// (콕핏은 공개 저장소라 필터는 지우면 그만이다, §12.1). 여기의 enabled 는 **사용자 취향**이다 —
// 받은 전략 중 무엇을 돌릴지.
//
// 장부 파일이 없으면(옛 설정) 모든 소스가 --engine-budget 하나로 돈다. 장부가 하나라도 있으면
// **장부에 없는 소스의 진입은 E_INACTIVE** — 모르는 전략이 조용히 남의 돈으로 사지 않게.
package book

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Default — 장부 설정이 없을 때 모든 소스가 가는 곳.
const Default = "_default"

type Book struct {
	Name string `json:"name"`
	// Kid — 발행자. 비면 **아무 kid** 나 받는다 (개발용 devsign 키 등).
	Kid string `json:"kid,omitempty"`
	// Scope — 발행 범위. 빈 값은 scope 없이 발행하는 옛 발행자.
	Scope string `json:"scope"`
	// Seed — 이 전략에 배정한 돈. 사이징의 분모이자 자본 한도다.
	// ★ 자동 배분(파일 맨 위 capital — docs/allocation.md)에선 생략한다. 그러면 콕핏이 몫을 계산한다.
	//   적으면 그 장부는 «고정 장부»라 자동 배분에서 빠진다 (1주 검증·도장 대조용).
	Seed float64 `json:"seed"`
	// Share — 자동 배분의 비중 (많이 2 · 보통 1 · 적게 0.5). 생략 = 1. 고정 장부엔 안 쓴다.
	Share *float64 `json:"share,omitempty"`
	// Pct — 자동 배분에서 이 장부 몫 = capital × pct%. 밤 장부도 capital 기준이다 (밤 장부 pct 합 ≤ overnight_pct).
	// ★ 한 파일에서 share 와 섞지 않는다 — pct 를 쓰면 seed 없는 장부는 전부 pct (user 2026-10-21 «전략별 비중 %»).
	Pct *float64 `json:"pct,omitempty"`
	// Hold — carry(날을 넘겨 든다) · day(당일 청산). 자동 배분에서 몫을 밤 한도에서 받을지(carry),
	// 낮에 계좌 전체에서 받을지(day) 정한다. 생략 = carry — 밤 몫이 더 작아 안전한 쪽이다.
	// 목표의 청산 기한과 어긋나면 콕핏이 알린다 (그 장부의 hold 를 고칠 것).
	Hold string `json:"hold,omitempty"`
	// Enabled — 이 전략을 돌릴지. 생략하면 켬. 끄면 진입만 막고 청산은 계속한다.
	Enabled *bool `json:"enabled,omitempty"`
	// MaxQty — 진입 한 건의 수량 상한 (주·코인 개수). 0 = 없음.
	//
	// ★ 사이징(weight × seed ÷ 가격)이 정한 수량이 이보다 크면 **이 수량만** 산다 — 첫 실전을
	// «1주씩» 검증하는 용도 (user 2026-10-16). 시드 부족 축소(E_CAPITAL)와는 다른 층이다: 이건
	// 사용자가 미리 정한 크기라 조용한 축소가 아니다. 사이징이 0주면(시드 부족) 여전히 E_CAPITAL.
	MaxQty float64 `json:"max_qty,omitempty"`
	// Weight — 진입 한 건의 비중 덮어쓰기 (0 = 생략 = 목표의 weight 그대로). 건당 = 몫(seed) × 이 값.
	//
	// ★ 목표의 weight 는 flat6 이 정한 «전략 자신의 동시 보유» 라 모든 콕핏에 같다. 계좌가 작으면 그 크기로는
	// 1주도 못 사는 종목이 많다 (cf3 1/60 → 로트 ≈1.6만, 2026-10-22 진입 6건 중 5건 too_small). 여기 적으면
	// **이 콕핏만** 자리를 덜 나눠 크게 산다 — 그 대신 동시 보유는 borrow_x ÷ weight 건으로 준다
	// (user 2026-10-22 «flat6 기본값 + 콕핏 오버라이드»). 같은 종목 조각을 여럿 내는 묶음 발행이면 조각마다 이 값이다.
	Weight float64 `json:"weight,omitempty"`
	// PaperFeeBp — paper 체결의 편도 비용 (bp). 생략하면 거래소·기본 요율 (--paper-fee-bp-*).
	// ★ ETF 는 거래세가 없어 주식 요율을 물리면 안 된다 — ETF 장부에 {"buy":0,"sell":0} 처럼 준다
	// (user 2026-10-16 «주식/ETF 구분»). live 에선 쓰지 않는다 — 진짜 비용은 체결 통지가 준다.
	PaperFeeBp *Fee `json:"paper_fee_bp,omitempty"`
}

// Fee — 편도 비용 (bp).
type Fee struct {
	Buy  float64 `json:"buy"`
	Sell float64 `json:"sell"`
}

func (b Book) On() bool { return b.Enabled == nil || *b.Enabled }

// 장부의 반 (Book.Hold).
const (
	HoldCarry = "carry"
	HoldDay   = "day"
)

// HoldOrCarry — 생략한 hold 는 carry 다.
func (b Book) HoldOrCarry() string {
	if b.Hold == "" {
		return HoldCarry
	}
	return b.Hold
}

// ShareOr1 — 생략한 비중은 1 이다.
func (b Book) ShareOr1() float64 {
	if b.Share == nil {
		return 1
	}
	return *b.Share
}

// Fixed — seed 를 직접 적은 장부. 자동 배분에선 몫을 받지 않고 seed 대로 산다.
func (b Book) Fixed() bool { return b.Seed > 0 }

// Alloc — 자동 배분 설정 (books.json 맨 위 — docs/allocation.md). Capital 0 이고 FromAccount 도 아니면 꺼짐 (장부마다 seed).
type Alloc struct {
	// Capital — 맡길 돈. 콕핏 보유 합은 이걸 못 넘는다. FromAccount 면 0 (그날 계좌에서 읽는다).
	Capital float64 `json:"capital"`
	// FromAccount — "capital": "account". 맡길 돈 = 계좌 현금 한도 + 콕핏 보유 (원가) — 기동·매일 08:50 에 읽어
	// 그날 고정한다. 입금·출금·손익이 손대지 않아도 다음 날 몫에 들어간다 (user 2026-10-21).
	FromAccount bool `json:"from_account,omitempty"`
	// CapitalPct — FromAccount 일 때 계좌의 몇 %를 맡기나 (생략 = 100).
	CapitalPct float64 `json:"capital_pct,omitempty"`
	// CapitalMax — FromAccount 일 때 맡길 돈 상한 (0 = 없음).
	CapitalMax float64 `json:"capital_max,omitempty"`
	// OvernightPct — 밤 한도 (%). 날을 넘겨 드는 보유 합 ≤ Capital × 이 %.
	OvernightPct float64 `json:"overnight_pct"`
	// BorrowX — 한 장부가 자기 몫의 몇 배까지 쓸 수 있나 (남는 돈 빌려 쓰기).
	BorrowX float64 `json:"borrow_x"`
	// ReserveTrades — 스윙 예비분 = 밤 장부 건당 × 이 건수 (밤 장부 전체 합).
	ReserveTrades int `json:"reserve_trades"`
}

// 자동 배분 기본값 (docs/allocation.md §2).
const (
	DefaultOvernightPct  = 70.0 // reflex 스윙 pool 과 같은 숫자
	DefaultBorrowX       = 2.0
	DefaultReserveTrades = 10
)

func (a Alloc) On() bool { return a.Capital > 0 || a.FromAccount }

// CapitalAccount — "capital": "account" 의 값.
const CapitalAccount = "account"

// Key — 소스 식별자 문자열 (로그·API 용).
func Key(kid, scope string) string { return kid + "/" + scope }

type Set struct {
	books []Book
	deflt float64
	// overbook — 장부 합이 계좌보다 큰 것이 **의도된** 설정이다 (books.json "overbook": true).
	// 그러면 매일 예산 점검(08:50)이 «예산이 계좌를 넘는다» 로 울리지 않는다 — 모자람은 계좌 층에서
	// 줄여 사고(최소 1주) 0주면 E_CAPITAL 로 답한다 (user 2026-10-15).
	overbook bool
	// paperFees — 이 사용자 계좌의 거래소별 paper 편도 비용 (books.json "paper_fees").
	paperFees map[string]Fee
	// alloc — 자동 배분 (books.json "capital" …). 꺼져 있으면 장부마다 seed.
	alloc Alloc
}

// New — ★ 한 소스가 두 장부에 걸리면 기동을 거부한다. 어느 돈으로 샀는지 모르는 매수는
// 장부 대조를 통째로 무효로 만든다.
func New(books []Book, defaultBudget float64) (*Set, error) {
	return NewAlloc(books, defaultBudget, Alloc{})
}

// NewAlloc — 자동 배분이 켜져 있으면(a.Capital > 0) seed 를 생략한 장부가 몫을 받는다.
func NewAlloc(books []Book, defaultBudget float64, a Alloc) (*Set, error) {
	if a.On() {
		switch {
		case a.OvernightPct <= 0 || a.OvernightPct > 100:
			return nil, fmt.Errorf("overnight_pct %.4g — 0 초과 100 이하여야 한다", a.OvernightPct)
		case a.BorrowX < 1:
			return nil, fmt.Errorf("borrow_x %.4g — 1 이상이어야 한다 (1 = 빌려 쓰지 않음)", a.BorrowX)
		case a.ReserveTrades < 0:
			return nil, fmt.Errorf("reserve_trades %d — 음수", a.ReserveTrades)
		}
	}
	s := &Set{deflt: defaultBudget, alloc: a}
	names := map[string]bool{}
	for i, b := range books {
		if b.Name == "" || b.Name == Default {
			return nil, fmt.Errorf("장부 %d: 이름이 비었거나 예약어(%s)다", i, Default)
		}
		if names[b.Name] {
			return nil, fmt.Errorf("장부 이름 중복: %s", b.Name)
		}
		names[b.Name] = true
		switch {
		case b.Seed < 0 || (b.Seed == 0 && !a.On()):
			return nil, fmt.Errorf("장부 %s: seed 가 0 이하 (seed 를 생략하려면 파일 맨 위에 capital)", b.Name)
		case b.Share != nil && !a.On():
			return nil, fmt.Errorf("장부 %s: share 는 자동 배분(capital)에서만 쓴다", b.Name)
		case b.Share != nil && *b.Share <= 0:
			return nil, fmt.Errorf("장부 %s: share 가 0 이하 — 끄려면 enabled: false", b.Name)
		case b.Share != nil && b.Fixed():
			return nil, fmt.Errorf("장부 %s: seed 를 적은 고정 장부에 share 를 줬다 — 둘 중 하나만", b.Name)
		case b.Hold != "" && b.Hold != HoldCarry && b.Hold != HoldDay:
			return nil, fmt.Errorf("장부 %s: hold %q — carry 또는 day", b.Name, b.Hold)
		case b.Pct != nil && !a.On():
			return nil, fmt.Errorf("장부 %s: pct 는 자동 배분(capital)에서만 쓴다", b.Name)
		case b.Pct != nil && (*b.Pct <= 0 || *b.Pct > 100):
			return nil, fmt.Errorf("장부 %s: pct %.4g — 0 초과 100 이하 (끄려면 enabled: false)", b.Name, *b.Pct)
		case b.Pct != nil && b.Fixed():
			return nil, fmt.Errorf("장부 %s: seed 를 적은 고정 장부에 pct 를 줬다 — 둘 중 하나만", b.Name)
		case b.Pct != nil && b.Share != nil:
			return nil, fmt.Errorf("장부 %s: pct 와 share 를 같이 줬다 — 둘 중 하나만", b.Name)
		}
		if b.MaxQty < 0 {
			return nil, fmt.Errorf("장부 %s: max_qty 가 음수", b.Name)
		}
		if b.Weight < 0 || b.Weight > 1 {
			return nil, fmt.Errorf("장부 %s: weight %.4g — 0 초과 1 이하 (생략 = 목표의 weight)", b.Name, b.Weight)
		}
		if f := b.PaperFeeBp; f != nil && (f.Buy < 0 || f.Sell < 0) {
			return nil, fmt.Errorf("장부 %s: paper_fee_bp 가 음수", b.Name)
		}
		for _, o := range s.books {
			if o.Scope == b.Scope && (o.Kid == b.Kid || o.Kid == "" || b.Kid == "") {
				return nil, fmt.Errorf("장부 %s 와 %s 가 같은 소스(%s)를 잡는다", o.Name, b.Name, Key(b.Kid, b.Scope))
			}
		}
		s.books = append(s.books, b)
	}
	if err := checkPct(s.books, a); err != nil {
		return nil, err
	}
	return s, nil
}

// checkPct — pct 와 share 는 한 파일에서 섞지 않는다. 섞으면 «pct 장부가 share 장부 몫을 먼저 떼는가» 를
// 사용자가 짐작해야 한다. 밤 장부 pct 합이 밤 한도를 넘으면 기동 거부 — 몫을 다 못 쓰는 설정이다.
func checkPct(books []Book, a Alloc) error {
	var withPct, withoutPct string
	var night float64
	for _, b := range books {
		if b.Fixed() {
			continue
		}
		if b.Pct == nil {
			withoutPct = b.Name
			continue
		}
		withPct = b.Name
		if b.On() && b.HoldOrCarry() == HoldCarry {
			night += *b.Pct
		}
	}
	switch {
	case withPct == "":
		return nil
	case withoutPct != "":
		return fmt.Errorf("장부 %s 엔 pct 가 있는데 %s 엔 없다 — pct 를 쓰면 seed 없는 장부는 전부 pct (share 와 섞지 말 것)",
			withPct, withoutPct)
	case night > a.OvernightPct+1e-9:
		return fmt.Errorf("밤 장부(carry) pct 합 %.4g%% 가 밤 한도 overnight_pct %.4g%% 를 넘는다", night, a.OvernightPct)
	}
	return nil
}

// Load 는 books.json 을 읽는다. 파일이 없으면 장부 없음 — 에러가 아니다.
//
//	{"books": [
//	  {"name": "d205",  "kid": "flat6-1", "scope": "intraday/d205",   "seed": 16000000},
//	  {"name": "klev",  "kid": "flat6-1", "scope": "intraday/klev",   "seed":  3000000, "max_qty": 1},
//	  {"name": "coin",  "kid": "flat6-1", "scope": "scalp/coin",      "seed":  2000000, "enabled": false}
//	], "overbook": true,
//	 "paper_fees": {"KRX": {"buy": 0, "sell": 20}, "UPBIT": {"buy": 5, "sell": 5}}}
//
// 자동 배분 (docs/allocation.md) — 맡길 돈 하나와 비중만 적고, seed 를 생략한 장부는 콕핏이 몫을 계산한다:
//
//	{"capital": 1460000, "overnight_pct": 70,
//	 "books": [
//	  {"name": "swing_60m", "kid": "flat6-1", "scope": "kiwoom/swing_60m", "hold": "carry"},
//	  {"name": "push10",    "kid": "flat6-1", "scope": "kiwoom/push10",    "seed": 300000, "max_qty": 1}
//	]}
//
// 맡길 돈을 계좌에서 읽고 장부마다 계좌의 몇 %인지 적는다 (user 2026-10-21):
//
//	{"capital": "account", "capital_pct": 100, "overnight_pct": 70,
//	 "books": [
//	  {"name": "swing_60m", "kid": "flat6-1", "scope": "kiwoom/swing_60m", "hold": "carry", "pct": 23},
//	  {"name": "push10",    "kid": "flat6-1", "scope": "kiwoom/push10",    "hold": "day",   "pct": 16},
//	  {"name": "cf3",       "kid": "flat6-1", "scope": "kiwoom/cf3",       "hold": "day",   "pct": 50, "weight": 0.1}
//	]}
func Load(path string, defaultBudget float64) (*Set, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(nil, defaultBudget)
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Books     []Book         `json:"books"`
		Overbook  bool           `json:"overbook"`
		PaperFees map[string]Fee `json:"paper_fees"`
		// 자동 배분 — 비면 기본값 (포인터라 «0 을 적었다» 와 «안 적었다» 를 가른다).
		// capital 은 숫자(맡길 돈) 또는 "account" (계좌에서 읽는다).
		Capital       json.RawMessage `json:"capital"`
		CapitalPct    *float64        `json:"capital_pct"`
		CapitalMax    *float64        `json:"capital_max"`
		OvernightPct  *float64        `json:"overnight_pct"`
		BorrowX       *float64        `json:"borrow_x"`
		ReserveTrades *int            `json:"reserve_trades"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	a := Alloc{OvernightPct: DefaultOvernightPct, BorrowX: DefaultBorrowX, ReserveTrades: DefaultReserveTrades}
	if len(f.Capital) > 0 && string(f.Capital) != "null" {
		var word string
		if json.Unmarshal(f.Capital, &word) == nil {
			if word != CapitalAccount {
				return nil, fmt.Errorf("%s: capital %q — 숫자(맡길 돈) 또는 %q", path, word, CapitalAccount)
			}
			a.FromAccount, a.CapitalPct = true, 100
		} else if err := json.Unmarshal(f.Capital, &a.Capital); err != nil {
			return nil, fmt.Errorf("%s: capital — 숫자(맡길 돈) 또는 %q: %w", path, CapitalAccount, err)
		}
	}
	switch {
	case a.Capital < 0:
		return nil, fmt.Errorf("%s: capital 이 음수", path)
	case !a.On() && (f.OvernightPct != nil || f.BorrowX != nil || f.ReserveTrades != nil):
		// 자동 배분 설정만 적고 capital 을 빠뜨리면 조용히 옛 방식으로 돈다 — 사용자는 켠 줄 안다.
		return nil, fmt.Errorf("%s: overnight_pct·borrow_x·reserve_trades 는 capital 과 같이 적어야 한다", path)
	case !a.FromAccount && (f.CapitalPct != nil || f.CapitalMax != nil):
		return nil, fmt.Errorf("%s: capital_pct·capital_max 는 \"capital\": %q 일 때만 쓴다", path, CapitalAccount)
	case f.CapitalPct != nil && (*f.CapitalPct <= 0 || *f.CapitalPct > 100):
		return nil, fmt.Errorf("%s: capital_pct %.4g — 0 초과 100 이하", path, *f.CapitalPct)
	case f.CapitalMax != nil && *f.CapitalMax <= 0:
		return nil, fmt.Errorf("%s: capital_max %.4g — 0 초과 (상한이 없으면 생략)", path, *f.CapitalMax)
	}
	if f.CapitalPct != nil {
		a.CapitalPct = *f.CapitalPct
	}
	if f.CapitalMax != nil {
		a.CapitalMax = *f.CapitalMax
	}
	if f.OvernightPct != nil {
		a.OvernightPct = *f.OvernightPct
	}
	if f.BorrowX != nil {
		a.BorrowX = *f.BorrowX
	}
	if f.ReserveTrades != nil {
		a.ReserveTrades = *f.ReserveTrades
	}
	s, err := NewAlloc(f.Books, defaultBudget, a)
	if err != nil {
		return nil, err
	}
	s.overbook = f.Overbook
	for ex, fee := range f.PaperFees {
		// 오타("krx")가 조용히 무시되면 사용자는 자기 요율이 걸린 줄 안다 — 기동을 거부한다.
		if ex == "" || strings.ToUpper(ex) != ex || strings.ContainsAny(ex, " /") {
			return nil, fmt.Errorf("%s: paper_fees 의 거래소 %q — 대문자 거래소 이름이어야 한다 (KRX·UPBIT)", path, ex)
		}
		if fee.Buy < 0 || fee.Sell < 0 {
			return nil, fmt.Errorf("%s: paper_fees.%s 가 음수", path, ex)
		}
	}
	s.paperFees = f.PaperFees
	return s, nil
}

// PaperFees — 사용자가 정한 거래소별 paper 편도 비용 (books.json "paper_fees", 없으면 빈 맵).
//
// ★ 수수료는 계좌마다 다르다 (우대·이벤트·등급) — 콕핏을 쓰는 사람이 자기 요율을 자기 설정 파일에 둔다
// (user 2026-10-16). 우선순위: 장부 paper_fee_bp → 여기 거래소 요율 → 내장 기본(--paper-fee-bp-*·업비트 5bp).
func (s *Set) PaperFees() map[string]Fee {
	out := map[string]Fee{}
	if s == nil {
		return out
	}
	for ex, f := range s.paperFees {
		out[ex] = f
	}
	return out
}

// Overbook — 장부 합 > 계좌가 의도된 설정인가 (예산 점검이 경고하지 않는다).
func (s *Set) Overbook() bool { return s != nil && s.overbook }

// Alloc — 자동 배분 설정 (꺼져 있으면 Capital 0).
func (s *Set) Alloc() Alloc {
	if s == nil {
		return Alloc{}
	}
	return s.alloc
}

// Of — 소스의 장부. active=false 면 진입 금지 (장부에 없거나 꺼졌다).
func (s *Set) Of(kid, scope string) (name string, budget float64, active bool) {
	if s == nil || len(s.books) == 0 {
		// 장부 설정이 없다 = 옛 방식. 모든 소스가 엔진 예산 하나.
		if s == nil {
			return Default, 0, true
		}
		return Default, s.deflt, true
	}
	for _, b := range s.books {
		if b.Scope == scope && (b.Kid == "" || b.Kid == kid) {
			return b.Name, b.Seed, b.On()
		}
	}
	return "", 0, false
}

// MaxQty — 소스의 진입 수량 상한 (0 = 없음). 장부가 없거나 그 소스가 장부에 없으면 0.
func (s *Set) MaxQty(kid, scope string) float64 {
	if s == nil {
		return 0
	}
	for _, b := range s.books {
		if b.Scope == scope && (b.Kid == "" || b.Kid == kid) {
			return b.MaxQty
		}
	}
	return 0
}

// Weight — 소스의 진입 비중 덮어쓰기 (0 = 목표의 weight 그대로). 장부가 없거나 그 소스가 장부에 없으면 0.
func (s *Set) Weight(kid, scope string) float64 {
	if s == nil {
		return 0
	}
	for _, b := range s.books {
		if b.Scope == scope && (b.Kid == "" || b.Kid == kid) {
			return b.Weight
		}
	}
	return 0
}

// PaperFee — 소스의 paper 편도 비용. ok=false 면 장부가 정하지 않았다 (거래소·기본 요율을 쓴다).
func (s *Set) PaperFee(kid, scope string) (buy, sell float64, ok bool) {
	if s == nil {
		return 0, 0, false
	}
	for _, b := range s.books {
		if b.Scope == scope && (b.Kid == "" || b.Kid == kid) {
			if b.PaperFeeBp == nil {
				return 0, 0, false
			}
			return b.PaperFeeBp.Buy, b.PaperFeeBp.Sell, true
		}
	}
	return 0, 0, false
}

// Configured — 장부 설정이 있는가 (없으면 옛 방식).
func (s *Set) Configured() bool { return s != nil && len(s.books) > 0 }

// Books 는 설정된 장부들, 이름순.
func (s *Set) Books() []Book {
	if s == nil {
		return nil
	}
	out := append([]Book(nil), s.books...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Total 은 **켜진** 장부 시드 합 (장부가 없으면 엔진 예산) — paper 기본 시드·예산 점검의 기준.
// 자동 배분이면 맡긴 돈(capital)이다 — 고정 장부도 그 안에서 산다.
// ★ "capital": "account" 면 0 — 정해 둔 숫자가 없다 (그날 몫의 capital 은 alloc.Plan 에 있다).
func (s *Set) Total() float64 {
	if s == nil {
		return 0
	}
	if s.alloc.FromAccount {
		return 0
	}
	if s.alloc.On() {
		return s.alloc.Capital
	}
	if len(s.books) == 0 {
		return s.deflt
	}
	var t float64
	for _, b := range s.books {
		if b.On() {
			t += b.Seed
		}
	}
	return t
}
