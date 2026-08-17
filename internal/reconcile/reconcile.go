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
	// AsOfBar — 이 계획이 근거한 목표 스냅샷의 기준봉 시각 (= 신호가 나온 시각).
	//
	// ★ 원장의 signal_ts 가 여기서 나온다. 이게 없으면 "신호 → 체결" 지연을 잴 근거가
	// 계획 안에 하나도 없고, 그러면 대신 손에 잡히는 값(entry.not_after 같은 것)을 쓰게 되는데
	// 그건 **미래 시각**이라 지연이 음수로 나온다 — 조용히, 그리고 계속.
	AsOfBar     time.Time    `json:"as_of_bar"`
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

	// Budget — 이 목표를 낸 엔진에 사용자가 배정한 예산 (원). 사이징의 분모다.
	//
	// ★ 슬롯당 자본이 아니다 (protocol.md §7.1). 배분은 2층이고, 슬롯 사이를 나누는 것은
	// 엔진이 weight 로 이미 했다 — 그건 백테 지식이라 사용자가 알 수 없다. 콕핏이 정하는 것은
	// "이 엔진에 얼마를 줄지" 하나뿐이다. 분모를 슬롯당으로 두면 슬롯 수만큼 노출이 곱해진다.
	//
	// ★ 두 번째 엔진이 붙으면 호출자가 kid 로 골라 넣는다 (architecture.md §4). 여기가 스칼라인
	// 것은 소스가 하나라서이지, 예산이 계좌 전체라는 뜻이 아니다.
	Budget float64
	// Price — 사이징 참조가. 없으면 그 종목은 진입하지 않는다.
	Price func(protocol.Symbol) (float64, bool)
	// Market — 종목별 주문 제약. nil 이면 주식 기본값.
	Market func(protocol.Symbol) sizing.Market

	// Terminal — 이미 종결된 intent_id 인가 (로컬 원장이 답한다).
	// ★ nil 이면 재진입 방지가 꺼진다. retained 목표는 재접속마다 그대로 다시 오므로,
	// stop 에 털린 자리에 같은 목표로 곧바로 재진입하게 된다.
	Terminal func(intentID string) bool
}

func (o Options) terminal(id string) bool {
	return o.Terminal != nil && o.Terminal(id)
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
	plan.AsOfBar = target.AsOfBar

	have := make(map[string]protocol.Position, len(actual))
	for _, p := range actual {
		have[p.IntentID] = p
	}
	wanted := make(map[string]struct{}, len(target.Targets))

	// book_state 는 포트폴리오 레벨 신호다. 모르는 값은 halted 로 읽는다(안전한 쪽).
	bookHalted := protocol.SafeBookState(target.BookState) == protocol.BookHalted
	localBlock := opt.entryBlocked()

	// ★ 이미 들고 있는 것이 예산을 먹고 있다.
	//
	// 이걸 안 빼면 스냅샷이 재발행될 때마다 예산 전액이 새로 생긴다 — 그리고 재발행(델타가 아니라
	// 전체 스냅샷)은 이 프로토콜의 **기본 동작**이라 매 봉 반복된다. 즉 조용히, 그러나 빠르게
	// 예산을 넘는다. 청산 주문이 나가는 자리도 아직 안 팔린 것이므로 그대로 센다.
	spent := committed(actual, opt)

	// ★ targets 순회 순서 = 엔진이 정한 우선순위 (protocol.md §6). 예산 소진도 이 순서를 따른다.
	// 정렬을 넣거나 map 으로 바꾸면 그 순간 우선순위가 사라진다 — 회귀 gate 가 이 순서를 지킨다.
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
				// ★ stop 과 같은 가드가 필요하다 (2026-08-17). 없을 땐 **매 틱 취소·재발행**
				//   이었다 — 실측으로 몇 분 만에 paper-tp-38 → 334. 라이브에선 주문 유량이고,
				//   더 나쁘게는 취소와 재발행 **사이에 TP 가 브로커에 없는 창**이 생긴다.
				//   하필 그 층의 존재 이유가 "데몬도 서버도 죽어도 이건 체결된다" 이다.
				//   ★ 아직 안 걸린 상태(TpOrderID 비어 있음)면 값이 같아도 걸어야 한다.
				if t.Exit.TpDelegate && t.Exit.TpPrice > 0 &&
					(pos.TpOrderID == "" || t.Exit.TpPrice != pos.TpArmed) {
					plan.TpUpdates = append(plan.TpUpdates, TpUpdate{Position: pos, To: t.Exit.TpPrice})
				}
			}
			plan.Acks = append(plan.Acks, ack(t.IntentID, "applied", nil))

		case action != protocol.ActionEnter:
			plan.Acks = append(plan.Acks, ack(t.IntentID, statusFor(codes), codes))

		case opt.terminal(t.IntentID):
			// 이미 끝난 목표다. 에러는 아니지만 침묵도 아니다 — 왜 안 샀는지 남긴다.
			plan.Acks = append(plan.Acks, ack(t.IntentID, "noop", []protocol.RejectCode{protocol.CodeTerminal}))

		case bookHalted:
			plan.Acks = append(plan.Acks, ack(t.IntentID, "rejected", []protocol.RejectCode{protocol.CodeLocalGuard}))

		case localBlock != "":
			plan.Acks = append(plan.Acks, ack(t.IntentID, "rejected", []protocol.RejectCode{localBlock}))

		default:
			enter, codes := buildEnter(t, opt, opt.Budget-spent)
			if len(codes) > 0 {
				plan.Acks = append(plan.Acks, ack(t.IntentID, "rejected", codes))
				continue
			}
			spent += enter.Notional
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

