package kiwoom

import "math"

// KRX 호가단위 — ★ 2023-01-25 시행 **현행** 규정.
//
// ★ 이 표가 낡으면 조용히 손해만 난다. 실측 사고(2026-08-12 발견): 구 규정(2010~2022)이
// 박혀 있었는데 1,000~2,000 / 10,000~20,000 / 100,000~200,000 세 대역이 각각 5배 과대였다.
// 구 단위는 현행 단위의 **배수**라 주문이 거부되지 않는다 — 거절 로그 하나 없이
// 지정가만 최대 +0.4%p 위로 올라가고 왕복 1틱 비용이 0.249% → 0.443% 가 됐다.
// ⟹ 이 상수표는 "언젠가 낡는다" 를 전제로 다뤄야 한다. 규정 개정 시 여기부터 고칠 것.
var krxTicks = []struct {
	above float64
	tick  float64
}{
	{500_000, 1000},
	{200_000, 500},
	{50_000, 100},
	{20_000, 50},
	{5_000, 10},
	{2_000, 5},
	{0, 1},
}

// TickSize 는 호가단위를 준다.
//
// ★ etp(ETF/ETN)는 위 표를 따르지 않는다 — 가격과 무관하게 5원이고,
// 2,000원 미만 저가 ETP 만 1원이다 (2023-10 개편).
func TickSize(price float64, etp bool) float64 {
	if etp {
		if price < 2_000 {
			return 1
		}
		return 5
	}
	for _, t := range krxTicks {
		if price > t.above {
			return t.tick
		}
	}
	return 1
}

// CeilToTick — 매도 지정가는 위로 올린다 (덜 받는 쪽으로 틀리지 않도록).
func CeilToTick(price float64, etp bool) float64 {
	t := TickSize(price, etp)
	if t <= 0 {
		return price
	}
	return math.Ceil(price/t) * t
}

// FloorToTick — 매수 지정가는 아래로 내린다 (더 주지 않도록).
func FloorToTick(price float64, etp bool) float64 {
	t := TickSize(price, etp)
	if t <= 0 {
		return price
	}
	return math.Floor(price/t) * t
}
