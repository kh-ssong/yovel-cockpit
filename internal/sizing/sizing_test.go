package sizing

import (
	"math"
	"testing"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

func hasCode(codes []protocol.RejectCode, c protocol.RejectCode) bool {
	for _, x := range codes {
		if x == c {
			return true
		}
	}
	return false
}

// ★ whole-share fiction 의 실제 크기.
// 백테는 1.94주를 산 것으로 계산하는데 라이브는 1주를 산다 — 의도 비중의 절반이다.
// 이 왜곡이 조용하면 "백테는 14% 였는데 라이브는 7%" 인 상태를 아무도 모른 채 오래 간다.
func TestWholeShareFloorDistortsWeight(t *testing.T) {
	const capital = 1_000_000.0
	res := Shares(0.14, capital, 72_100, StockMarket())

	if res.Qty != 1 {
		t.Fatalf("qty=%v, 기대 1주 (140,000원 / 72,100원 = 1.94주)", res.Qty)
	}
	if math.Abs(res.RealizedWeight-0.0721) > 1e-9 {
		t.Fatalf("실현 비중 %v", res.RealizedWeight)
	}
	if d := res.Distortion(); d > -0.4 {
		t.Fatalf("왜곡이 %v — 절반 가까이 줄어든 걸 보고해야 한다", d)
	}
}

// 결과가 0주면 조용히 1주 사지 않는다. 그건 서버가 지시하지 않은 비중이다.
func TestZeroSharesIsCapitalReject(t *testing.T) {
	res := Shares(0.01, 1_000_000, 500_000, StockMarket()) // 예산 1만원, 주가 50만원
	if res.Qty != 0 || !hasCode(res.Codes, protocol.CodeCapital) {
		t.Fatalf("qty=%v codes=%v", res.Qty, res.Codes)
	}
}

func TestFractionalMarketKeepsWeight(t *testing.T) {
	res := Shares(0.25, 1_000_000, 137_000_000, Market{Fractional: true, LotSize: 0})
	if res.Qty <= 0 {
		t.Fatalf("코인은 분수 수량이 되어야 한다: %+v", res)
	}
	if math.Abs(res.RealizedWeight-0.25) > 1e-9 {
		t.Fatalf("분수인데 비중이 왜곡됐다: %v", res.RealizedWeight)
	}
}

// 최소주문금액 미달을 "그럼 조금 더" 로 처리하지 않는다.
func TestMinOrderValueRejectsInsteadOfRounding(t *testing.T) {
	m := StockMarket()
	m.MinOrderValue = 100_000
	res := Shares(0.05, 1_000_000, 40_000, m) // 예산 5만원 → 1주 4만원 < 최소 10만원
	if res.Qty != 0 || !hasCode(res.Codes, protocol.CodeCapital) {
		t.Fatalf("qty=%v codes=%v — 상향 반올림하면 안 된다", res.Qty, res.Codes)
	}
}

func TestBadInputs(t *testing.T) {
	cases := []struct {
		name                   string
		weight, capital, price float64
		want                   protocol.RejectCode
	}{
		{"비중 0", 0, 1_000_000, 100, protocol.CodeSchema},
		{"비중 1 초과", 1.5, 1_000_000, 100, protocol.CodeSchema},
		{"가격 0", 0.1, 1_000_000, 0, protocol.CodeSymbol},
		{"자본 0", 0.1, 0, 100, protocol.CodeCapital},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Shares(c.weight, c.capital, c.price, StockMarket())
			if !hasCode(res.Codes, c.want) {
				t.Fatalf("codes=%v, 기대 %s", res.Codes, c.want)
			}
			if res.Qty != 0 {
				t.Fatalf("거절인데 수량이 %v", res.Qty)
			}
		})
	}
}

func TestLotSize(t *testing.T) {
	m := Market{LotSize: 10}
	res := Shares(1, 1_000_000, 12_000, m) // 83.3주 → 로트 10 → 80주
	if res.Qty != 80 {
		t.Fatalf("qty=%v, 기대 80", res.Qty)
	}
}
