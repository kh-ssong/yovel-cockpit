// Package book 은 **전략별 장부**다 — 시드 하나를 여러 전략이 나눠 쓰지 않게 한다.
//
// ★ 왜 슬롯이 아니라 장부인가 (protocol.md §7.1 과의 관계):
// 슬롯마다 자본을 주면 한 엔진이 슬롯을 여러 개 쓸 때 노출이 슬롯 수만큼 곱해진다 — 그래서
// 콕핏은 "엔진 예산 하나" 로 갔다. 장부는 그 **엔진 예산을 전략 단위로 여러 개** 두는 것이다.
// 한 전략이 슬롯을 여럿 쓰면 그 슬롯들을 **한 장부에 묶는다** — 그 안의 분배는 여전히 weight 가 한다.
//
// 장부에 없는 슬롯은 기본 장부(= --engine-budget)로 간다. 기본 예산이 0 이면 그 슬롯은
// E_CAPITAL 로 거절된다 — 장부 설정에서 빠진 전략이 조용히 남의 돈으로 사지 않게.
package book

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
)

// Default — 장부에 없는 슬롯이 가는 곳의 이름.
const Default = "_default"

type Book struct {
	Name string `json:"name"`
	// Seed — 이 전략에 배정한 돈. 사이징의 분모이자 자본 한도다.
	Seed float64 `json:"seed"`
	// Slots — 이 장부로 묶을 슬롯. 비면 Name 과 같은 슬롯 하나.
	Slots []string `json:"slots,omitempty"`
}

type Set struct {
	books  []Book
	bySlot map[string]int
	deflt  float64
	byName map[string]int
}

// New 는 장부 목록과 기본 예산으로 만든다. ★ 한 슬롯이 두 장부에 있으면 거부한다 —
// 어느 돈으로 샀는지 모르는 매수는 장부 대조를 통째로 무효로 만든다.
func New(books []Book, defaultBudget float64) (*Set, error) {
	s := &Set{bySlot: map[string]int{}, byName: map[string]int{}, deflt: defaultBudget}
	for i, b := range books {
		if b.Name == "" || b.Name == Default {
			return nil, fmt.Errorf("장부 %d: 이름이 비었거나 예약어(%s)다", i, Default)
		}
		if b.Seed <= 0 {
			return nil, fmt.Errorf("장부 %s: seed 가 0 이하", b.Name)
		}
		if _, dup := s.byName[b.Name]; dup {
			return nil, fmt.Errorf("장부 이름 중복: %s", b.Name)
		}
		if len(b.Slots) == 0 {
			b.Slots = []string{b.Name}
		}
		for _, sl := range b.Slots {
			if j, dup := s.bySlot[sl]; dup {
				return nil, fmt.Errorf("슬롯 %q 가 장부 %s 와 %s 에 동시에 있다", sl, s.books[j].Name, b.Name)
			}
			s.bySlot[sl] = len(s.books)
		}
		s.byName[b.Name] = len(s.books)
		s.books = append(s.books, b)
	}
	return s, nil
}

// Load 는 books.json 을 읽는다. 파일이 없으면 장부 없음(기본 장부만) — 에러가 아니다.
//
//	{"books": [{"name": "d205", "seed": 16000000, "slots": ["d205", "klev"]}]}
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

// Of 는 슬롯이 속한 장부와 그 예산을 준다. nil Set 은 전부 기본 장부다.
func (s *Set) Of(slot string) (name string, budget float64) {
	if s == nil {
		return Default, 0
	}
	if i, ok := s.bySlot[slot]; ok {
		return s.books[i].Name, s.books[i].Seed
	}
	return Default, s.deflt
}

// Books 는 설정된 장부들(기본 장부 제외), 이름순.
func (s *Set) Books() []Book {
	if s == nil {
		return nil
	}
	out := append([]Book(nil), s.books...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Total 은 모든 장부 시드 + 기본 예산 — paper 계좌의 기본 시드로 쓴다.
func (s *Set) Total() float64 {
	if s == nil {
		return 0
	}
	t := s.deflt
	for _, b := range s.books {
		t += b.Seed
	}
	return t
}

func (s *Set) DefaultBudget() float64 {
	if s == nil {
		return 0
	}
	return s.deflt
}
