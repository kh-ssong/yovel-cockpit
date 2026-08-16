// Package engine 은 데몬의 상태를 소유한다.
//
// ★ 아직 브로커도 릴레이도 없다. 그래도 이 조각이 먼저 완결되는 게 맞다 —
// 설계 순서가 "계약 → (transport 없이) 루프백으로 완결 → transport → 서버" 이기 때문이다.
// 지금 이 패키지는 다운링크 바이트를 받아 판정하고, 무엇을 할지 계획까지 낸다.
// 빠진 것은 "그 계획을 실제로 내는 손"뿐이다.
package engine

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/reconcile"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
	"github.com/kh-ssong/yovel-cockpit/internal/version"
)

// Store 는 엔진이 필요로 하는 영속 조각만 추린 것.
//
// ★ nil 이어도 엔진은 돈다 — 단, 그러면 재시작에 가드가 풀리고 종결된 목표로 재진입한다.
// 그 상태를 정상처럼 보이게 두지 않으려고 인터페이스로 드러낸다.
type Store interface {
	LoadGuards(context.Context) (store.GuardState, error)
	SaveGuards(context.Context, store.GuardState) error
	TerminalIntents(context.Context) (map[string]struct{}, error)
	OpenIntents(context.Context) ([]protocol.Position, error)
	Ledger(context.Context, store.LedgerQuery) ([]store.Order, error)
}

type Config struct {
	// Store — 영속. nil 이면 메모리에만 산다.
	Store Store

	Mode         protocol.Mode
	Policy       protocol.Policy
	TargetMaxAge time.Duration
	MaxOrders    int
	// EngineBudget — 이 콕핏에 붙은 엔진의 예산. 사용자가 정한다 (엔진은 비중만 보낸다).
	// ★ 슬롯당이 아니라 엔진 전체다 (protocol.md §7.1).
	EngineBudget float64
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

	// terminal — 이미 끝난 intent_id 캐시 (원장에서 복원).
	terminal map[string]struct{}
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
		terminal:  map[string]struct{}{},
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
	e.persistGuardsLocked(c.Reason)
}

// persistGuardsLocked — ★ 재시작하면 풀리는 일시정지는 안전장치가 아니다.
func (e *Engine) persistGuardsLocked(reason string) {
	if e.cfg.Store == nil {
		return
	}
	_ = e.cfg.Store.SaveGuards(context.Background(), store.GuardState{
		Paused:          e.paused,
		BlockEntryUntil: e.blockEntryUntil,
		CircuitBreaker:  e.circuitBreaker,
		LiquidateAll:    e.liquidateAll,
		Reason:          reason,
	})
}

// Restore 는 재시작 후 로컬 원장에서 상태를 되살린다.
//
// ★ 이걸 건너뛰면 두 가지가 동시에 터진다: 걸어둔 pause 가 풀리고,
// 이미 끝난 목표로 재진입한다 (retained 목표가 그대로 다시 오므로).
func (e *Engine) Restore(ctx context.Context) error {
	if e.cfg.Store == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	g, err := e.cfg.Store.LoadGuards(ctx)
	if err != nil {
		return err
	}
	e.paused, e.blockEntryUntil = g.Paused, g.BlockEntryUntil
	e.circuitBreaker, e.liquidateAll = g.CircuitBreaker, g.LiquidateAll

	term, err := e.cfg.Store.TerminalIntents(ctx)
	if err != nil {
		return err
	}
	e.terminal = term

	// ★ 여기서 복원하는 포지션은 "브로커가 이럴 것이다" 라는 우리 기억이지 진실이 아니다.
	// 브로커가 붙으면 조회 결과로 덮어써야 한다 (SSOT 는 브로커).
	open, err := e.cfg.Store.OpenIntents(ctx)
	if err != nil {
		return err
	}
	e.positions = make(map[string]protocol.Position, len(open))
	for _, p := range open {
		e.positions[p.IntentID] = p
	}
	return nil
}

// MarkClosed 는 목표를 종결 처리한다 (청산 체결 후 호출).
func (e *Engine) MarkClosed(intentID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.terminal[intentID] = struct{}{}
	delete(e.positions, intentID)
}

// Ledger 는 매매기록을 준다. ★ mode 없이는 조회할 수 없다 (합산 = 허위 표시).
func (e *Engine) Ledger(ctx context.Context, mode protocol.Mode, limit int) ([]store.Order, error) {
	if e.cfg.Store == nil {
		return nil, nil
	}
	return e.cfg.Store.Ledger(ctx, store.LedgerQuery{Mode: mode, Limit: limit})
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

// UpsertPosition 은 포지션 하나를 갱신한다 (브로커 조회 결과 또는 방금 체결된 진입).
func (e *Engine) UpsertPosition(p protocol.Position) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.positions[p.IntentID] = p
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
		Budget:          e.cfg.EngineBudget,
		Price:           e.cfg.Price,
		Market:          e.cfg.Market,
		Terminal:        e.isTerminalLocked,
	})
}

func (e *Engine) isTerminalLocked(intentID string) bool {
	_, ok := e.terminal[intentID]
	return ok
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
