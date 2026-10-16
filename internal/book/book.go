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
	Seed float64 `json:"seed"`
	// Enabled — 이 전략을 돌릴지. 생략하면 켬. 끄면 진입만 막고 청산은 계속한다.
	Enabled *bool `json:"enabled,omitempty"`
	// MaxQty — 진입 한 건의 수량 상한 (주·코인 개수). 0 = 없음.
	//
	// ★ 사이징(weight × seed ÷ 가격)이 정한 수량이 이보다 크면 **이 수량만** 산다 — 첫 실전을
	// «1주씩» 검증하는 용도 (user 2026-10-16). 시드 부족 축소(E_CAPITAL)와는 다른 층이다: 이건
	// 사용자가 미리 정한 크기라 조용한 축소가 아니다. 사이징이 0주면(시드 부족) 여전히 E_CAPITAL.
	MaxQty float64 `json:"max_qty,omitempty"`
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

// Key — 소스 식별자 문자열 (로그·API 용).
func Key(kid, scope string) string { return kid + "/" + scope }

type Set struct {
	books []Book
	deflt float64
	// overbook — 장부 합이 계좌보다 큰 것이 **의도된** 설정이다 (books.json "overbook": true).
	// 그러면 매일 예산 점검(08:50)이 «예산이 계좌를 넘는다» 로 울리지 않는다 — 모자람은 계좌 층에서
	// 줄여 사고(최소 1주) 0주면 E_CAPITAL 로 답한다 (user 2026-10-15).
	overbook bool
}

// New — ★ 한 소스가 두 장부에 걸리면 기동을 거부한다. 어느 돈으로 샀는지 모르는 매수는
// 장부 대조를 통째로 무효로 만든다.
func New(books []Book, defaultBudget float64) (*Set, error) {
	s := &Set{deflt: defaultBudget}
	names := map[string]bool{}
	for i, b := range books {
		if b.Name == "" || b.Name == Default {
			return nil, fmt.Errorf("장부 %d: 이름이 비었거나 예약어(%s)다", i, Default)
		}
		if names[b.Name] {
			return nil, fmt.Errorf("장부 이름 중복: %s", b.Name)
		}
		names[b.Name] = true
		if b.Seed <= 0 {
			return nil, fmt.Errorf("장부 %s: seed 가 0 이하", b.Name)
		}
		if b.MaxQty < 0 {
			return nil, fmt.Errorf("장부 %s: max_qty 가 음수", b.Name)
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
	return s, nil
}

// Load 는 books.json 을 읽는다. 파일이 없으면 장부 없음 — 에러가 아니다.
//
//	{"books": [
//	  {"name": "d205",  "kid": "flat6-1", "scope": "intraday/d205",   "seed": 16000000},
//	  {"name": "klev",  "kid": "flat6-1", "scope": "intraday/klev",   "seed":  3000000, "max_qty": 1},
//	  {"name": "coin",  "kid": "flat6-1", "scope": "scalp/coin",      "seed":  2000000, "enabled": false}
//	], "overbook": true}
func Load(path string, defaultBudget float64) (*Set, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(nil, defaultBudget)
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Books    []Book `json:"books"`
		Overbook bool   `json:"overbook"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s, err := New(f.Books, defaultBudget)
	if err != nil {
		return nil, err
	}
	s.overbook = f.Overbook
	return s, nil
}

// Overbook — 장부 합 > 계좌가 의도된 설정인가 (예산 점검이 경고하지 않는다).
func (s *Set) Overbook() bool { return s != nil && s.overbook }

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
func (s *Set) Total() float64 {
	if s == nil {
		return 0
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
