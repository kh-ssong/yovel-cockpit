package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// acctBroker — 계좌 값 공급자 시험용. Cash·Positions 만 쓴다 (나머지를 부르면 nil 인터페이스라 터진다).
type acctBroker struct {
	broker.Broker
	mu        sync.Mutex
	cashCalls int
	posCalls  int
	block     chan struct{} // 두 번째 Cash 부터 이게 닫힐 때까지 막힌다 (느린 키움 흉내)
}

func (b *acctBroker) Cash(ctx context.Context) (broker.Cash, error) {
	b.mu.Lock()
	b.cashCalls++
	n := b.cashCalls
	b.mu.Unlock()
	if n > 1 && b.block != nil {
		<-b.block
	}
	return broker.Cash{Deposit: float64(1000 * n), Orderable: 500, Currency: "KRW"}, nil
}

func (b *acctBroker) Positions(ctx context.Context) ([]broker.Holding, error) {
	b.mu.Lock()
	b.posCalls++
	b.mu.Unlock()
	return []broker.Holding{{Symbol: protocol.Symbol{Exchange: "KRX", Code: "069500"}, Qty: 3, Price: 100}}, nil
}

func (b *acctBroker) counts() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cashCalls, b.posCalls
}

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func noPrice(protocol.Symbol) (float64, bool) { return 0, false }

// ★ 캐시가 늙어도 요청은 브로커를 기다리지 않는다 — 직전 값을 바로 주고 뒤에서 한 번만 갱신한다
// (2026-10-20: 만료 직후 요청이 ~300ms 붙잡혀 발행자의 목표가 늦게 나갔다).
func TestAccountProviderServesStaleWithoutBlocking(t *testing.T) {
	br := &acctBroker{block: make(chan struct{})}
	p := newAccountProvider(br, noPrice, nil, quietLog, 20*time.Millisecond)
	ctx := context.Background()

	first := p(ctx)
	if first == nil || first.Deposit != 1000 {
		t.Fatalf("첫 조회 %+v", first)
	}
	time.Sleep(30 * time.Millisecond) // 늙힌다

	start := time.Now()
	a := p(ctx) // 갱신은 뒤에서 막혀 있다
	b := p(ctx) // 갱신 중엔 또 띄우지 않는다
	if waited := time.Since(start); waited > 15*time.Millisecond {
		t.Fatalf("늙은 캐시인데 %v 기다렸다", waited)
	}
	if a == nil || b == nil || a.Deposit != 1000 || b.Deposit != 1000 {
		t.Fatalf("직전 값을 줘야 한다 a=%+v b=%+v", a, b)
	}
	waitFor(t, func() bool { c, _ := br.counts(); return c == 2 }) // 뒤 갱신이 떴다 (막혀 있다)
	time.Sleep(30 * time.Millisecond)                              // 갱신이 막힌 채 또 늙는다
	if c := p(ctx); c == nil || c.Deposit != 1000 {
		t.Fatalf("갱신이 막혀도 직전 값 %+v", c)
	}
	if cash, _ := br.counts(); cash != 2 {
		t.Fatalf("갱신은 한 번에 하나여야 한다 (Cash %d번)", cash)
	}

	close(br.block)
	waitFor(t, func() bool { c := p(ctx); return c != nil && c.Deposit == 2000 })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("1초 안에 조건이 안 됐다")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ★ 보유는 집행기가 잔고 대조로 받아 둔 것을 쓴다 — 새것이면 브로커에 또 묻지 않는다.
func TestAccountProviderUsesFreshHeld(t *testing.T) {
	br := &acctBroker{}
	held := func() ([]broker.Holding, time.Time) {
		return []broker.Holding{{Symbol: protocol.Symbol{Exchange: "KRX", Code: "069500"}, Qty: 2, Price: 50}}, time.Now()
	}
	acc := newAccountProvider(br, noPrice, held, quietLog, time.Minute)(context.Background())
	if acc == nil || acc.Holdings != 100 {
		t.Fatalf("집행기 보유(2주 × 50)로 채워야 한다: %+v", acc)
	}
	if _, pos := br.counts(); pos != 0 {
		t.Fatalf("새 보유가 있는데 브로커에 %d번 물었다", pos)
	}

	// 늙은 보유면 직접 묻는다.
	stale := func() ([]broker.Holding, time.Time) { return nil, time.Now().Add(-time.Hour) }
	acc = newAccountProvider(br, noPrice, stale, quietLog, time.Minute)(context.Background())
	if acc == nil || acc.Holdings != 300 {
		t.Fatalf("늙었으면 브로커 보유(3주 × 100): %+v", acc)
	}
	if _, pos := br.counts(); pos != 1 {
		t.Fatalf("브로커 보유 조회 %d번 (1번이어야)", pos)
	}
}
