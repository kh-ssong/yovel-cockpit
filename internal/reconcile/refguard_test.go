package reconcile

import (
	"testing"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

func refTarget(ref float64) protocol.Target {
	t := openTarget("a", "005930")
	t.Entry.RefPrice = ref
	return t
}

// ★ flat6 dummy 통합 테스트 재현 — 낡은 가격표(71,000 vs 실제 286,500)로 4배 수량을 살 뻔했다.
func TestRefPriceFarFromMarketIsRejected(t *testing.T) {
	o := opts()
	o.RefMaxDev = 0.15
	o.RefCheckPrice = func(protocol.Symbol) (float64, bool) { return 286_500, true }

	p := Build(book(refTarget(71_000)), nil, o)
	a, _ := ackFor(p, "a")
	if len(p.Enters) != 0 || len(a.Codes) == 0 || a.Codes[0] != protocol.CodeLocalGuard || len(p.Notes) != 1 {
		t.Fatalf("낡은 신호가로 진입했다: enters=%+v ack=%+v notes=%v", p.Enters, a, p.Notes)
	}
	// 허용 범위 안이면 신호가로 사이징한다 (시세로 바꾸지 않는다).
	p = Build(book(refTarget(290_000)), nil, o)
	if len(p.Enters) != 1 || p.Enters[0].Price != 290_000 {
		t.Fatalf("정상 신호가를 막았다: %+v", p)
	}
}

// 독립 시세를 모르면 막지 않는다 — 신호가가 있는 이유가 콕핏이 시세를 모를 때를 위해서다.
func TestRefPriceUncheckedWithoutQuote(t *testing.T) {
	o := opts()
	o.RefMaxDev = 0.15
	o.RefCheckPrice = func(protocol.Symbol) (float64, bool) { return 0, false }
	if p := Build(book(refTarget(71_000)), nil, o); len(p.Enters) != 1 {
		t.Fatalf("%+v", p)
	}
}
