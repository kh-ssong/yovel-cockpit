package reconcile

import (
	"testing"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
)

func frac(f float64) *float64 { return &f }

func heldLot(qty, entry float64) protocol.Position {
	p := held("a", "005930", 900)
	p.Qty, p.EntryQty = qty, entry
	return p
}

func holdTarget(f float64) protocol.Target {
	t := openTarget("a", "005930")
	t.Exit.HoldFrac = frac(f)
	return t
}

func TestHoldFracSellsDownToFractionOfEntry(t *testing.T) {
	// 15주 × 0.5 = 7.5 → 8주 남기고 7주 판다.
	p := Build(book(holdTarget(0.5)), []protocol.Position{heldLot(15, 15)}, opts())
	if len(p.Exits) != 1 || p.Exits[0].Reason != "reduce" || p.Exits[0].Qty != 7 {
		t.Fatalf("%+v", p.Exits)
	}
}

// ★ 같은 스냅샷이 다시 와도 또 팔지 않는다 — 기준이 **최초** 수량이라서.
func TestHoldFracIsIdempotent(t *testing.T) {
	p := Build(book(holdTarget(0.5)), []protocol.Position{heldLot(8, 15)}, opts())
	if len(p.Exits) != 0 {
		t.Fatalf("이미 줄인 로트를 또 줄였다: %+v", p.Exits)
	}
	// 비율을 올려도 되사지 않는다.
	p = Build(book(holdTarget(0.9)), []protocol.Position{heldLot(8, 15)}, opts())
	if len(p.Enters)+len(p.Exits) != 0 {
		t.Fatalf("줄이기만 해야 한다: %+v", p)
	}
}

func TestHoldFracZeroIsFlat(t *testing.T) {
	p := Build(book(holdTarget(0)), []protocol.Position{heldLot(15, 15)}, opts())
	if len(p.Exits) != 1 || p.Exits[0].Qty != 0 || p.Exits[0].Reason != "flat" {
		t.Fatalf("%+v", p.Exits)
	}
}

// 최초 수량을 모르는 옛 로트는 줄이지 않고 사유를 남긴다 (지금 수량 기준이면 매번 또 줄인다).
func TestHoldFracWithoutEntryQtyIsNoted(t *testing.T) {
	p := Build(book(holdTarget(0.5)), []protocol.Position{heldLot(15, 0)}, opts())
	if len(p.Exits) != 0 || len(p.Notes) != 1 {
		t.Fatalf("exits=%+v notes=%v", p.Exits, p.Notes)
	}
}

// 코인 — 남길 몫이 최소주문금액 밑이면 먼지가 되므로 전량, 팔 몫이 밑이면 못 판다.
func TestHoldFracRespectsMinOrder(t *testing.T) {
	o := opts()
	o.Market = func(protocol.Symbol) sizing.Market { return sizing.Market{LotSize: 1e-8, MinOrderValue: 5000} }
	o.Price = func(protocol.Symbol) (float64, bool) { return 1000, true }

	p := Build(book(holdTarget(0.1)), []protocol.Position{heldLot(40, 40)}, o) // 남길 4개 = 4,000원 < 5,000
	if len(p.Exits) != 1 || p.Exits[0].Qty != 0 {
		t.Fatalf("먼지로 남길 뻔했다: %+v", p.Exits)
	}
	p = Build(book(holdTarget(0.9)), []protocol.Position{heldLot(40, 40)}, o) // 팔 4개 = 4,000원 < 5,000
	if len(p.Exits) != 0 || len(p.Notes) != 1 {
		t.Fatalf("최소주문 미만 매도를 냈다: %+v %v", p.Exits, p.Notes)
	}
}
