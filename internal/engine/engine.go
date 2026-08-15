// Package engine 은 데몬의 상태를 소유한다.
//
// ★ 아직 브로커도 릴레이도 없다. 그래도 이 조각이 먼저 완결되는 게 맞다 —
// 설계 순서가 "계약 → (transport 없이) 루프백으로 완결 → transport → 서버" 이기 때문이다.
// 지금 이 패키지는 다운링크 바이트를 받아 판정하고, 무엇을 할지 계획까지 낸다.
// 빠진 것은 "그 계획을 실제로 내는 손"뿐이다.
package engine

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/reconcile"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
	"github.com/kh-ssong/yovel-cockpit/internal/version"
)

type Config struct {
	Mode         protocol.Mode
	Policy       protocol.Policy
	TargetMaxAge time.Duration
	MaxOrders    int
	// SlotCapital — 슬롯별 자본. 사용자가 정한다 (서버는 비중만 보낸다).
	SlotCapital func(slot string) float64
	// Price — 참조가. 브로커가 붙기 전에는 없다.
	Price func(protocol.Symbol) (float64, bool)
	// Market — 종목별 주문 제약.
	Market func(protocol.Symbol) sizing.Market
}

type Engine struct {
	mu  sync.Mutex
	cfg Config

	startedAt time.Time
	guard     *protocol.Guard

	target     *protocol.IntentTarget
	appliedSeq uint64
	// envelopeEntryOK — 마지막 목표 봉투가 진입까지 허용했는가 (만료 여부).
	envelopeEntryOK bool

	positions map[string]protocol.Position

	paused          bool
	blockEntryUntil *time.Time
	circuitBreaker  bool
	// liquidateAll — de-risk liquidate 가 걸린 상태. 새 목표가 와도 유지된다.
	liquidateAll bool
}

func New(cfg Config, now time.Time) *Engine {
	if cfg.MaxOrders <= 0 {
		cfg.MaxOrders = 5
	}
	return &Engine{
		cfg:       cfg,
		startedAt: now,
		guard:     protocol.NewGuard(),
		positions: map[string]protocol.Position{},
	}
}

// Apply 는 다운링크 한 통을 받아 판정하고 상태에 반영한다. 반환값이 곧 업링크 ack 다.
func (e *Engine) Apply(raw []byte, now time.Time) protocol.Ack {
	e.mu.Lock()
	defer e.mu.Unlock()

	adm := protocol.Admit(raw, now, e.cfg.Policy, e.guard)

	ack := protocol.Ack{Status: adm.Status(), Codes: adm.Codes}
	if adm.Env != nil {
		ack.RefID = adm.Env.ID
		ack.RefTyp = adm.Env.Typ
		if adm.Env.Seq != nil {
			ack.RefSeq = *adm.Env.Seq
		}
	}
	if !adm.Accept {
		return ack
	}

	switch adm.Env.Typ {
	case protocol.TypeIntentTarget:
		var it protocol.IntentTarget
		if err := json.Unmarshal(adm.Env.Body, &it); err != nil {
			ack.Status = "rejected"
			ack.Codes = append(ack.Codes, protocol.CodeSchema)
			return ack
		}
		e.target = &it
		e.appliedSeq = ack.RefSeq
		e.envelopeEntryOK = adm.EntryAllowed
		ack.PerIntent = e.planLocked(now).Acks

	case protocol.TypeCmdDerisk:
		var c protocol.CmdDerisk
		if err := json.Unmarshal(adm.Env.Body, &c); err != nil {
			ack.Status = "rejected"
			ack.Codes = append(ack.Codes, protocol.CodeSchema)
			return ack
		}
		e.applyDerisk(c)
	}
	return ack
}

