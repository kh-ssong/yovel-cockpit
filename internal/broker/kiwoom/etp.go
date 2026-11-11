package kiwoom

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// ETF/ETN 판정 — 키움 호가로 안다.
//
// ★ 2026-11-11 발견: Config.IsETP 를 아무도 채우지 않아 069500 (ETF · 호가단위 5원) 의 지정가가
//   주식 표 (5만~20만 100원) 로 깎였다. 매수 IOC 는 100원 아래로 내려가 매도1 에 못 닿았고
//   (11/05~11 sweep IOC 0 체결 31건이 전부 이것), 매도 IOC·TP 는 100원 위로 올라갔다.
//   주식 단위는 ETP 단위의 배수라 **거부가 안 나서** 조용히 손해만 났다 (tick.go 의 옛 사고와 같은 모양).
//
// 판정 규칙: 최우선 호가에 **주식 호가단위로는 나올 수 없는 가격**이 보이면 ETP 다.
//   - 주식 호가는 언제나 주식 단위 위에 있다 → 주식을 ETP 로 볼 일은 없다 (그 틀림은 주문 거부라 위험한 쪽).
//   - ETP 인데 아직 못 알아봤으면 예전처럼 주식 단위로 깎는다 — 거부는 안 나고 덜 유리할 뿐이다.
//   - 두 표가 같은 가격대 (5,000원 이하) 는 판정이 필요 없다.
//   - 한 번 ETP 로 본 종목은 데몬이 도는 동안 ETP 다. 호가단위는 장중에 바뀌지 않는다.

// etpRetry — 호가 조회가 실패한 종목은 이만큼 다시 묻지 않는다 (지정가마다 조회가 매달리지 않게).
const etpRetry = time.Minute

type etpCache struct {
	mu      sync.Mutex
	yes     map[string]bool      // 주식 단위 밖 호가를 본 종목
	checked map[string]bool      // 호가를 한 번이라도 읽은 종목
	failAt  map[string]time.Time // 마지막 조회 실패
}

func newETPCache() etpCache {
	return etpCache{yes: map[string]bool{}, checked: map[string]bool{}, failAt: map[string]time.Time{}}
}

// offStockGrid — 주식이라면 나올 수 없는 가격인가.
// 경계값에서 코드가 아래 대역 단위를 쓰는데, 그 단위는 위 대역 단위의 약수라 주식 호가는 여전히 격자 위다.
func offStockGrid(p float64) bool {
	if p <= 0 {
		return false
	}
	return math.Mod(p, TickSize(p, false)) != 0
}

// noteBook — 읽은 호가를 판정에 보탠다 (Book 이 부른다).
func (b *Broker) noteBook(code string, bk broker.Book) {
	c := &b.etps
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checked[code] = true
	delete(c.failAt, code)
	if offStockGrid(bk.Ask1) || offStockGrid(bk.Bid1) {
		c.yes[code] = true
	}
}

func (b *Broker) etpState(code string) (yes, checked bool, failAt time.Time) {
	c := &b.etps
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.yes[code], c.checked[code], c.failAt[code]
}

func (b *Broker) etpFailed(code string) {
	c := &b.etps
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failAt[code] = b.now()
}

// etp — 이 가격의 지정가를 ETP 호가단위로 깎을지.
// 두 표가 갈리는 가격대인데 그 종목 호가를 아직 한 번도 못 봤으면 한 번 읽어 본다 (sweep 은 이미 읽었다).
func (b *Broker) etp(ctx context.Context, s protocol.Symbol, price float64) bool {
	if b.cfg.IsETP != nil && b.cfg.IsETP(s) {
		return true
	}
	if TickSize(price, false) == TickSize(price, true) {
		return false
	}
	yes, checked, failAt := b.etpState(s.Code)
	if yes || checked {
		return yes
	}
	if !failAt.IsZero() && b.now().Sub(failAt) < etpRetry {
		return false
	}
	if _, err := b.Book(ctx, s); err != nil {
		b.etpFailed(s.Code)
		return false
	}
	yes, _, _ = b.etpState(s.Code)
	return yes
}
