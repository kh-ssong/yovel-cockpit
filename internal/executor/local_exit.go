package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/guard"
)

// enforceLocalExits — 시간청산과 로컬 stop 을 판정하고, 걸린 포지션을 시장가로 판다.
//
// ★ 판단(언제 팔지)은 엔진 몫이다. 이 층은 **엔진이 없을 때**를 위한 것이다 — 신호원이 죽거나
// 끊긴 날에도 15:20 에 닫히고, 마지막으로 받은 stop 이 지켜진다. 엔진이 살아 있으면 엔진이
// 먼저 `want=flat` 을 보내므로 여기가 나설 일이 없다.
//
// ★ 시간청산은 시세 없이 성립한다(시계만 필요). stop 은 신선한 시세가 있을 때만 판정하고,
// 없으면 팔지 않고 Blind 로 올린다 — 피드 글리치 한 번에 전 포지션을 시장가로 털지 않는다.
func (x *Executor) enforceLocalExits(ctx context.Context, now time.Time, res *Result) map[string]bool {
	sold := map[string]bool{}
	for _, pos := range x.d.Engine.Positions() {
		rules := guard.Rules{StopPrice: pos.StopArmed, TimeExitAt: pos.TimeExitAt}
		in := guard.Inputs{Now: now, MaxPriceAge: x.d.MaxPriceAge}

		timeDue := rules.TimeExitAt != nil && !now.Before(*rules.TimeExitAt)
		if !timeDue && rules.StopPrice > 0 && x.d.Quote != nil {
			if p, at, ok := x.d.Quote(ctx, pos.Symbol); ok {
				in.LastPrice, in.PriceAsOf = p, at
			} else {
				in.PriceAsOf = time.Time{} // 모른다 — 아래에서 Blind 가 된다
			}
		}
		if !timeDue && rules.StopPrice > 0 && x.d.Quote == nil {
			continue // 시세원 자체가 없다 — stop 은 이 데몬이 지킬 수 없는 규칙이다
		}

		d := guard.Evaluate(rules, in)
		if rules.StopPrice > 0 && !timeDue && (d.Blind || in.LastPrice <= 0) {
			res.Blind = append(res.Blind, fmt.Sprintf("%s(%s)", pos.Symbol.Code, pos.IntentID))
		}
		if !d.Exit {
			continue
		}
		x.d.Log.Warn("로컬 청산 발동", "reason", d.Reason, "intent_id", pos.IntentID,
			"code", pos.Symbol.Code, "price", in.LastPrice, "stop", rules.StopPrice)
		x.doExit(ctx, now, pos, string(d.Reason), res)
		sold[pos.IntentID] = true
	}
	return sold
}
