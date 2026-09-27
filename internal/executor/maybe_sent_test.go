package executor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/paper"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// unknownBuyer — 매수 요청이 5xx 로 끝나 나갔는지 모른다.
type unknownBuyer struct {
	*paper.Broker
	buys int
}

func (b *unknownBuyer) Buy(context.Context, broker.OrderRequest) (broker.Fill, error) {
	b.buys++
	return broker.Fill{}, fmt.Errorf("kt10000: HTTP 502 — %w", broker.ErrMaybeSent)
}

// ★ 매수 결과를 모르는 목표로 다음 틱에 또 사면, 첫 주문이 나갔을 때 두 번 산 것이 된다.
func TestUnknownBuyIsNotRetriedNextTick(t *testing.T) {
	h := newHarness(t)
	ub := &unknownBuyer{Broker: h.br}
	h.rewire(ub)
	var alerts []string
	h.x.d.Notify = func(o store.Order, slot string) { alerts = append(alerts, o.Detail) }

	h.apply(t, h.target(t, 1, protocol.WantOpen, 900, 0))
	res := h.x.Tick(ctx, base.Add(time.Second))
	if len(res.Errors) == 0 {
		t.Fatalf("미상이 조용히 넘어갔다: %+v", res)
	}
	h.x.Tick(ctx, base.Add(2*time.Second))
	h.x.Tick(ctx, base.Add(3*time.Second))
	if ub.buys != 1 {
		t.Fatalf("★ 결과 미상 뒤 %d 번 매수했다", ub.buys)
	}
	if len(alerts) != 1 {
		t.Fatalf("사람에게 알리지 않았다: %v", alerts)
	}
	rows := h.ledger(t)
	if len(rows) != 1 || rows[0].Phase != "rejected" {
		t.Fatalf("원장 %+v", rows)
	}
}
