package upbit

import "math"

// 업비트 KRW 마켓 호가단위.
//
// ★ reflex-agent 라이브(`app/utils/tick_utils.py`)에서 그대로 옮겼다. 시장가 주문에는 안 쓰이고
// **지정가(TP)에만** 쓰인다. 업비트가 표를 바꾸면 지정가가 거부되거나(단위 미달) 조용히
// 불리해진다(단위 과대) — 키움 표가 한 번 그렇게 낡았다. 개정 시 여기부터 고칠 것.
var krwTicks = []struct {
	above float64
	tick  float64
}{
	{2_000_000, 1000},
	{1_000_000, 500},
	{500_000, 100},
	{100_000, 50},
	{10_000, 10},
	{1_000, 5},
	{100, 1},
	{10, 0.1},
	{1, 0.01},
	{0, 0.001},
}

func TickSize(price float64) float64 {
	for _, t := range krwTicks {
		if price >= t.above {
			return t.tick
		}
	}
	return 0.001
}

// CeilToTick — 매도 지정가. 올린다 (내리면 의도보다 싸게 팔린다).
//
// ★ 소수 단위(0.1·0.01·0.001)는 나눗셈 표현 오차로 정확히 틱 위의 값이 한 틱 튄다.
// 1e-9 여유를 두고, 결과를 소수 3자리로 되돌려 표현 오차를 지운다.
func CeilToTick(p float64) float64 {
	t := TickSize(p)
	return round3(math.Ceil(p/t-1e-9) * t)
}

// FloorToTick — 매수 지정가. 내린다 (올리면 의도보다 비싸게 산다).
func FloorToTick(p float64) float64 {
	t := TickSize(p)
	return round3(math.Floor(p/t+1e-9) * t)
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
