package engine

import (
	"fmt"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/alloc"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/reconcile"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
)

// 자동 배분 (docs/allocation.md) — 몫은 alloc.Plan(하루 고정), 소스를 가로지르는 한도는 계획마다 alloc.Gate.

// SetAlloc — 그날의 배분을 갈아 끼운다 (기동·매일 08:50). nil 이면 장부마다 seed (옛 방식).
func (e *Engine) SetAlloc(p *alloc.Plan) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.alloc = p
}

// AllocPlan — 지금 배분 (없으면 nil).
func (e *Engine) AllocPlan() *alloc.Plan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.alloc
}

// AllocReport — /v1/allocation.
type AllocReport struct {
	Plan *alloc.Plan `json:"plan"`
	// Free — 맡긴 돈 − 콕핏 보유. NightLeft — 밤 한도 − 밤 보유. Reserve — 지금 스윙 예비분.
	Free      float64       `json:"free"`
	NightLeft float64       `json:"night_left"`
	Reserve   float64       `json:"reserve"`
	Today     alloc.Report  `json:"today"`
	Yesterday *alloc.Report `json:"yesterday,omitempty"`
}

// AllocReport — 자동 배분이 꺼져 있으면 nil.
func (e *Engine) AllocReport(now time.Time) *AllocReport {
	e.mu.Lock()
	p := e.alloc
	if p == nil {
		e.mu.Unlock()
		return nil
	}
	g := e.newGateLocked(now)
	e.mu.Unlock()
	today, prev := e.meter.Report(now, p.Capital)
	return &AllocReport{Plan: p, Free: g.Free(), NightLeft: g.NightLeft(), Reserve: g.Reserve(), Today: today, Yesterday: prev}
}

// RecordAlloc — 집행 틱마다: 보유를 계측기에 적고, 이번 계획에서 한도에 걸린 진입을 센다.
// ★ 미리보기(/v1/plan)·ack 판정은 부르지 않는다 — 놓친 신호를 두 번 세지 않게.
func (e *Engine) RecordAlloc(now time.Time, evs []alloc.Event) {
	e.mu.Lock()
	p := e.alloc
	if p == nil {
		e.mu.Unlock()
		return
	}
	held := e.heldLocked(now)
	e.mu.Unlock()
	e.meter.Sample(now, p.Capital, held)
	e.meter.Record(now, p.Capital, evs)
}

// NoteCapitalShort — 브로커가 «0주» 라고 했다 (계좌 주문가능금액 부족). 놓친 신호로 센다.
func (e *Engine) NoteCapitalShort(kid, scope, intentID string, krw float64, now time.Time) {
	e.mu.Lock()
	p := e.alloc
	name, _, _ := e.cfg.Books.Of(kid, scope)
	e.mu.Unlock()
	if p == nil {
		return
	}
	e.meter.Record(now, p.Capital, []alloc.Event{{Book: name, IntentID: intentID, Reason: alloc.ReasonAccount, KRW: krw}})
}

// newGateLocked — 지금 보유로 이번 계획의 장부 밖 한도를 만든다. 자동 배분이 아니면 nil.
func (e *Engine) newGateLocked(now time.Time) *alloc.Gate {
	if e.alloc == nil {
		return nil
	}
	return alloc.NewGate(e.alloc, now, e.heldLocked(now), func(b string) float64 { return e.lastW[b] })
}

// heldLocked — 콕핏이 든 로트들 (원가 기준 — 예산 층 committed 와 같은 잣대).
// ★ 평단을 모르면(원장이 아직 못 채운 로트) 참조가로라도 센다 — 0 으로 세면 그만큼 더 산다.
func (e *Engine) heldLocked(now time.Time) []alloc.Held {
	out := make([]alloc.Held, 0, len(e.positions))
	for _, p := range e.positions {
		price := p.AvgEntryPrice
		if price <= 0 && e.cfg.Price != nil {
			if q, ok := e.cfg.Price(p.Symbol); ok {
				price = q
			}
		}
		name, _, _ := e.cfg.Books.Of(p.Kid, p.Scope)
		out = append(out, alloc.Held{Book: name, Value: p.Qty * price, Night: alloc.NightLot(p, now)})
	}
	return out
}

// HeldCost — 콕핏이 든 로트 원가 합 ("capital": "account" 의 맡길 돈 = 계좌 현금 한도 + 이것).
// 맡긴 돈 층(Gate.free = capital − 이 합)과 같은 잣대라, 계좌에서 읽은 날 아침의 free 가 곧 현금이다.
func (e *Engine) HeldCost(now time.Time) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var sum float64
	for _, h := range e.heldLocked(now) {
		sum += h.Value
	}
	return sum
}

// observeAllocLocked — 목표에서 배울 것: 장부별 최근 weight (예비분의 건당 추정), hold 가 어긋난 장부 (알림).
func (e *Engine) observeAllocLocked(now time.Time) {
	if e.alloc == nil {
		return
	}
	for src, st := range e.targets {
		name, _, _ := e.cfg.Books.Of(src.kid, src.scope)
		ab, ok := e.alloc.Book(name)
		if !ok {
			continue
		}
		for _, t := range st.it.Targets {
			if t.Want != protocol.WantOpen {
				continue
			}
			if t.Weight > 0 {
				e.lastW[name] = t.Weight
			}
			// 들고 있는 로트는 마지막 날 기한이 «오늘» 이 되므로 진입 전 목표로만 판단한다.
			if _, held := e.positions[t.IntentID]; held || ab.Fixed || e.isTerminalLocked(t.IntentID) {
				continue
			}
			got := alloc.HoldOf(t, now)
			if got == ab.Hold || !e.meter.NoteHold(now, e.alloc.Capital, name, got) {
				continue
			}
			if f := e.cfg.OnAllocWarn; f != nil {
				msg := fmt.Sprintf("자동 배분: 장부 %s 의 hold 는 %s 인데 목표(%s)의 청산 기한으로는 %s 다 — "+
					"몫이 %s 기준으로 잡혀 있다. books.json 의 hold 를 고칠 것 (다음 08:50·재시작부터 반영)",
					name, ab.Hold, t.IntentID, got, ab.Hold)
				go f(msg)
			}
		}
	}
}

// allocOptsLocked — 장부 하나의 사이징 분모·상한·장부 밖 한도. 자동 배분이 아니면 받은 budget 그대로.
func (e *Engine) allocOptsLocked(name string, budget float64, gate *alloc.Gate) (
	base, capv float64, admit func(protocol.Target, reconcile.EnterOrder, sizing.Market) (reconcile.EnterOrder, []protocol.RejectCode), bookName string) {

	ab, ok := e.alloc.Book(name)
	if e.alloc == nil || !ok {
		return budget, 0, nil, ""
	}
	base, capv, bookName = ab.Base, ab.Cap, name
	if gate != nil {
		admit = func(t protocol.Target, en reconcile.EnterOrder, m sizing.Market) (reconcile.EnterOrder, []protocol.RejectCode) {
			q, why := gate.Admit(name, t, en.Qty, en.Price, m)
			if q <= 0 && why != "" {
				return en, []protocol.RejectCode{protocol.CodeCapital}
			}
			if q < en.Qty {
				en.Qty, en.Notional = q, q*en.Price
				if base > 0 {
					en.RealizedWeight = en.Notional / base
				}
			}
			return en, nil
		}
	}
	return base, capv, admit, bookName
}
