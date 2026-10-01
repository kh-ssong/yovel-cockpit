package executor

import (
	"context"
	"errors"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

const (
	// sellRetryAfterLock — 남의 주문에 잠긴 수량. 사람이 풀 때까지 자주 두드릴 이유가 없다.
	sellRetryAfterLock = time.Minute
	// sellRetryAfterUnknown — 실보유 조회 실패 · 그 밖의 매도 거부.
	sellRetryAfterUnknown = 15 * time.Second
)

// sellFailed — 매도가 거부됐다.
//
// ★ "매도가능 0주"(키움 800033) / insufficient_funds_ask(업비트)는 두 뜻이다 (reflex 교훈):
//   - **안 들고 있다** (이미 팔렸다 — 수동 매도·놓친 체결) → 계속 팔려 들면 무한 거부다 (reflex 5/22 091180).
//   - **들고 있는데 잠겼다** (콕핏이 모르는 지정가가 수량을 묶었다) → 그 주문을 콕핏이 취소하면 안 된다
//     (사람·다른 봇의 주문일 수 있다, reflex 8/12 형제 슬롯 TP). 알리고 기다린다.
//
// 그래서 실보유를 물어 셋으로 나눈다: 조회 실패 / 0 / N. 어느 경우든 매 틱 다시 내지 않는다.
func (x *Executor) sellFailed(ctx context.Context, now time.Time, pos protocol.Position, reason string,
	err error, res *Result) {
	if !errors.Is(err, broker.ErrNotEnoughShare) {
		x.sellAfter[pos.IntentID] = now.Add(sellRetryAfterUnknown)
		res.fail("매도 %s(%s): %v", pos.IntentID, pos.Symbol.Code, err)
		return
	}

	hs, qerr := x.d.Broker.Positions(ctx)
	if qerr != nil {
		// ★ 조회 실패를 "0주" 로 읽지 않는다 — 그러면 들고 있는 로트를 종결해 버린다.
		x.sellAfter[pos.IntentID] = now.Add(sellRetryAfterUnknown)
		res.fail("매도가능 0주 %s(%s) — 실보유 조회 실패, 잠시 뒤 다시: %v", pos.IntentID, pos.Symbol.Code, qerr)
		return
	}
	var held, sellable float64
	for _, h := range hs {
		if h.Symbol.Code == pos.Symbol.Code {
			held, sellable = h.Qty, h.Sellable
		}
	}
	lot := x.d.Broker.LotSize(pos.Symbol)

	if held <= lot/2 {
		// 이미 없다 — 콕핏 밖에서 팔렸다. 콕핏 밖 매도 체결가로 닫는다 (모르면 미상 — 지어내지 않는다).
		x.attributeVanished(ctx, now, []protocol.Position{pos}, res)
		delete(x.sellAfter, pos.IntentID)
		return
	}

	// 들고 있는데 못 판다 — 잠겼다. 자동으로 남의 주문을 취소하지 않는다.
	x.sellAfter[pos.IntentID] = now.Add(sellRetryAfterLock)
	res.fail("매도가능 부족 %s(%s): 보유 %v · 매도가능 %v · 로트 %v — 콕핏이 모르는 주문이 수량을 잠갔다. "+
		"HTS/앱에서 미체결 주문을 확인할 것 (자동 취소 안 함, 1분 뒤 다시)",
		pos.IntentID, pos.Symbol.Code, held, sellable, pos.Qty)
	_ = reason
}
