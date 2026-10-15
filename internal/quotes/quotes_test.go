package quotes

import (
	"context"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

var sym = protocol.Symbol{Exchange: "KRX", Code: "005930"}

// stub — Quote 만 답하는 브로커 (나머지는 부르면 터진다 — 부르면 안 된다).
type stub struct {
	broker.Broker
	price float64
	asOf  time.Time
	calls int
}

func (s *stub) Quote(context.Context, protocol.Symbol) (broker.Quote, error) {
	s.calls++
	return broker.Quote{Price: s.price, AsOf: s.asOf}, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// 잔고가 준 현재가가 바뀌면 캐시에 들어가고, 시세를 따로 묻지 않는다.
func TestPutFeedsCacheOnChange(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 15, 0, 1, 0, 0, time.UTC)}
	br := &stub{price: 900}
	s := New(br, 3*time.Second, c.now)

	s.Put(sym, 1000)
	if p, at, ok := s.Get(context.Background(), sym); !ok || p != 1000 || !at.Equal(c.t) || br.calls != 0 {
		t.Fatalf("p=%v at=%v ok=%v 조회 %d", p, at, ok, br.calls)
	}
}

// 값이 그대로면 시각은 두고 캐시만 데운다 — 보유 20 종목을 3초마다 따로 묻지 않는다.
func TestPutSamePriceKeepsWarmWithOldTime(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 15, 0, 1, 0, 0, time.UTC)}
	first := c.t
	br := &stub{price: 1000}
	s := New(br, 3*time.Second, c.now)

	s.Put(sym, 1000)
	c.t = c.t.Add(2 * time.Second)
	s.Put(sym, 1000)
	c.t = c.t.Add(2 * time.Second)
	if p, at, ok := s.Get(context.Background(), sym); !ok || p != 1000 || br.calls != 0 || !at.Equal(first) {
		t.Fatalf("p=%v at=%v 조회 %d — 데우지 않았거나 시각을 새로 붙였다", p, at, br.calls)
	}
}

// ★ 그대로인 값이 오래되면 데우지 않는다 — 거래정지 종목의 낡은 가격이 "지금" 으로 보이면 stop 이 그
// 가격으로 판정된다. 캐시가 식으면 거래소 시각이 붙은 시세를 다시 묻는다.
func TestPutSamePriceGoesColdWhenOld(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 15, 0, 1, 0, 0, time.UTC)}
	stale := c.t.Add(-10 * time.Minute)
	br := &stub{price: 1000, asOf: stale}
	s := New(br, 3*time.Second, c.now)

	s.Put(sym, 1000)
	c.t = c.t.Add(putKeepWarm + time.Second)
	s.Put(sym, 1000) // 그대로이고 늙었다 — 데우지 않는다
	p, at, ok := s.Get(context.Background(), sym)
	if !ok || p != 1000 || br.calls != 1 || !at.Equal(stale) {
		t.Fatalf("p=%v at=%v 조회 %d — 늙은 값을 데웠다", p, at, br.calls)
	}
}

// 신호가 검증 시세 — 엔진은 기억한 값만 보고(네트워크 없음), 너무 오래된 값은 모른다고 한다.
func TestRefCachedOnlyAfterFetch(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 15, 0, 1, 0, 0, time.UTC)}
	n := 0
	r := NewRef(func(protocol.Symbol) (float64, bool) { n++; return 286_500, true }, time.Minute, c.now)

	if _, ok := r.Cached(sym); ok || n != 0 {
		t.Fatalf("조회한 적 없는데 안다고 했다 (조회 %d)", n)
	}
	if p, ok := r.Fetch(sym); !ok || p != 286_500 {
		t.Fatal(p, ok)
	}
	if p, ok := r.Cached(sym); !ok || p != 286_500 || n != 1 {
		t.Fatalf("p=%v ok=%v 조회 %d", p, ok, n)
	}
	c.t = c.t.Add(61 * time.Second)
	if _, ok := r.Cached(sym); ok {
		t.Fatal("1분 넘은 값을 안다고 했다")
	}
}
