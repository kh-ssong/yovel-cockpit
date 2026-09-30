package executor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/paper"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// halfSeller — 첫 매도는 절반만 체결된다 (시장가 분할체결 중 타임아웃).
type halfSeller struct {
	*paper.Broker
	calls int
}

func (b *halfSeller) Sell(ctx context.Context, req broker.OrderRequest) (broker.Fill, error) {
	b.calls++
	if b.calls == 1 {
		req.Qty = float64(int64(req.Qty / 2))
		f, err := b.Broker.Sell(ctx, req)
		f.Partial = true
		return f, err
	}
	return b.Broker.Sell(ctx, req)
}

func (h *harness) rewire(br broker.Broker) {
	h.x = New(Deps{
		Broker: br, Store: h.st, Engine: h.eng, Mode: protocol.ModePaper, DaemonSHA: "abc1234",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// ★ 부분체결 청산을 종결로 처리하면 잔량이 원장 밖 유령이 된다 — 아무도 다시 팔지 않는다.
func TestPartialExitKeepsRemainderAndSellsItNextTick(t *testing.T) {
	h := newHarness(t)
	now := base.Add(time.Second)
	hs := &halfSeller{Broker: h.br}
	h.rewire(hs)

	h.apply(t, h.target(t, 1, protocol.WantOpen, 900, 0))
	if res := h.x.Tick(ctx, now); res.Entered != 1 {
		t.Fatalf("%+v", res)
	}
	held, _ := h.br.Positions(ctx)
	total := held[0].Qty

	h.apply(t, h.target(t, 2, protocol.WantFlat, 0, 0))
	res := h.x.Tick(ctx, now)
	if res.PartialExits != 1 || res.Exited != 0 {
		t.Fatalf("부분 청산을 종결로 읽었다: %+v", res)
	}
	open, _ := h.st.OpenIntents(ctx)
	if len(open) != 1 || open[0].Qty != total-float64(int64(total/2)) {
		t.Fatalf("잔량이 원장에 없다: %+v", open)
	}

	// 다음 틱 — 청산 사유(flat)가 살아 있으므로 나머지를 판다.
	res = h.x.Tick(ctx, now)
	if res.Exited != 1 {
		t.Fatalf("잔량을 안 팔았다: %+v", res)
	}
	if held, _ := h.br.Positions(ctx); len(held) != 0 {
		t.Fatalf("브로커에 남았다: %+v", held)
	}
	var sold float64
	partialNoted := false
	for _, o := range h.ledger(t) {
		if o.Phase == "exit_filled" {
			sold += o.Qty
			partialNoted = partialNoted || strings.Contains(o.Detail, "부분 체결")
		}
	}
	if sold != total || !partialNoted {
		t.Fatalf("원장 매도 합 %v (보유 %v), 부분 표시 %v", sold, total, partialNoted)
	}
}

// strictCancel — 실브로커처럼, 이미 취소된 주문을 또 취소하면 오류를 낸다.
// 그리고 첫 매도는 실패한다.
type strictCancel struct {
	*paper.Broker
	cancelled map[string]bool
	sells     int
}

func (b *strictCancel) CancelOrder(ctx context.Context, s protocol.Symbol, id string) error {
	if b.cancelled[id] {
		return errors.New("취소할 주문이 없습니다")
	}
	b.cancelled[id] = true
	return b.Broker.CancelOrder(ctx, s, id)
}

func (b *strictCancel) Sell(ctx context.Context, req broker.OrderRequest) (broker.Fill, error) {
	b.sells++
	if b.sells == 1 {
		return broker.Fill{}, errors.New("일시 오류")
	}
	return b.Broker.Sell(ctx, req)
}

// ★ TP 를 취소한 뒤 매도가 실패하면, 다음 틱이 죽은 TP 를 또 취소하려다 막혀 **영영 못 판다**.
func TestExitAfterFailedSellDoesNotRecancelDeadTP(t *testing.T) {
	h := newHarness(t)
	now := base.Add(time.Second)
	sc := &strictCancel{Broker: h.br, cancelled: map[string]bool{}}
	h.rewire(sc)

	h.apply(t, h.target(t, 1, protocol.WantOpen, 900, 1200))
	h.x.Tick(ctx, now)
	if res := h.x.Tick(ctx, now); res.TpPlaced != 1 {
		t.Fatalf("TP 위임 안 됨: %+v", res)
	}

	h.apply(t, h.target(t, 2, protocol.WantFlat, 0, 0))
	if res := h.x.Tick(ctx, now); res.Exited != 0 || len(res.Errors) == 0 {
		t.Fatalf("첫 매도는 실패해야 한다: %+v", res)
	}
	res := h.x.Tick(ctx, now)
	if res.Exited != 1 {
		t.Fatalf("★ 죽은 TP 취소에 막혀 청산을 못 했다: %+v", res)
	}
}
