// Package sizing 는 서버가 보낸 비중(weight)을 실제 수량으로 바꾼다.
//
// ★ 이 계산이 서버가 아니라 클라에 있는 이유 (protocol.md §7):
//
//  1. 잔고가 와이어에 안 오른다. 서버가 수량을 계산하려면 잔고를 알아야 한다.
//  2. ★ whole-share 는 로컬에서만 정직하다. 백테는 분수 주식으로 도는데 라이브는
//     정수로 내림된다. 소액 계좌에서 그 내림은 비중을 크게 왜곡하고, 그 왜곡은
//     가격·틱·계좌를 다 아는 클라만 정확히 안다.
//  3. 증권사별 최소주문금액·호가단위가 전부 다르다. 서버에 그 표를 복제하면 그 표가 낡는다.
package sizing

import (
	"math"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// Market 은 한 종목의 주문 제약. 증권사 드라이버가 채운다.
type Market struct {
	// Fractional — 분수 수량이 가능한가 (코인 true / 주식 false).
	Fractional bool
	// LotSize — 최소 주문 단위 (주식 보통 1).
	LotSize float64
	// MinOrderValue — 최소 주문 금액 (원). 0 이면 제약 없음.
	MinOrderValue float64
}

func StockMarket() Market { return Market{Fractional: false, LotSize: 1} }

// Result 는 수량과 함께 "얼마나 왜곡됐는지"를 같이 준다.
//
// ★ RealizedWeight 를 굳이 내보내는 이유: 소액 계좌에서 내림 하나가 비중을 두 배로
// 만들 수도, 절반으로 만들 수도 있다. 조용히 진행하면 "백테는 14% 였는데 라이브는 22%" 인
// 상태를 아무도 모른 채 오래 간다.
type Result struct {
	Qty            float64
	Notional       float64
	IntendedWeight float64
	RealizedWeight float64
	Codes          []protocol.RejectCode
}

// Distortion 은 의도 대비 실제 비중의 상대 오차. 0.5 면 의도의 절반만 담긴 것.
func (r Result) Distortion() float64 {
	if r.IntendedWeight == 0 {
		return 0
	}
	return r.RealizedWeight/r.IntendedWeight - 1
}

// Shares 는 비중을 수량으로 바꾼다.
//
// ★ engineBudget = 그 엔진에 배정한 예산 **전체**다. 슬롯당 자본이 아니다 (protocol.md §7.1) —
// 슬롯 사이의 분배는 엔진이 weight 로 이미 했다. 분모를 슬롯당으로 두면 슬롯 수만큼 노출이
// 곱해지는데, 스키마도 서명도 전부 통과하므로 어디에서도 안 걸린다.
//
// ★ 결과가 0주면 진입하지 않는다 (E_CAPITAL). 조용히 1주 사는 것은 금지 — 그건 비중이 아니다.
func Shares(weight, engineBudget, price float64, m Market) Result {
	res := Result{IntendedWeight: weight}

	if weight <= 0 || weight > 1 {
		res.Codes = []protocol.RejectCode{protocol.CodeSchema}
		return res
	}
	if price <= 0 {
		res.Codes = []protocol.RejectCode{protocol.CodeSymbol}
		return res
	}
	if engineBudget <= 0 {
		res.Codes = []protocol.RejectCode{protocol.CodeCapital}
		return res
	}

	budget := weight * engineBudget
	lot := m.LotSize
	if lot <= 0 {
		lot = 1
	}

	var qty float64
	if m.Fractional {
		qty = budget / price
	} else {
		qty = math.Floor(budget/price/lot) * lot
	}
	if qty <= 0 {
		res.Codes = []protocol.RejectCode{protocol.CodeCapital}
		return res
	}

	res.Qty = qty
	res.Notional = qty * price
	res.RealizedWeight = res.Notional / engineBudget

	if m.MinOrderValue > 0 && res.Notional < m.MinOrderValue {
		// 최소주문금액 미달을 "그럼 조금 더 사자"로 처리하지 않는다 — 그건 서버가 지시하지 않은 비중이다.
		res.Codes = []protocol.RejectCode{protocol.CodeCapital}
		res.Qty = 0
		res.Notional = 0
		res.RealizedWeight = 0
	}
	return res
}
