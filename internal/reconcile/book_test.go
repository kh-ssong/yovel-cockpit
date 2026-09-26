package reconcile

import (
	"testing"

	bk "github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

func withSlot(t protocol.Target, slot string) protocol.Target { t.Slot = slot; return t }

func booked(t *testing.T, books []bk.Book, deflt float64) Options {
	t.Helper()
	s, err := bk.New(books, deflt)
	if err != nil {
		t.Fatal(err)
	}
	o := opts()
	o.Book = s.Of
	return o
}

// ★ 장부마다 분모가 다르다 — 같은 weight 라도 전략의 시드로 사이징한다.
func TestSizingUsesBookSeed(t *testing.T) {
	o := booked(t, []bk.Book{{Name: "d205", Seed: 16_000_000}}, 1_000_000)
	p := Build(book(withSlot(openTargetW("a", "005930", 0.0625), "d205")), nil, o)
	if len(p.Enters) != 1 || p.Enters[0].Qty != 1000 { // 1600만 × 0.0625 / 1000원
		t.Fatalf("%+v %+v", p.Enters, p.Acks)
	}
}

// ★ 한 전략이 예산을 다 써도 다른 전략의 진입은 막히지 않는다 — 그리고 그 반대로,
// 다 쓴 전략이 남의 남은 돈으로 사지도 않는다.
func TestBooksDoNotShareCash(t *testing.T) {
	o := booked(t, []bk.Book{
		{Name: "a", Seed: 1_000_000},
		{Name: "b", Seed: 1_000_000},
	}, 0)
	full := held("h", "000001", 0)
	full.Slot, full.Qty = "a", 1000 // a 장부 100만 전액 보유 중

	p := Build(book(
		withSlot(openTargetW("a2", "005930", 0.5), "a"),
		withSlot(openTargetW("b1", "000660", 0.5), "b"),
		flatTarget("h", "000001"), // 보유 포지션도 목표에 있어야 고아가 아니다
	), []protocol.Position{full}, o)

	if a, _ := ackFor(p, "a2"); len(a.Codes) == 0 || a.Codes[0] != protocol.CodeCapital {
		t.Fatalf("다 쓴 장부가 또 샀다: %+v", a)
	}
	if a, _ := ackFor(p, "b1"); a.Status != "applied" {
		t.Fatalf("다른 장부가 막혔다: %+v", a)
	}
}

// 장부에 없는 슬롯 + 기본 예산 0 → 거절. 장부 설정에서 빠진 전략이 조용히 사지 않는다.
func TestUnbookedSlotWithoutDefaultIsRejected(t *testing.T) {
	o := booked(t, []bk.Book{{Name: "d205", Seed: 16_000_000}}, 0)
	p := Build(book(withSlot(openTarget("x", "005930"), "stray")), nil, o)
	if a, _ := ackFor(p, "x"); len(a.Codes) == 0 || a.Codes[0] != protocol.CodeCapital {
		t.Fatalf("%+v", a)
	}
}
