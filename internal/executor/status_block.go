package executor

import (
	"context"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/ids"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// blockedStatus — 이 종목이 진입 차단 상태면 그 이름. 조회 실패는 막지 않는다(fail-open) —
// API 혼잡 한 번에 그날 진입을 통째로 잃으면 안 된다 (kt00011 사전 조회와 같은 원칙).
func (x *Executor) blockedStatus(ctx context.Context, s protocol.Symbol) string {
	if len(x.d.BlockStatus) == 0 {
		return ""
	}
	sc, ok := x.d.Broker.(broker.StatusChecker)
	if !ok {
		return ""
	}
	st, err := sc.SymbolStatus(ctx, s)
	if err != nil {
		return ""
	}
	for _, l := range st.Labels {
		if x.d.BlockStatus[l] {
			return l
		}
	}
	return ""
}

// blockEntry — 차단된 종목의 진입을 포기한다. 목표를 종결해 매 틱 다시 묻지 않는다 (오늘 안엔 안 풀린다).
func (x *Executor) blockEntry(ctx context.Context, now time.Time, t protocol.Target, kid, scope, label string, res *Result) {
	x.recordFill(ctx, t.Slot, kid, scope, store.Order{
		ID: ids.NewAt(now), IntentID: t.IntentID, Phase: "rejected", Symbol: t.Symbol, Side: "buy",
		Detail: "진입 차단 — " + label + " 종목 (--block-stock-status)",
	}, res)
	if err := x.d.Store.UpsertIntent(ctx, store.Intent{
		IntentID: t.IntentID, Slot: t.Slot, Kid: kid, Scope: scope, Group: t.Group, Symbol: t.Symbol, Side: string(t.Side),
	}); err == nil {
		_ = x.d.Store.CloseIntent(ctx, t.IntentID, "blocked", now)
	}
	x.d.Engine.MarkClosed(t.IntentID)
	x.noteClose(protocol.Position{IntentID: t.IntentID, Kid: kid, Scope: scope, Group: t.Group, Symbol: t.Symbol}, "blocked", 0, now)
	res.fail("진입 차단 %s(%s): %s 종목", t.IntentID, t.Symbol.Code, label)
}