// committed 는 지금 보유가 예산에서 이미 먹고 있는 금액.
//
// ★ 평단이 비어 있으면(원장이 아직 못 채운 포지션) 현재가로라도 값을 매긴다. 0 으로 세면
// 그 포지션이 예산에서 사라져 그만큼 더 사게 되는데, 그건 "모르는 것"을 "없는 것"으로 읽는 것이다.
func committed(actual []protocol.Position, opt Options) float64 {
	var sum float64
	for _, p := range actual {
		price := p.AvgEntryPrice
		if price <= 0 && opt.Price != nil {
			if q, ok := opt.Price(p.Symbol); ok {
				price = q
			}
		}
		if price > 0 {
			sum += p.Qty * price
		}
	}
	return sum
}

// buildEnter — remaining 은 이번 스냅샷에서 아직 안 쓴 예산.
//
// ★ 남은 예산을 넘으면 **거절하지 축소하지 않는다.** 줄여서 사면 그건 엔진이 지시한 적 없는
// 비중이고 (§7 "조용히 1주" 금지와 같은 병), 사용자는 자기가 무엇을 못 샀는지도 모르게 된다.
// 거절은 ack{E_CAPITAL} 로 남으므로 화면에서 보인다.
func buildEnter(t protocol.Target, opt Options, remaining float64) (EnterOrder, []protocol.RejectCode) {
	// ★ 신호를 낸 쪽이 기준가를 실어 보냈으면 그걸 쓴다. 여기서 다시 조회하면
	// 신호를 낸 가격과 사이징한 가격이 갈리고, 종목 수만큼 왕복이 늘어난다.
	var price float64
	if t.Entry != nil && t.Entry.RefPrice > 0 {
		price = t.Entry.RefPrice
	} else {
		if opt.Price == nil {
			return EnterOrder{}, []protocol.RejectCode{protocol.CodeSymbol}
		}
		p, ok := opt.Price(t.Symbol)
		if !ok || p <= 0 {
			return EnterOrder{}, []protocol.RejectCode{protocol.CodeSymbol}
		}
		price = p
	}
	if t.Entry != nil && t.Entry.Mode == "limit" && t.Entry.LimitPrice > 0 {
		price = t.Entry.LimitPrice
	}

	res := sizing.Shares(t.Weight, opt.Budget, price, opt.market(t.Symbol))
	if len(res.Codes) > 0 {
		return EnterOrder{}, res.Codes
	}
	// 부동소수 오차로 마지막 한 건이 억울하게 잘리지 않도록 아주 작은 여유만 둔다.
	if res.Notional-remaining > 1e-6 {
		return EnterOrder{}, []protocol.RejectCode{protocol.CodeCapital}
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
