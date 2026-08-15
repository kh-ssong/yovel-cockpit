// Package quotes 는 기준가를 한 곳에서 공급한다.
//
// ★ 사이징과 stop 평가가 **같은 가격**을 봐야 한다. 두 곳이 각자 조회하면 같은 틱 안에서도
// 다른 값을 보게 되고, 그러면 "왜 이 수량이 나왔는지" 를 사후에 재현할 수 없다.
//
// ★ 캐시 TTL 은 신선도 문제이기도 하다. 늙은 가격으로 stop 을 평가하면 안 되므로,
// 나이를 함께 돌려주고 판단은 호출자(guard)가 한다 — 여기서 조용히 "괜찮다" 고 하지 않는다.
package quotes

import (
	"context"
	"sync"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

type entry struct {
	price float64
	asOf  time.Time
}

type Source struct {
	mu    sync.Mutex
	br    broker.Broker
	ttl   time.Duration
	now   func() time.Time
	cache map[string]entry
}

func New(br broker.Broker, ttl time.Duration, now func() time.Time) *Source {
	if ttl <= 0 {
		ttl = 3 * time.Second
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Source{br: br, ttl: ttl, now: now, cache: map[string]entry{}}
}

func key(s protocol.Symbol) string { return s.Exchange + ":" + s.Code }

// Get 은 가격과 그 시각을 준다. 실패하면 ok=false —
// ★ 마지막으로 알던 값을 조용히 되돌려주지 않는다. 그건 "모른다" 를 "안다" 로 바꾸는 것이다.
func (s *Source) Get(ctx context.Context, sym protocol.Symbol) (float64, time.Time, bool) {
	s.mu.Lock()
	e, ok := s.cache[key(sym)]
	fresh := ok && s.now().Sub(e.asOf) < s.ttl
	s.mu.Unlock()

	if fresh {
		return e.price, e.asOf, true
	}

	q, err := s.br.Quote(ctx, sym)
	if err != nil || q.Price <= 0 {
		return 0, time.Time{}, false
	}

	s.mu.Lock()
	s.cache[key(sym)] = entry{price: q.Price, asOf: q.AsOf}
	s.mu.Unlock()
	return q.Price, q.AsOf, true
}

// Price 는 reconcile/engine 이 쓰는 형태 (시각 없이 값만).
func (s *Source) Price(sym protocol.Symbol) (float64, bool) {
	p, _, ok := s.Get(context.Background(), sym)
	return p, ok
}
