package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/alloc"
	"github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
)

// 자동 배분 (docs/allocation.md) — 몫은 기동 때와 매일 08:50 에 계산해 그날 고정한다.

// 계좌에서 맡길 돈 읽기 ("capital": "account").
const (
	capitalReadTries   = 3
	capitalReadTimeout = 10 * time.Second
	// bootAllocWait — 기동 때 계좌 읽기를 기다리는 한도. 로컬 API 가 이 뒤에 뜬다 (macbook.sh 는 40초 기다린다).
	// 못 읽으면 08:50 점검의 기동 회차(뒤에서)가 다시 해 본다.
	bootAllocWait = 12 * time.Second
	// capitalJump — 어제보다 이만큼 바뀌면 알린다 (입금·출금이면 정상, 아니면 계좌를 볼 일).
	capitalJump = 0.10
)

// 시험이 줄인다.
var (
	capitalReadPause = 2 * time.Second
	// capitalRetryEvery — 계좌를 못 읽고 마지막 값도 없을 때 다시 해 보는 간격. 그동안 몫이 없어 진입은 E_CAPITAL.
	capitalRetryEvery = 30 * time.Second
)

// allocator — 오늘 몫을 계산해 엔진에 건다. "capital": "account" 면 맡길 돈을 계좌에서 읽는다.
type allocator struct {
	eng   *engine.Engine
	books *book.Set
	br    broker.Broker
	// path — 마지막으로 읽은 맡길 돈 (data-dir/capital.json). 계좌 조회가 실패하면 이 값으로 그날 몫을 잡고,
	// 크게 바뀌었는지 이것과 비교한다.
	path string
	log  *slog.Logger

	mu       sync.Mutex // apply 를 한 번에 하나만 (기동·08:50·재시도가 겹칠 수 있다)
	alert    func(key, text string)
	pending  []string // alert 가 생기기 전(기동 초기)의 알림
	retrying atomic.Bool
}

// savedCapital — capital.json.
type savedCapital struct {
	Day     string    `json:"day"`
	Capital float64   `json:"capital"`
	Cash    float64   `json:"cash"` // 계좌 현금 한도 (L1)
	Held    float64   `json:"held"` // 콕핏 보유 원가
	At      time.Time `json:"at"`
}

// setAlert — 알림 경로가 생기면 기동 초기에 쌓인 알림을 보낸다.
func (a *allocator) setAlert(f func(key, text string)) {
	a.mu.Lock()
	a.alert = f
	pend := a.pending
	a.pending = nil
	a.mu.Unlock()
	for _, m := range pend {
		f("capital", m)
	}
}

func (a *allocator) notifyLocked(msg string) {
	if a.alert != nil {
		a.alert("capital", msg)
		return
	}
	a.pending = append(a.pending, msg)
}

// apply — 오늘 몫을 건다. 자동 배분이 꺼져 있으면 아무것도 안 한다.
// 날이 안 바뀌었으면 다시 계산하지 않는다 (장중 재계산 = 장중 크기 변경).
// false = 맡길 돈을 못 정했다 (계좌 조회 실패 + 마지막 값 없음) — 그 전 몫이 있으면 그대로 쓴다.
func (a *allocator) apply(ctx context.Context, now time.Time) bool {
	if !a.books.Alloc().On() {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cur := a.eng.AllocPlan()
	if cur != nil && cur.Day == alloc.Day(now) {
		return true
	}
	capital, from, err := a.capitalLocked(ctx, now)
	if err != nil {
		msg := fmt.Sprintf("💰 자동 배분: 맡길 돈(계좌)을 못 읽었다 — %v. ", err)
		if cur != nil {
			msg += fmt.Sprintf("%s 몫(맡긴 돈 %.0f원)을 그대로 쓴다", cur.Day, cur.Capital)
		} else {
			msg += "몫이 없어 진입은 E_CAPITAL 로 거절된다 — 30초마다 다시 읽는다"
		}
		a.log.Error(msg)
		a.notifyLocked(msg)
		return false
	}
	p := alloc.ComputeWith(a.books, capital, from, now)
	a.eng.SetAlloc(p)
	logPlan(a.log, p)
	return true
}

// retry — 맡길 돈을 못 정했으면 정할 때까지 다시 해 본다 (한 번에 하나).
func (a *allocator) retry(ctx context.Context) {
	if !a.retrying.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.retrying.Store(false)
		t := time.NewTicker(capitalRetryEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if a.apply(ctx, time.Now()) {
					return
				}
			}
		}
	}()
}

