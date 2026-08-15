// Package broker 는 증권사와 말하는 경계다.
//
// ★ 드라이버는 증권사마다 **별도 파일**로 짓는다. 겉만 비슷하고 응답 스키마가 전부 달라서,
// 섣부른 공통화는 한 증권사의 착각을 전 증권사로 번지게 한다. 이 인터페이스는 데몬이
// 필요로 하는 **최소 동작**만 규정하고, 그 뒤는 드라이버가 알아서 한다.
package broker

import (
	"context"
	"errors"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// Holding — 브로커가 말하는 보유. ★ 실상태의 SSOT 는 이쪽이다.
//
// ★ 여기엔 intent_id 가 없다. 브로커는 "이 주식을 왜 샀는지" 를 모른다 —
// 계좌를 조회하면 "005930 14주" 만 나온다. 의도와의 연결은 로컬 원장(store.intents)이 들고 있고,
// 그래서 그 원장을 잃으면 전 포지션이 유령이 된다.
type Holding struct {
	Symbol   protocol.Symbol
	Qty      float64
	AvgPrice float64
	// Sellable — 매도 가능 수량. 미체결 매도가 걸려 있으면 Qty 보다 작다.
	// ★ Qty 로 팔려 들면 "잔고 부족" 을 만난다.
	Sellable float64
}

// Cash — ★ 두 값을 절대 합치지 말 것.
//
// Deposit(예수금)과 Orderable(주문가능금액)은 다른 층이다. 증거금·미결제·해외 원화배정 때문에
// 예수금이 있어도 주문가능금액이 0 일 수 있고, 사이징을 예수금으로 하면 그대로 주문 거부가 된다.
type Cash struct {
	Deposit   float64 // L1 — 계좌에 있는 돈
	Orderable float64 // L2 — 지금 실제로 주문에 쓸 수 있는 돈
	Currency  string
}

type Quote struct {
	Symbol protocol.Symbol
	// Price — 판정 기준가. ★ 완성봉 종가 규약을 따른다 (봉 내 wick 으로 판정하지 않는다).
	Price float64
	AsOf  time.Time
}

type OrderRequest struct {
	IntentID string
	Symbol   protocol.Symbol
	Qty      float64
	// LimitPrice — 0 이면 시장가.
	LimitPrice float64
	// RefPrice — 슬리피지를 재는 기준가 (보통 신호를 낸 봉의 종가).
	RefPrice float64
	// MaxSlipBp — 0 이면 제한 없음. 지정가로 표현할 수 있으면 드라이버가 그렇게 한다.
	MaxSlipBp int
}

// Fill — 체결 결과.
//
// ★ SlippageBp 와 FeeKRW 를 드라이버가 채운다. 여기서 안 재면 나중에 못 잰다 —
// 그리고 이 프로젝트에서 체결 규약은 결과를 지배한다.
type Fill struct {
	BrokerOrderID string
	Qty           float64
	Price         float64
	SubmittedAt   time.Time
	FilledAt      time.Time
	FeeKRW        float64
	SlippageBp    float64
	// Partial — 일부만 체결됐다. 나머지는 취소했거나 미체결로 남아 있다.
	Partial bool
}

// Broker 는 데몬이 증권사에 요구하는 전부.
type Broker interface {
	Name() string

	// Positions / Cash — 실상태 조회. reconcile 이 매 틱 부른다.
	Positions(ctx context.Context) ([]Holding, error)
	Cash(ctx context.Context) (Cash, error)

	// Quote — 사이징·stop 평가에 쓸 기준가.
	Quote(ctx context.Context, s protocol.Symbol) (Quote, error)

	Buy(ctx context.Context, req OrderRequest) (Fill, error)
	Sell(ctx context.Context, req OrderRequest) (Fill, error)

	// PlaceTP — 익절 지정가를 브로커에 위임한다.
	// ★ exit 3층 중 제일 튼튼한 층이다: 데몬도 서버도 죽어도 이건 체결된다.
	PlaceTP(ctx context.Context, s protocol.Symbol, qty, price float64) (orderID string, err error)
	CancelOrder(ctx context.Context, s protocol.Symbol, orderID string) error

	// LotSize / MinOrderValue — 사이징이 쓰는 종목별 제약.
	// ★ 이 표를 서버에 복제하면 그 표가 낡는다. 증권사와 붙어 있는 쪽이 답한다.
	LotSize(s protocol.Symbol) float64
	MinOrderValue(s protocol.Symbol) float64
}

var (
	ErrNotSupported   = errors.New("이 브로커가 지원하지 않는 동작")
	ErrInsufficient   = errors.New("주문가능금액 부족")
	ErrUnknownSymbol  = errors.New("모르는 종목")
	ErrNotEnoughShare = errors.New("매도 가능 수량 부족")
)

// SlippageBp 는 기준가 대비 체결 슬리피지를 bp 로 잰다 (매수는 비싸게 사면 +).
//
// ★ 부호 규약을 한 곳에 고정한다. 드라이버마다 다르게 재면 원장 전체가 못 쓰게 된다.
func SlippageBp(side string, ref, filled float64) float64 {
	if ref <= 0 || filled <= 0 {
		return 0
	}
	d := (filled - ref) / ref * 10000
	if side == "sell" {
		d = -d // 싸게 팔면 손해 = +
	}
	return d
}
