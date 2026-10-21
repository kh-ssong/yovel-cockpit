package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/alloc"
	"github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
)

// cashBroker — 맡길 돈 읽기 시험용. Cash 만 쓴다.
type cashBroker struct {
	broker.Broker
	mu    sync.Mutex
	calls int
	seed  float64
	err   error
}

func (b *cashBroker) Cash(context.Context) (broker.Cash, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.err != nil {
		return broker.Cash{}, b.err
	}
	// 예수금은 일부러 다르게 — 맡길 돈은 현금 한도(Seed)로 잡아야 한다.
	return broker.Cash{Deposit: b.seed + 437_186, Orderable: b.seed - 90_000, Seed: b.seed, Currency: "KRW"}, nil
}

func (b *cashBroker) set(seed float64, err error) {
	b.mu.Lock()
	b.seed, b.err = seed, err
	b.mu.Unlock()
}

func (b *cashBroker) n() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func loadBooks(t *testing.T, raw string) *book.Set {
	t.Helper()
	p := filepath.Join(t.TempDir(), "books.json")
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := book.Load(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const accountBooks = `{"capital": "account", "books":[
  {"name":"gap","kid":"flat6-1","scope":"kiwoom/gapdown_main","hold":"day","pct":25},
  {"name":"s60","kid":"flat6-1","scope":"kiwoom/swing_60m","hold":"carry","pct":23}]}`

type alcHarness struct {
	a      *allocator
	eng    *engine.Engine
	br     *cashBroker
	alerts []string
}

func newAlc(t *testing.T, raw string, seed float64) *alcHarness {
	t.Helper()
	capitalReadPause = 0
	set := loadBooks(t, raw)
	h := &alcHarness{br: &cashBroker{seed: seed}}
	h.eng = engine.New(engine.Config{Books: set}, time.Now())
	h.a = &allocator{eng: h.eng, books: set, br: h.br, path: filepath.Join(t.TempDir(), "capital.json"), log: quietLog}
	return h
}

func (h *alcHarness) collect() {
	h.a.setAlert(func(_, text string) { h.alerts = append(h.alerts, text) })
}

func kstAt(day, hm string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04", day+" "+hm, session.KST)
	return t
}

// ★ 맡길 돈 = 계좌 현금 한도 + 콕핏 보유 원가 (예수금 아님) — 그날 아침 «맡긴 돈 − 보유» 가 곧 현금이 된다.
func TestAllocatorReadsCapitalFromAccount(t *testing.T) {
	h := newAlc(t, accountBooks, 1_818_464)
	h.eng.SetPositions([]protocol.Position{{IntentID: "01J9Z8QK3M7X2ABCDEFGHJKMA0", Kid: "flat6-1", Scope: "kiwoom/swing_60m",
		Symbol: protocol.Symbol{Exchange: "KRX", Code: "475050"}, Qty: 10, AvgEntryPrice: 4487}})
	now := kstAt("2026-10-22", "08:50")
	if !h.a.apply(context.Background(), now) {
		t.Fatal("apply 실패")
	}
	p := h.eng.AllocPlan()
	if p == nil || p.Capital != 1_863_334 || p.CapitalFrom != alloc.FromAccount || p.Day != "2026-10-22" {
		t.Fatalf("plan %+v", p)
	}
	if g, _ := p.Book("gap"); !near(g.Base, 1_863_334*0.25) {
		t.Fatalf("gap 몫 %+v", g)
	}
	saved, ok := h.a.load()
	if !ok || saved.Capital != 1_863_334 || saved.Cash != 1_818_464 || saved.Held != 44_870 {
		t.Fatalf("기록 %+v", saved)
	}
	// 같은 날 다시 불러도 계좌를 또 묻지 않는다 (하루 고정).
	h.a.apply(context.Background(), now.Add(3*time.Hour))
	if h.br.n() != 1 {
		t.Fatalf("같은 날 계좌 조회 %d번", h.br.n())
	}
}

func TestAllocatorCapitalPctAndMax(t *testing.T) {
	h := newAlc(t, `{"capital": "account", "capital_pct": 50, "books":[{"name":"d","scope":"a","hold":"day","pct":10}]}`, 1_000_000)
	h.a.apply(context.Background(), kstAt("2026-10-22", "08:50"))
	if p := h.eng.AllocPlan(); p == nil || p.Capital != 500_000 {
		t.Fatalf("capital_pct 50 → 50만: %+v", p)
	}
	h = newAlc(t, `{"capital": "account", "capital_max": 400000, "books":[{"name":"d","scope":"a","hold":"day","pct":10}]}`, 1_000_000)
	h.a.apply(context.Background(), kstAt("2026-10-22", "08:50"))
	if p := h.eng.AllocPlan(); p == nil || p.Capital != 400_000 {
		t.Fatalf("capital_max 40만: %+v", p)
	}
}

// 계좌 조회가 실패하면 마지막으로 읽은 값으로 그날 몫을 잡고 알린다.
func TestAllocatorFallsBackToSaved(t *testing.T) {
	h := newAlc(t, accountBooks, 0)
	h.a.save(savedCapital{Day: "2026-10-21", Capital: 1_500_000, At: kstAt("2026-10-21", "08:50")})
	h.br.set(0, errors.New("context deadline exceeded"))
	if !h.a.apply(context.Background(), kstAt("2026-10-22", "08:50")) {
		t.Fatal("마지막 값이 있으면 몫을 잡는다")
	}
	if p := h.eng.AllocPlan(); p == nil || p.Capital != 1_500_000 || p.CapitalFrom != alloc.FromAccountSaved {
		t.Fatalf("plan %+v", p)
	}
	if h.br.n() != capitalReadTries {
		t.Fatalf("조회 %d번 (다시 해 봐야 한다)", h.br.n())
	}
	h.collect()
	if len(h.alerts) != 1 || !strings.Contains(h.alerts[0], "1500000") {
		t.Fatalf("기동 초기 알림이 늦게라도 가야 한다: %v", h.alerts)
	}
}

// 마지막 값도 없으면 몫을 안 건다 (진입은 E_CAPITAL) — 0원 몫으로 하루를 버리지 않게 다시 해 본다.
func TestAllocatorNoCapitalRetries(t *testing.T) {
	h := newAlc(t, accountBooks, 0)
	h.collect()
	h.br.set(0, errors.New("timeout"))
	now := kstAt("2026-10-22", "08:50")
	if h.a.apply(context.Background(), now) || h.eng.AllocPlan() != nil {
		t.Fatal("못 읽었는데 몫을 걸었다")
	}
	if len(h.alerts) != 1 || !strings.Contains(h.alerts[0], "E_CAPITAL") {
		t.Fatalf("알림 %v", h.alerts)
	}
	// 현금 한도 0 = 드라이버가 모른다 → 실패로 본다.
	h.br.set(0, nil)
	if h.a.apply(context.Background(), now) {
		t.Fatal("현금 한도 0 으로 몫을 잡았다")
	}
	// 계좌가 돌아오면 재시도가 몫을 건다.
	capitalRetryEvery = 10 * time.Millisecond
	defer func() { capitalRetryEvery = 30 * time.Second }()
	h.br.set(1_000_000, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.a.retry(ctx)
	waitFor(t, func() bool { return h.eng.AllocPlan() != nil })
	if p := h.eng.AllocPlan(); p.Capital != 1_000_000 {
		t.Fatalf("plan %+v", p)
	}
}

// 어제보다 10% 넘게 바뀌면 알린다 (입금이면 정상).
func TestAllocatorAlertsOnJump(t *testing.T) {
	h := newAlc(t, accountBooks, 1_860_000)
	h.collect()
	h.a.save(savedCapital{Day: "2026-10-21", Capital: 1_460_000})
	h.a.apply(context.Background(), kstAt("2026-10-22", "08:50"))
	if len(h.alerts) != 1 || !strings.Contains(h.alerts[0], "+27%") {
		t.Fatalf("알림 %v", h.alerts)
	}
	// 다음 날 조금만 바뀌면 조용하다.
	h.br.set(1_900_000, nil)
	h.a.apply(context.Background(), kstAt("2026-10-23", "08:50"))
	if len(h.alerts) != 1 {
		t.Fatalf("2%% 에 울렸다: %v", h.alerts)
	}
}

// 숫자 capital 은 계좌를 묻지 않는다 (옛 방식 그대로).
func TestAllocatorFixedCapitalSkipsBroker(t *testing.T) {
	h := newAlc(t, `{"capital": 1460000, "books":[{"name":"d","scope":"a","hold":"day"}]}`, 9_999_999)
	h.a.apply(context.Background(), kstAt("2026-10-22", "08:50"))
	if p := h.eng.AllocPlan(); p == nil || p.Capital != 1_460_000 || p.CapitalFrom != alloc.FromBooks || h.br.n() != 0 {
		t.Fatalf("plan %+v calls %d", p, h.br.n())
	}
}

// 계좌에서 만든 맡길 돈은 예산 점검을 소수점 오차로 넘지 않는다.
func TestBudgetToleratesAccountCapitalRounding(t *testing.T) {
	if ok, msg := budgetVerdict(1_863_334.0000001, 44_870, broker.Cash{Seed: 1_818_464}, false); !ok {
		t.Fatal(msg)
	}
}

func near(a, b float64) bool { return a-b < 1e-6 && b-a < 1e-6 }