// capitalLocked — 오늘 맡길 돈과 출처.
//
// "account" 면 계좌 현금 한도(L1, Cash.Seed) + 콕핏 보유 원가. ★ 예수금(Deposit)이 아니다 — 예수금은
// 결제 전(D+2) 매수분을 이중으로 세고, 해외 원화배정처럼 빠져나간 돈을 못 보여준다 (budget.go).
// 콕핏 밖 보유(사람이 산 주식 등)는 콕핏이 팔아 쓸 돈이 아니라 안 센다.
func (a *allocator) capitalLocked(ctx context.Context, now time.Time) (float64, string, error) {
	al := a.books.Alloc()
	if !al.FromAccount {
		return al.Capital, alloc.FromBooks, nil
	}
	cash, err := a.readCash(ctx)
	if err != nil {
		s, ok := a.load()
		if !ok {
			return 0, "", err
		}
		msg := fmt.Sprintf("💰 자동 배분: 계좌 조회 실패(%v) — 마지막으로 읽은 맡길 돈 %.0f원(%s)으로 오늘 몫을 잡는다",
			err, s.Capital, s.At.In(session.KST).Format("01-02 15:04"))
		a.log.Warn(msg)
		a.notifyLocked(msg)
		return s.Capital, alloc.FromAccountSaved, nil
	}
	held := a.eng.HeldCost(now)
	capital := math.Floor((cash + held) * al.CapitalPct / 100)
	if al.CapitalMax > 0 && capital > al.CapitalMax {
		capital = al.CapitalMax
	}
	a.log.Info("자동 배분 — 맡길 돈을 계좌에서 읽음", "capital", capital, "cash", cash, "held", round0(held),
		"capital_pct", al.CapitalPct, "capital_max", al.CapitalMax)
	if prev, ok := a.load(); ok && prev.Capital > 0 && math.Abs(capital-prev.Capital) >= prev.Capital*capitalJump {
		msg := fmt.Sprintf("💰 자동 배분: 맡길 돈이 %.0f원 → %.0f원 (%+.0f%%) — 입금·출금이면 정상, 아니면 계좌를 확인할 것",
			prev.Capital, capital, (capital/prev.Capital-1)*100)
		a.log.Warn(msg)
		a.notifyLocked(msg)
	}
	a.save(savedCapital{Day: alloc.Day(now), Capital: capital, Cash: cash, Held: held, At: now.UTC()})
	return capital, alloc.FromAccount, nil
}

// readCash — 계좌 현금 한도 (몇 번 다시 해 본다 — 키움 조회는 가끔 시간이 넘는다).
func (a *allocator) readCash(ctx context.Context) (float64, error) {
	var last error
	for i := 0; i < capitalReadTries; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return 0, last // 기동은 짧게만 기다린다 (bootAllocWait) — 진짜 실패 사유를 남긴다
			case <-time.After(capitalReadPause):
			}
		}
		cctx, cancel := context.WithTimeout(ctx, capitalReadTimeout)
		c, err := a.br.Cash(cctx)
		cancel()
		switch {
		case err != nil:
			last = err
		case c.Seed <= 0:
			// 0 = 드라이버가 모른다 (broker.Cash). 0원으로 몫을 잡으면 하루 내내 아무것도 안 산다.
			last = fmt.Errorf("브로커가 현금 한도(L1)를 0 으로 줬다 (deposit %.0f · orderable %.0f)", c.Deposit, c.Orderable)
		default:
			return c.Seed, nil
		}
	}
	return 0, last
}

func (a *allocator) load() (savedCapital, bool) {
	var s savedCapital
	raw, err := os.ReadFile(a.path)
	if err != nil || json.Unmarshal(raw, &s) != nil || s.Capital <= 0 {
		return savedCapital{}, false
	}
	return s, true
}

// save — 원자적으로 쓴다 (쓰다 죽어도 반쪽 파일이 남지 않게).
func (a *allocator) save(s savedCapital) {
	raw, _ := json.MarshalIndent(s, "", "  ")
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		a.log.Warn("맡길 돈 기록 실패", "path", a.path, "err", err)
		return
	}
	if err := os.Rename(tmp, a.path); err != nil {
		a.log.Warn("맡길 돈 기록 실패", "path", a.path, "err", err)
	}
}

