// Package reconcile 은 목표상태와 실상태의 차이를 계획으로 바꾼다.
//
// ★ 순수 함수다 — 브로커도 네트워크도 모른다. 이유는 테스트가 아니라 안전이다:
// "무엇을 할지 정하는 것"과 "그걸 실제로 내는 것"을 섞으면, 계획 단계의 버그가
// 곧바로 주문으로 나간다. 여기서 나온 Plan 을 사람이 읽을 수 있어야 한다.
package reconcile

import (
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
)

// ★ json 태그를 다는 이유: 이 계획은 로컬 API(/v1/plan)로 그대로 나간다.
// 와이어 전체가 snake_case 인데 여기만 Go 필드명이면, UI 가 두 가지 규약을 동시에 다뤄야 한다.
type EnterOrder struct {
	Target   protocol.Target `json:"target"`
	Qty      float64         `json:"qty"`
	Price    float64         `json:"price"` // 사이징에 쓴 참조가. 지정가면 그대로, 시장가면 추정용
	Notional float64         `json:"notional"`
	// RealizedWeight — whole-share 내림 후 실제 비중 (의도와 다를 수 있다).
	RealizedWeight float64 `json:"realized_weight"`
}

type ExitOrder struct {
	Position protocol.Position `json:"position"`
	Reason   string            `json:"reason"` // flat | derisk
}

type StopUpdate struct {
	Position protocol.Position `json:"position"`
	From     float64           `json:"from"`
	To       float64           `json:"to"`
}

type TpUpdate struct {
	Position protocol.Position `json:"position"`
	From     float64           `json:"from"`
	To       float64           `json:"to"`
}

// Plan 은 이번 틱에 할 일 전부.
type Plan struct {
	Enters      []EnterOrder `json:"enters"`
	Exits       []ExitOrder  `json:"exits"`
	StopUpdates []StopUpdate `json:"stop_updates"`
	TpUpdates   []TpUpdate   `json:"tp_updates"`
	// Orphans — 목표에 없는데 실제로 들고 있는 포지션. ★ 자동 청산하지 않는다.
	Orphans []protocol.Position  `json:"orphans"`
	Acks    []protocol.AckIntent `json:"acks"`
	// DroppedEnters — 주문 상한에 걸려 이번 틱에 못 낸 진입 수.
	// ★ 조용한 절단 금지: 잘렸다는 사실 자체를 보고한다.
	DroppedEnters int `json:"dropped_enters"`
}

// Options 는 계획에 필요한 로컬 사실들.
type Options struct {
	Now time.Time

	// EntryAllowed — 봉투 만료·스냅샷 노화가 이미 반영된 값 (protocol.Admission.EntryAllowed).
	EntryAllowed bool
	// Paused / CircuitBreaker — 로컬 가드. ★ 서버가 끌 수 없다.
	Paused          bool
	CircuitBreaker  bool
	BlockEntryUntil *time.Time

	// MaxOrders — 이번 틱 주문 상한 (폭주 차단).
	MaxOrders int

	// SlotCapital — 슬롯이 쓸 수 있는 자본. 사용자가 정하고 클라가 안다.
	SlotCapital func(slot string) float64
	// Price — 사이징 참조가. 없으면 그 종목은 진입하지 않는다.
	Price func(protocol.Symbol) (float64, bool)
	// Market — 종목별 주문 제약. nil 이면 주식 기본값.
	Market func(protocol.Symbol) sizing.Market
}

func (o Options) market(s protocol.Symbol) sizing.Market {
	if o.Market == nil {
		return sizing.StockMarket()
	}
	return o.Market(s)
}

// entryBlocked 는 진입을 막는 로컬 사유를 준다 (없으면 빈 값).
func (o Options) entryBlocked() protocol.RejectCode {
	if o.Paused {
		return protocol.CodePaused
	}
	if o.BlockEntryUntil != nil && o.Now.Before(*o.BlockEntryUntil) {
		return protocol.CodePaused
	}
	if o.CircuitBreaker {
		return protocol.CodeLocalGuard
	}
	return ""
}

