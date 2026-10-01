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
}

func (b Book) On() bool { return b.Enabled == nil || *b.Enabled }

// Key — 소스 식별자 문자열 (로그·API 용).
func Key(kid, scope string) string { return kid + "/" + scope }

type Set struct {
	books []Book
	deflt float64
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
//	  {"name": "klev",  "kid": "flat6-1", "scope": "intraday/klev",   "seed":  3000000},
//	  {"name": "coin",  "kid": "flat6-1", "scope": "scalp/coin",      "seed":  2000000, "enabled": false}
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
		Books []Book `json:"books"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return New(f.Books, defaultBudget)
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