// applyDerisk — 나가는 방향만. ★ 디스크 영속은 아직 없다 (재시작하면 풀리는 pause 는
// 안전장치가 아니므로, 상태 저장이 붙기 전까지는 이 한계를 문서와 스냅샷에 드러낸다).
func (e *Engine) applyDerisk(c protocol.CmdDerisk) {
	switch c.Action {
	case protocol.DeriskLiquidate:
		e.liquidateAll = true
		e.paused = true // 팔면서 동시에 사는 일이 없도록
	case protocol.DeriskPause:
		e.paused = true
	case protocol.DeriskBlockEntry:
		e.blockEntryUntil = c.Until
	case protocol.DeriskResume:
		// ★ resume 은 사람의 명시적 행동으로만 온다. liquidate 플래그까지 같이 푼다.
		e.paused = false
		e.blockEntryUntil = nil
		e.liquidateAll = false
	}
}

// SetPositions 는 브로커 조회 결과를 반영한다 (브로커가 붙기 전에는 테스트가 쓴다).
// ★ 실상태의 SSOT 는 브로커지 우리 원장이 아니다.
func (e *Engine) SetPositions(ps []protocol.Position) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.positions = make(map[string]protocol.Position, len(ps))
	for _, p := range ps {
		e.positions[p.IntentID] = p
	}
}

// Plan 은 지금 무엇을 할지 계산한다. 주문을 내지는 않는다.
func (e *Engine) Plan(now time.Time) reconcile.Plan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.planLocked(now)
}

func (e *Engine) planLocked(now time.Time) reconcile.Plan {
	actual := make([]protocol.Position, 0, len(e.positions))
	for _, p := range e.positions {
		actual = append(actual, p)
	}

	if e.liquidateAll {
		// de-risk liquidate 는 목표와 무관하게 전량 청산이다.
		plan := reconcile.Plan{}
		for _, p := range actual {
			plan.Exits = append(plan.Exits, reconcile.ExitOrder{Position: p, Reason: "derisk"})
		}
		return plan
	}
	if e.target == nil {
		return reconcile.Plan{}
	}

	return reconcile.Build(*e.target, actual, reconcile.Options{
		Now:             now,
		EntryAllowed:    e.entryAllowedLocked(now),
		Paused:          e.paused,
		CircuitBreaker:  e.circuitBreaker,
		BlockEntryUntil: e.blockEntryUntil,
		MaxOrders:       e.cfg.MaxOrders,
		SlotCapital:     e.cfg.SlotCapital,
		Price:           e.cfg.Price,
		Market:          e.cfg.Market,
	})
}

// entryAllowedLocked — 진입이 살아 있는 조건 두 가지를 모두 본다:
// 봉투가 만료되지 않았고, 목표 스냅샷이 늙지 않았을 것.
// ★ 둘 중 어느 쪽이 꺼져도 청산은 막지 않는다.
func (e *Engine) entryAllowedLocked(now time.Time) bool {
	if e.target == nil || !e.envelopeEntryOK {
		return false
	}
	return !protocol.Stale(e.target.AsOfBar, now, e.cfg.TargetMaxAge)
}

func (e *Engine) Snapshot() protocol.StateSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UTC()
	v := version.Get()

	positions := make([]protocol.Position, 0, len(e.positions))
	for _, p := range e.positions {
		positions = append(positions, p)
	}

	var orphans []protocol.Symbol
	for _, o := range e.planLocked(now).Orphans {
		orphans = append(orphans, o.Symbol)
	}
	if orphans == nil {
		orphans = []protocol.Symbol{}
	}

	return protocol.StateSnapshot{
		AsOf: now,
		Daemon: protocol.DaemonInfo{
			Version: v.Version, SHA: v.SHA, StartedAt: &e.startedAt,
		},
		Mode:       e.cfg.Mode,
		AppliedSeq: e.appliedSeq,
		Guards: protocol.Guards{
			Paused:          e.paused,
			BlockEntryUntil: e.blockEntryUntil,
			CircuitBreaker:  e.circuitBreaker,
			// ★ 목표를 받은 적이 없으면 "진입 가능"이 아니라 "늙음"이다.
			// 빈 상태를 정상으로 보이게 두면 아무도 배선이 빠진 걸 눈치채지 못한다.
			TargetStale: !e.entryAllowedLocked(now),
		},
		Positions: positions,
		Orphans:   orphans,
	}
}
