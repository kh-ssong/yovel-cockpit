package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/ids"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// foreignSells — since 이후 이 종목의 **콕핏이 내지 않은** 매도 체결 합 (HTS·앱 수동 매도, 다른 봇).
//
// ★ 콕핏 주문번호(원장·진행 중·위임 TP)를 빼고 남는 것만 센다 — 자기 매도를 수동 매도로 읽으면
// 형제 로트를 잘못 닫는다 (reflex 2026-08-08). 모르면 ok=false (조회 실패를 "없음" 으로 읽지 않는다).
func (x *Executor) foreignSells(ctx context.Context, s protocol.Symbol, since time.Time) (qty, avg, fee float64, n int, ok bool) {
	fl, isFL := x.d.Broker.(broker.FillLister)
	if !isFL {
		return 0, 0, 0, 0, false
	}
	fills, err := fl.SellFills(ctx, s)
	if err != nil {
		return 0, 0, 0, 0, false
	}
	known, err := x.d.Store.KnownOrderIDs(ctx)
	if err != nil {
		return 0, 0, 0, 0, false
	}
	var amt float64
	priced := true
	for _, f := range fills {
		if known[f.OrderID] || (!since.IsZero() && !f.At.IsZero() && f.At.Before(since)) {
			continue
		}
		qty += f.Qty
		amt += f.Qty * f.Price
		fee += f.FeeKRW
		n++
		if f.Price <= 0 {
			priced = false
		}
	}
	if qty > 0 && priced {
		avg = amt / qty
	}
	return qty, avg, fee, n, true
}

// closeManual — 콕핏 밖 매도로 사라진 로트를 그 체결가로 닫는다 (가격을 모르면 0 — 지어내지 않는다).
func (x *Executor) closeManual(ctx context.Context, now time.Time, p protocol.Position, price, fee float64,
	detail string, res *Result) {
	o := store.Order{
		ID: ids.NewAt(now), IntentID: p.IntentID, Phase: "exit_filled", Symbol: p.Symbol, Side: "sell",
		Qty: p.Qty, Price: price, FeeKRW: fee, ExitReason: "manual", Source: store.SourceManual, Detail: detail,
	}
	if price > 0 {
		o.RealizedPct = realizedPct(p.AvgEntryPrice, price)
	}
	x.recordFill(ctx, p.Slot, p.Kid, p.Scope, o, res)
	if err := x.d.Store.CloseIntent(ctx, p.IntentID, "manual", now); err != nil {
		res.fail("종결 %s: %v", p.IntentID, err)
		return
	}
	x.d.Engine.MarkClosed(p.IntentID)
	x.noteClose(p, "manual", price, now)
	res.ClosedByBroker++
}

// attributeVanished — 종목이 계좌에서 사라졌다. 이 종목의 로트들(lots)을 콕핏 밖 매도 체결가로 닫는다.
// 체결이 수량을 다 설명하지 못하면 가격 미상으로 닫는다 (어쨌든 실물이 없다).
func (x *Executor) attributeVanished(ctx context.Context, now time.Time, lots []protocol.Position, res *Result) {
	var total float64
	since := time.Time{}
	for _, p := range lots {
		total += p.Qty
		if p.EntryAt != nil && (since.IsZero() || p.EntryAt.Before(since)) {
			since = *p.EntryAt
		}
	}
	qty, avg, fee, n, ok := x.foreignSells(ctx, lots[0].Symbol, since)
	lot := x.d.Broker.LotSize(lots[0].Symbol)
	explained := ok && avg > 0 && qty >= total-lot/2
	for _, p := range lots {
		if explained {
			share := fee * p.Qty / qty
			x.closeManual(ctx, now, p, avg, share,
				fmt.Sprintf("콕핏 밖 매도 체결 %d건(평균가) — 사후 감지", n), res)
			continue
		}
		x.closeManual(ctx, now, p, 0, 0, "브로커 조회로 사후 감지 — 체결가·시각 미상", res)
	}
	x.d.Log.Warn("브로커에서 사라진 로트를 종결했다", "code", lots[0].Symbol.Code, "lots", len(lots),
		"attributed", explained)
}

// attributeShrink — 실물이 장부보다 적다. ★ 로트가 **하나뿐**이고 콕핏 밖 매도가 차이를 설명할 때만 그 로트를
// 줄인다. 로트가 여럿이면 누구 몫인지 모른다 — 추측하지 않고 보고만 한다 (reflex 무추측 규칙).
func (x *Executor) attributeShrink(ctx context.Context, now time.Time, p protocol.Position, held float64, res *Result) bool {
	diff := p.Qty - held
	lot := x.d.Broker.LotSize(p.Symbol)
	since := time.Time{}
	if p.EntryAt != nil {
		since = *p.EntryAt
	}
	qty, avg, fee, n, ok := x.foreignSells(ctx, p.Symbol, since)
	if !ok || qty < diff-lot/2 || qty > diff+lot/2 {
		return false
	}
	x.recordFill(ctx, p.Slot, p.Kid, p.Scope, store.Order{
		ID: ids.NewAt(now), IntentID: p.IntentID, Phase: "exit_filled", Symbol: p.Symbol, Side: "sell",
		Qty: diff, Price: avg, FeeKRW: fee, ExitReason: "manual", Source: store.SourceManual,
		RealizedPct: realizedPct(p.AvgEntryPrice, avg),
		Detail:      fmt.Sprintf("콕핏 밖 일부 매도 %d건 — 로트를 %v 로 줄임", n, held),
	}, res)
	if err := x.d.Store.ReduceIntent(ctx, p.IntentID, held); err != nil {
		res.fail("수동 일부 매도 반영 %s: %v", p.IntentID, err)
		return true
	}
	p.Qty = held
	x.d.Engine.UpsertPosition(p)
	res.PartialExits++
	return true
}
