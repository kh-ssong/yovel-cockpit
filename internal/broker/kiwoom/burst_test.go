package kiwoom

// 몰림 대비 (몰림 재생 2026-10-15) — 조회 하나가 다른 API·주문을 붙잡지 않는다.

import (
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
)

// ★ 같은 API 의 간격 대기가 다른 API 를 붙잡지 않는다. 예전엔 락을 쥔 채 자서, 시세(ka10007)
// 대기열이 주문(kt10000)까지 막았다 — API 별 한도가 사실상 계좌 전체 한 줄이었다.
func TestPacerWaitDoesNotBlockOtherAPIs(t *testing.T) {
	var p pacer
	p.wait(apiQuote, time.Now, time.Sleep) // 자리만 잡는다
	done := make(chan struct{})
	go func() { p.wait(apiQuote, time.Now, time.Sleep); close(done) }() // ~250ms 기다린다
	time.Sleep(30 * time.Millisecond)

	t0 := time.Now()
	p.wait(apiBuy, time.Now, time.Sleep)
	if d := time.Since(t0); d > 100*time.Millisecond {
		t.Fatalf("주문이 시세 대기열 뒤에서 %v 기다렸다", d)
	}
	<-done
}

// 같은 API 를 여러 고루틴이 한꺼번에 불러도 간격은 지킨다 (자리를 차례로 잡는다).
func TestPacerConcurrentSameAPIKeepsGap(t *testing.T) {
	var p pacer
	var mu sync.Mutex
	var at []time.Time
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.wait(apiBalance, time.Now, time.Sleep)
			mu.Lock()
			at = append(at, time.Now())
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(at, func(i, j int) bool { return at[i].Before(at[j]) })
	if span := at[3].Sub(at[0]); span < 3*pacerGap-30*time.Millisecond {
		t.Fatalf("네 번이 %v 안에 나갔다 — 간격(%v)을 안 지켰다", span, pacerGap)
	}
}

// ★ 응답 없이 매달린 조회는 QueryTimeout 에 끊는다 — 집행 루프는 한 줄이라 그 뒤 주문이 전부 기다린다.
func TestHungQueryGivesUpAtQueryTimeout(t *testing.T) {
	f := newFake()
	release := make(chan struct{})
	f.on(apiBalance, func(map[string]any) any { <-release; return map[string]any{"return_code": 0} })
	b, _ := newBroker(t, f)
	defer close(release)
	b.cfg.QueryTimeout = 200 * time.Millisecond

	t0 := time.Now()
	if _, err := b.Cash(ctx); err == nil {
		t.Fatal("매달린 조회가 성공했다")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("매달린 조회를 %v 기다렸다 (상한 200ms)", d)
	}
}

// 주문은 QueryTimeout 으로 끊지 않는다 — 끊으면 "나갔는지 모름" 이 된다.
func TestOrderNotCutByQueryTimeout(t *testing.T) {
	f := newFake()
	f.on(apiBuy, func(map[string]any) any {
		time.Sleep(300 * time.Millisecond)
		return map[string]any{"return_code": 0, "ord_no": "0000123"}
	})
	b, _ := newBroker(t, f)
	b.cfg.QueryTimeout = 100 * time.Millisecond
	s, err := b.SubmitBuy(ctx, broker.OrderRequest{Symbol: sym, Qty: 3, RefPrice: 1000})
	if err != nil || s.OrderID != "0000123" {
		t.Fatalf("느린 주문을 끊었다: %+v %v", s, err)
	}
}

// 잔고가 주는 현재가를 같이 넘긴다 — 보유 종목마다 시세를 또 묻지 않게. 부호는 등락 표시다.
func TestPositionsCarryCurrentPrice(t *testing.T) {
	f := newFake()
	f.on(apiBalance, func(map[string]any) any {
		return map[string]any{"return_code": 0, "stk_cntr_remn": []any{
			map[string]any{"stk_cd": "A005930", "cur_qty": "000000000003", "buy_uv": "000000071000", "cur_prc": "-72100"},
		}}
	})
	b, _ := newBroker(t, f)
	hs, err := b.Positions(ctx)
	if err != nil || len(hs) != 1 || hs[0].Price != 72100 {
		t.Fatalf("%+v %v", hs, err)
	}
}

// ★ 증거금 부족(855056)으로 끝나면 «돈이 없다» 로 읽힌다 — 집행기가 E_CAPITAL 로 알리고 기다린다.
func TestMarginRejectZeroIsInsufficient(t *testing.T) {
	f := newFake()
	f.on(apiBuy, func(map[string]any) any {
		return map[string]any{"return_code": 20, "return_msg": "[2000](855056:매수증거금이 부족합니다. 0주 매수가능)"}
	})
	b, _ := newBroker(t, f)
	_, err := b.SubmitBuy(ctx, broker.OrderRequest{Symbol: sym, Qty: 3, RefPrice: 1000})
	if !errors.Is(err, broker.ErrInsufficient) || len(f.bodies(apiBuy)) != 1 {
		t.Fatalf("err=%v 주문 %d 번", err, len(f.bodies(apiBuy)))
	}
}