// Build 는 목표와 실상태를 비교해 계획을 만든다.
//
// 규칙표는 protocol.md §6 와 1:1 이다. 특히 마지막 줄:
// ★ 목표에 없는 실보유는 자동 청산하지 않는다. "목표에 없으면 판다" 는 얼핏 깔끔하지만,
// 서버가 상태를 한 번 잘못 계산하거나 스냅샷이 잘리면 사용자의 수동 보유분까지 털어버린다.
func Build(target protocol.IntentTarget, actual []protocol.Position, opt Options) Plan {
	var plan Plan

	have := make(map[string]protocol.Position, len(actual))
	for _, p := range actual {
		have[p.IntentID] = p
	}
	wanted := make(map[string]struct{}, len(target.Targets))

	// book_state 는 포트폴리오 레벨 신호다. 모르는 값은 halted 로 읽는다(안전한 쪽).
	bookHalted := protocol.SafeBookState(target.BookState) == protocol.BookHalted
	localBlock := opt.entryBlocked()

	for _, t := range target.Targets {
		wanted[t.IntentID] = struct{}{}
		pos, held := have[t.IntentID]

		action, codes := protocol.AdmitTarget(t, opt.Now, opt.EntryAllowed)

		switch {
		case action == protocol.ActionExit:
			if held {
				plan.Exits = append(plan.Exits, ExitOrder{Position: pos, Reason: "flat"})
				plan.Acks = append(plan.Acks, ack(t.IntentID, "applied", nil))
			} else {
				plan.Acks = append(plan.Acks, ack(t.IntentID, "noop", nil))
			}

		case held:
			// 이미 들고 있다 — 진입이 아니라 숫자 갱신만 한다.
			// ★ stop 갱신은 만료·일시정지와 무관하게 항상 반영한다. 청산 쪽을 막을 이유가 없다.
			if t.Exit != nil {
				if t.Exit.StopPrice > 0 && t.Exit.StopPrice != pos.StopArmed {
					plan.StopUpdates = append(plan.StopUpdates, StopUpdate{
						Position: pos, From: pos.StopArmed, To: t.Exit.StopPrice,
					})
				}
				if t.Exit.TpDelegate && t.Exit.TpPrice > 0 {
					plan.TpUpdates = append(plan.TpUpdates, TpUpdate{Position: pos, To: t.Exit.TpPrice})
				}
			}
			plan.Acks = append(plan.Acks, ack(t.IntentID, "applied", nil))

		case action != protocol.ActionEnter:
			plan.Acks = append(plan.Acks, ack(t.IntentID, statusFor(codes), codes))

		case bookHalted:
			plan.Acks = append(plan.Acks, ack(t.IntentID, "rejected", []protocol.RejectCode{protocol.CodeLocalGuard}))

		case localBlock != "":
			plan.Acks = append(plan.Acks, ack(t.IntentID, "rejected", []protocol.RejectCode{localBlock}))

		default:
			enter, codes := buildEnter(t, opt)
			if len(codes) > 0 {
				plan.Acks = append(plan.Acks, ack(t.IntentID, "rejected", codes))
				continue
			}
			plan.Enters = append(plan.Enters, enter)
			plan.Acks = append(plan.Acks, ack(t.IntentID, "applied", nil))
		}
	}

	for _, p := range actual {
		if _, ok := wanted[p.IntentID]; !ok {
			plan.Orphans = append(plan.Orphans, p)
			plan.Acks = append(plan.Acks, ack(p.IntentID, "rejected", []protocol.RejectCode{protocol.CodeOrphan}))
		}
	}

	applyOrderCap(&plan, opt.MaxOrders)
	return plan
}

// applyOrderCap 은 주문 상한을 건다.
//
// ★ 자를 때 청산은 절대 건드리지 않는다. 상한은 폭주를 막는 장치지 청산을 미루는 장치가 아니고,
// 진입 실패는 기회 상실(유한)이지만 청산 실패는 손실 노출(무한)이다.
func applyOrderCap(plan *Plan, max int) {
	if max <= 0 {
		return
	}
	room := max - len(plan.Exits)
	if room < 0 {
		room = 0
	}
	if len(plan.Enters) <= room {
		return
	}
	dropped := plan.Enters[room:]
	plan.Enters = plan.Enters[:room]
	plan.DroppedEnters = len(dropped)

	for _, e := range dropped {
		replaceAck(plan, e.Target.IntentID, "rejected", []protocol.RejectCode{protocol.CodeRate})
	}
}

func buildEnter(t protocol.Target, opt Options) (EnterOrder, []protocol.RejectCode) {
	if opt.Price == nil {
		return EnterOrder{}, []protocol.RejectCode{protocol.CodeSymbol}
	}
	price, ok := opt.Price(t.Symbol)
	if !ok || price <= 0 {
		return EnterOrder{}, []protocol.RejectCode{protocol.CodeSymbol}
	}
	if t.Entry != nil && t.Entry.Mode == "limit" && t.Entry.LimitPrice > 0 {
		price = t.Entry.LimitPrice
	}

	var capital float64
	if opt.SlotCapital != nil {
		capital = opt.SlotCapital(t.Slot)
	}
	res := sizing.Shares(t.Weight, capital, price, opt.market(t.Symbol))
	if len(res.Codes) > 0 {
		return EnterOrder{}, res.Codes
	}
	return EnterOrder{
		Target:         t,
		Qty:            res.Qty,
		Price:          price,
		Notional:       res.Notional,
		RealizedWeight: res.RealizedWeight,
	}, nil
}

func ack(id, status string, codes []protocol.RejectCode) protocol.AckIntent {
	return protocol.AckIntent{IntentID: id, Status: status, Codes: codes}
}

func replaceAck(plan *Plan, id, status string, codes []protocol.RejectCode) {
	for i := range plan.Acks {
		if plan.Acks[i].IntentID == id {
			plan.Acks[i] = ack(id, status, codes)
			return
		}
	}
	plan.Acks = append(plan.Acks, ack(id, status, codes))
}

func statusFor(codes []protocol.RejectCode) string {
	for _, c := range codes {
		if c == protocol.CodeExpired {
			return "expired"
		}
	}
	if len(codes) == 0 {
		return "noop"
	}
	return "rejected"
}
