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
	asOf  time.Time // 시세 자체의 시각 (마지막 체결)
	got   time.Time // 우리가 조회한 시각 — 캐시 신선도는 이걸로 본다
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
	// ★ 캐시는 "언제 물어봤나" 로 판단한다. 시세 시각(asOf)으로 판단하면 거래가 뜸한 종목은
	// 캐시가 영영 안 맞아 매번 다시 조회한다. 시세가 늙었는지는 asOf 를 받은 호출자(guard)가 판단한다.
	fresh := ok && s.now().Sub(e.got) < s.ttl
	s.mu.Unlock()

	if fresh {
		return e.price, e.asOf, true
	}

	q, err := s.br.Quote(ctx, sym)
	if err != nil || q.Price <= 0 {
		return 0, time.Time{}, false
	}

	s.mu.Lock()
	s.cache[key(sym)] = entry{price: q.Price, asOf: q.AsOf, got: s.now()}
	s.mu.Unlock()
	return q.Price, q.AsOf, true
}

// Price 는 reconcile/engine 이 쓰는 형태 (시각 없이 값만).
func (s *Source) Price(sym protocol.Symbol) (float64, bool) {
	p, _, ok := s.Get(context.Background(), sym)
	return p, ok
}

// Put — 다른 조회가 같이 준 현재가를 캐시에 넣는다 (키움 잔고의 보유 종목 cur_prc).
//
// ★ 보유 종목마다 시세를 따로 물으면 20 종목만 들어도 키움 한도(API 당 ~4/s)가 시세 조회로 차고,
// 진입 주문이 그 뒤에 줄을 선다 (몰림 재생 2026-10-15). 잔고는 어차피 매 틱 부른다.
//
// ★ 값이 바뀌었을 때만 새 시각을 붙인다. 잔고는 체결 시각을 안 주므로 "받은 시각" 을 시세 시각으로
// 쓰는데, 값이 그대로인 종목(거래정지 등)에 매번 새 시각을 붙이면 낡은 가격이 "지금" 으로 보이고 stop
// 이 그 가격으로 판정된다 (TestQuoteAsOfFromExchange 와 같은 병).
// 값이 그대로면 시각은 두고 캐시만 데운다 — 단 시각이 putKeepWarm 보다 늙었으면 데우지 않아 캐시가
// 식고, 다음 Get 이 거래소 시각이 붙은 시세를 다시 묻는다 (같은 값에 계속 체결되는 종목은 거기서 젊어진다).
func (s *Source) Put(sym protocol.Symbol, price float64) {
	if price <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.now()
	if e, ok := s.cache[key(sym)]; ok && e.price == price {
		if t.Sub(e.asOf) < putKeepWarm {
			e.got = t
			s.cache[key(sym)] = e
		}
		return
	}
	s.cache[key(sym)] = entry{price: price, asOf: t, got: t}
}

// putKeepWarm — 값이 그대로인 종목을 잔고만으로 데워 두는 한계. 넘으면 거래소 시세를 다시 묻는다.
const putKeepWarm = time.Minute

// Ref — 신호가(entry.ref_price) 검증용 독립 시세.
//
// ★ 엔진은 Cached 만 부른다. 엔진 락 안에서 네트워크를 타면 목표 한 통의 판정이 시세 조회 대기열
// 뒤에 줄을 서고, 그동안 집행 루프·상태 조회가 전부 멈춘다 (몰림 재생 2026-10-15: 판정 11초,
// 틱 27~35초). 값은 락 밖의 집행기가 주문 직전에 Fetch 로 채우고, 같은 검사를 거기서 한 번 더 한다.
type Ref struct {
	fetch  func(protocol.Symbol) (float64, bool)
	maxAge time.Duration
	now    func() time.Time

	mu sync.Mutex
	m  map[string]refEntry
}

type refEntry struct {
	price float64
	at    time.Time
}

// NewRef — maxAge 보다 오래된 값은 Cached 가 모른다고 한다. ±15% 같은 거친 검사라 1분이면 충분하다.
func NewRef(fetch func(protocol.Symbol) (float64, bool), maxAge time.Duration, now func() time.Time) *Ref {
	if maxAge <= 0 {
		maxAge = time.Minute
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Ref{fetch: fetch, maxAge: maxAge, now: now, m: map[string]refEntry{}}
}

// Fetch — 조회하고 기억한다 (네트워크 — 락 밖에서만).
func (r *Ref) Fetch(sym protocol.Symbol) (float64, bool) {
	if r == nil || r.fetch == nil {
		return 0, false
	}
	p, ok := r.fetch(sym)
	if !ok || p <= 0 {
		return 0, false
	}
	r.mu.Lock()
	r.m[key(sym)] = refEntry{price: p, at: r.now()}
	r.mu.Unlock()
	return p, true
}

// Cached — 기억한 값만 (네트워크 없음). 모르면 ok=false — 검사를 건너뛴다(신호가의 존재 이유).
func (r *Ref) Cached(sym protocol.Symbol) (float64, bool) {
	if r == nil {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[key(sym)]
	if !ok || r.now().Sub(e.at) > r.maxAge {
		return 0, false
	}
	return e.price, true
}
