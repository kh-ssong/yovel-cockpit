// Package guard 는 네트워크에 의존하지 않는 청산 층이다 (protocol.md §8 의 2층).
//
// 여기 있는 규칙은 릴레이가 끊겨도, 서버가 죽어도 그대로 동작해야 한다.
// 그래서 이 패키지는 **아무것도 조회하지 않는다** — 순수 함수만 있다.
//
// ★ 트레일·계단을 여기 구현하지 않는다. 서버가 매 완성봉마다 재계산해 stop 숫자만 내려보내고,
// 클라는 `price <= stop_price` 를 볼 뿐이다. 끊기면 트레일이 따라 올라가지 못할 뿐,
// 마지막 stop 은 그대로 유효하다.
package guard

import "time"

// Rules 는 포지션 하나에 걸린 로컬 청산 규칙. 서버가 준 숫자를 그대로 담는다.
type Rules struct {
	StopPrice  float64
	TimeExitAt *time.Time
}

type Reason string

const (
	ReasonNone Reason = ""
	ReasonStop Reason = "stop"
	ReasonTime Reason = "time"
)

// Decision 은 판정 결과.
//
// ★ Blind 가 따로 있는 이유: "청산 조건이 아니다" 와 "판단할 근거가 없다" 는 완전히 다른 말인데
// bool 하나로 합치면 둘이 같아 보인다. 시세가 끊긴 상태를 "안전하다"로 읽는 게 최악이다.
type Decision struct {
	Exit   bool
	Reason Reason
	// Blind — 시세가 늙어 stop 을 평가할 수 없었다.
	Blind bool
}

// Inputs 는 판정에 필요한 전부. 이 구조체 밖의 것을 읽지 않는다.
type Inputs struct {
	// LastPrice — 마지막 **완성봉 종가**. 봉 내 wick 으로 stop 을 잡지 않는다
	// (mean-reversion 계열은 노이즈 내성이 엣지라 wick 에 걸리면 엣지가 소멸한다).
	LastPrice float64
	// PriceAsOf — 그 종가의 시각.
	PriceAsOf time.Time
	Now       time.Time
	// MaxPriceAge — 이보다 늙은 시세로는 stop 을 평가하지 않는다. 0 이면 검사 안 함.
	MaxPriceAge time.Duration
}

// Evaluate 는 청산해야 하는지 본다.
//
// ★ 시세가 끊겼다고 자동 청산하지 않는다. 데이터 결손으로 파는 것은 그 자체가 사고이고
// (한 번의 피드 글리치가 전 포지션을 시장가로 털어낸다), 진입 실패와 달리 되돌릴 수 없다.
// 대신 Blind 를 올려서 호출자가 heartbeat(broker_ws) 와 긴급 알림으로 올리게 한다.
//
// ★ 반면 시간청산은 시세가 없어도 성립한다 — 시계만 있으면 되기 때문이다.
// 그래서 피드가 죽은 상태에서도 "장 마감 전 강제청산"은 계속 동작한다.
func Evaluate(r Rules, in Inputs) Decision {
	blind := in.MaxPriceAge > 0 && in.Now.Sub(in.PriceAsOf) > in.MaxPriceAge

	// 시간청산을 먼저 본다: 시세 유무와 무관하게 성립하는 유일한 규칙이라
	// 피드가 죽은 상황에서 마지막까지 남는 안전장치다.
	if r.TimeExitAt != nil && !in.Now.Before(*r.TimeExitAt) {
		return Decision{Exit: true, Reason: ReasonTime, Blind: blind}
	}

	if blind || in.LastPrice <= 0 || r.StopPrice <= 0 {
		return Decision{Blind: blind}
	}
	if in.LastPrice <= r.StopPrice {
		return Decision{Exit: true, Reason: ReasonStop}
	}
	return Decision{}
}