// logPlan — 오늘 몫을 기동 로그에 남긴다 (사람이 자기 설정이 어떻게 풀렸는지 눈으로 확인).
func logPlan(log *slog.Logger, p *alloc.Plan) {
	log.Info("자동 배분 — 오늘 몫 (하루 고정)", "day", p.Day, "capital", p.Capital, "capital_from", p.CapitalFrom,
		"night_cap", p.NightCap, "overnight_pct", p.OvernightPct, "borrow_x", p.BorrowX, "reserve_trades", p.ReserveTrades,
		"day_pct", round1(p.DayPct), "night_pct", round1(p.NightPct))
	for _, b := range p.Books {
		switch {
		case b.Fixed:
			log.Info("  고정 장부 (seed 대로)", "name", b.Name, "hold", b.Hold, "seed", b.Base)
		case !b.Enabled:
			log.Info("  꺼진 장부 (몫 없음)", "name", b.Name)
		case b.Pct > 0:
			log.Info("  장부 몫", "name", b.Name, "hold", b.Hold, "pct", b.Pct,
				"base", round0(b.Base), "cap", round0(b.Cap), "per_trade_at_w0.05", round0(b.Base*0.05))
		default:
			log.Info("  장부 몫", "name", b.Name, "hold", b.Hold, "share", b.Share,
				"base", round0(b.Base), "cap", round0(b.Cap), "per_trade_at_w0.05", round0(b.Base*0.05))
		}
	}
	if p.DayPct > 100.05 {
		log.Info(fmt.Sprintf("  낮 장부 몫 합이 맡긴 돈의 %.0f%% — 장부끼리 계좌를 선착순으로 나눠 쓴다 (맡긴 돈 층이 넘지 않게 막는다)", p.DayPct))
	}
}

func round0(v float64) float64 { return float64(int64(v + 0.5)) }

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// allocReportAt — 하루 요약 시각 (KST). 장 마감 정산·밤 보유가 다 잡힌 뒤.
const allocReportHour, allocReportMin = 15, 40

// watchAllocReport — 매일 15:40 에 자본 효율 요약을 로그로 남긴다 (텔레그램이 켜져 있으면 같이).
func watchAllocReport(ctx context.Context, eng *engine.Engine, log *slog.Logger, send func(string)) {
	for {
		now := time.Now().In(session.KST)
		next := time.Date(now.Year(), now.Month(), now.Day(), allocReportHour, allocReportMin, 0, 0, session.KST)
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		r := eng.AllocReport(time.Now())
		if r == nil {
			continue
		}
		msg := allocSummary(r)
		log.Info(msg)
		if send != nil {
			send("⚖️ " + msg)
		}
	}
}

// allocSummary — 사람이 읽는 한 줄 요약.
func allocSummary(r *engine.AllocReport) string {
	t := r.Today
	var b strings.Builder
	fmt.Fprintf(&b, "자본 효율 %s — 낮 가동률 평균 %.0f%% · 최대 %.0f%%", t.Day, t.DayUtilAvg*100, t.DayUtilMax*100)
	if t.NightUtil >= 0 {
		fmt.Fprintf(&b, " · 밤 %.0f%%", t.NightUtil*100)
	}
	miss := alloc.Totals(t.Missed)
	if len(miss) == 0 {
		b.WriteString(" · 돈 때문에 놓친 신호 0")
	} else {
		b.WriteString(" · 놓친 신호")
		for _, k := range alloc.SortedBooks(miss) {
			fmt.Fprintf(&b, " %s %d건(%.0f원)", k, miss[k].N, miss[k].KRW)
		}
	}
	if sh := alloc.Totals(t.Shrunk); len(sh) > 0 {
		b.WriteString(" · 줄여 산 것")
		for _, k := range alloc.SortedBooks(sh) {
			fmt.Fprintf(&b, " %s %d건(−%.0f원)", k, sh[k].N, sh[k].KRW)
		}
	}
	if len(t.HoldMismatch) > 0 {
		fmt.Fprintf(&b, " · ★ hold 어긋남 %v", t.HoldMismatch)
	}
	return b.String()
}
