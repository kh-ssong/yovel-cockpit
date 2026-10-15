package executor_test

// 몰림 재생 — 실측 신호 묶음(flat6 픽스처)을 **실시간으로** 가짜 키움 + 실제 집행 경로에 흘린다.
//
// cockpitd 배선을 그대로 옮긴다: 엔진(장부·주문 상한 5·신호가 검증은 캐시만) · 집행 루프(목표 도착
// 즉시 + 5초 틱 + 0.5초 진행 중 주문 확인 · 주문 직전 신호가 조회) · Desk 발행자(1초마다 전체 스냅샷 ·
// 2초마다 /v1/state = 스냅샷 + 계좌) · 키움 드라이버(pacer · kt00011 사전 축소 · 855056 재시도).
// 키움 왕복 지연은 HTTP 감싸개가 흉내 낸다 (추정 100ms). 가짜는 가격이 안 움직이고 즉시 체결된다.
//
// 세는 것: 산 것 / 계좌 축소 / 계좌 0주 / E_CAPITAL 만료 / E_RATE 만료 / 마감 지나 안 냄 / 줄 서다 만료.
//
// 픽스처 testdata/burst_20261015.json = 10/15 폭락일(CB 1단계) 갭다운 Desk 2개 신호 54건 (30초에 52건).
// 그날 고치기 전엔 시나리오 B(계좌 2,000만·장부 합 200%) 에서 돈의 79% 만 썼고, 한 틱이 35초,
// 목표 한 통 판정이 11초, 진입 마감(신호 + 10초)을 넘긴 주문이 신호 + 39초에 나갔다. 고친 뒤엔
// 계좌를 다 쓰고(99.99%) 최장 틱 2.4초 · 마감 넘긴 주문 0.
//
// BURST_FILE 이 없으면 건너뛴다 — 시나리오마다 실시간 2~3분이 걸린다 (병렬로 돈다).
//
//	BURST_FILE=testdata/burst_20261015.json [BURST_OUT=<dir>] [BURST_ONLY=A,B] \
//	  go test ./internal/executor -run TestBurstReplay -v -timeout 20m

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/kiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/executor"
	"github.com/kh-ssong/yovel-cockpit/internal/fakekiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/ids"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/quotes"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

type burstSignal struct {
	TMs      int64   `json:"t_ms"`
	Desk     string  `json:"desk"`
	Code     string  `json:"code"`
	RefPrice float64 `json:"ref_price"`
}

type burstScenario struct {
	Name    string
	Note    string
	Cash    float64
	Seeds   map[string]float64 // 장부 scope → seed (신호 없는 장부도 넣어 합 > 100% 를 만든다)
	Weight  float64
	Latency time.Duration // 키움 왕복 (추정)
	// Fault994 — 몰림 내내 kt00005 가 rc=-994 로 거부된다.
	Fault994 bool
	// HangFrom..HangTo (09:00 기준) 사이 kt00005 는 HangFor 동안 응답이 없다 (클라 타임아웃 15초).
	HangFrom, HangTo, HangFor time.Duration
	// NoRefCheck — 신호가 검증(ka10007 시세를 엔진 락 안에서 조회)을 끈다 — 원인 분리용 대조군.
	NoRefCheck bool
}

const (
	burstKid      = "flat6-1"
	burstTTL      = 10 * time.Second // flat6 apps/desk/exec.py ENTRY_TTL
	burstPublish  = time.Second      // flat6 relay_publish_interval
	burstStatePol = 2 * time.Second  // flat6 relay_state_interval
	burstLead     = 3 * time.Second  // 09:00 전에 미리 띄운다
)

var burstBlock = "거래정지,관리종목,정리매매,투자위험,투자경고,단기과열" // config 기본값

func TestBurstReplay(t *testing.T) {
	path := os.Getenv("BURST_FILE")
	if path == "" {
		t.Skip("BURST_FILE 없음 — 몰림 재생은 수동으로만 돈다")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Signals []burstSignal `json:"signals"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	sort.SliceStable(f.Signals, func(i, j int) bool { return f.Signals[i].TMs < f.Signals[j].TMs })

	pool := 941_382.0 // reflex 풀 총액 (픽스처 reflex_reference)
	scens := []burstScenario{
		{Name: "A", Note: "reflex 모양 — 계좌 94만 · 장부 main 60% mild 70% deep 50% brkout 1% (합 181%) · 1건 = seed×0.2",
			Cash: pool, Weight: 0.2, Latency: 100 * time.Millisecond,
			Seeds: map[string]float64{"kiwoom/gapdown_main": 0.60 * pool, "kiwoom/gapdown_mild": 0.70 * pool,
				"kiwoom/gapdown_deep": 0.50 * pool, "kiwoom/brkout": 0.01 * pool}},
		{Name: "B", Note: "flat6 U3 모양 — 계좌 2,000만 · 장부 main 2,000만 mild 2,000만 (합 200%) · 1건 = 100만",
			Cash: 20_000_000, Weight: 0.05, Latency: 100 * time.Millisecond,
			Seeds: map[string]float64{"kiwoom/gapdown_main": 20_000_000, "kiwoom/gapdown_mild": 20_000_000}},
		{Name: "C", Note: "B + 몰림 내내 kt00005 rc=-994",
			Cash: 20_000_000, Weight: 0.05, Latency: 100 * time.Millisecond, Fault994: true,
			Seeds: map[string]float64{"kiwoom/gapdown_main": 20_000_000, "kiwoom/gapdown_mild": 20_000_000}},
		{Name: "D", Note: "B + 09:00:05~09:00:35 kt00005 응답 없음 (20초 매달림 → 클라 15초 타임아웃)",
			Cash: 20_000_000, Weight: 0.05, Latency: 100 * time.Millisecond,
			HangFrom: 5 * time.Second, HangTo: 35 * time.Second, HangFor: 20 * time.Second,
			Seeds: map[string]float64{"kiwoom/gapdown_main": 20_000_000, "kiwoom/gapdown_mild": 20_000_000}},
		{Name: "E", Note: "B 대조군 — 신호가 검증 끔 (--ref-price-max-dev 0)",
			Cash: 20_000_000, Weight: 0.05, Latency: 100 * time.Millisecond, NoRefCheck: true,
			Seeds: map[string]float64{"kiwoom/gapdown_main": 20_000_000, "kiwoom/gapdown_mild": 20_000_000}},
		{Name: "F", Note: "A 대조군 — 신호가 검증 끔",
			Cash: pool, Weight: 0.2, Latency: 100 * time.Millisecond, NoRefCheck: true,
			Seeds: map[string]float64{"kiwoom/gapdown_main": 0.60 * pool, "kiwoom/gapdown_mild": 0.70 * pool,
				"kiwoom/gapdown_deep": 0.50 * pool, "kiwoom/brkout": 0.01 * pool}},
	}
	only := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("BURST_ONLY"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			only[s] = true
		}
	}
	for _, sc := range scens {
		if len(only) > 0 && !only[sc.Name] {
			continue
		}
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			t.Parallel()
			rep := runBurst(t, sc, f.Signals)
			t.Log("\n" + rep.text)
			if dir := os.Getenv("BURST_OUT"); dir != "" {
				b, _ := json.MarshalIndent(rep, "", " ")
				_ = os.WriteFile(filepath.Join(dir, "burst_"+sc.Name+".json"), b, 0o644)
				_ = os.WriteFile(filepath.Join(dir, "burst_"+sc.Name+".txt"), []byte(rep.text), 0o644)
			}
		})
	}
}

// ── 결과 ────────────────────────────────────────────────────────────────────

type ackEvent struct {
	AtMs   int64    `json:"at_ms"`
	Status string   `json:"status"`
	Codes  []string `json:"codes,omitempty"`
}

type intentRow struct {
	IntentID string     `json:"intent_id"`
	Desk     string     `json:"desk"`
	Code     string     `json:"code"`
	SignalMs int64      `json:"signal_ms"`
	Ref      float64    `json:"ref"`
	WantQty  float64    `json:"want_qty"`
	GotQty   float64    `json:"got_qty"`
	SubmitMs int64      `json:"submit_ms,omitempty"` // 09:00 기준
	Outcome  string     `json:"outcome"`
	Acks     []ackEvent `json:"acks"`
	Errors   []string   `json:"errors,omitempty"`
	Rejected string     `json:"rejected,omitempty"`
}

type burstReport struct {
	Scenario   burstScenario  `json:"scenario"`
	Counts     map[string]int `json:"counts"`
	Intents    []intentRow    `json:"intents"`
	Calls      map[string]int `json:"calls"`
	Ticks      int            `json:"ticks"`
	MaxTickMs  int64          `json:"max_tick_ms"`
	MaxApplyMs int64          `json:"max_apply_ms"`
	StateFails int            `json:"state_fails"`
	Expired    int            `json:"expired_skips"`
	Short      int            `json:"capital_short_notes"`
	CashLeft   float64        `json:"cash_left"`
	Spent      float64        `json:"spent"`
	text       string
}

// ── 실행 ────────────────────────────────────────────────────────────────────

// noteLog — 집행기 로그에서 «계좌 0주 대기» · «마감 지나 안 냄» 을 목표별로 센다 (이제 오류가 아니라 로그다).
type noteLog struct {
	mu          *sync.Mutex
	short, late map[string]int
}

func (h noteLog) Enabled(context.Context, slog.Level) bool { return true }
func (h noteLog) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h noteLog) WithGroup(string) slog.Handler            { return h }
func (h noteLog) Handle(_ context.Context, r slog.Record) error {
	iid := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "intent_id" {
			iid = a.Value.String()
		}
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case strings.HasPrefix(r.Message, "계좌 주문가능금액 부족"):
		h.short[iid]++
	case strings.HasPrefix(r.Message, "진입 마감이 지나"):
		h.late[iid]++
	}
	return nil
}

// hangWrap — 키움 왕복 지연 + kt00005 매달림을 흉내 내는 감싸개.
type hangWrap struct {
	h   http.Handler
	sc  burstScenario
	now func() time.Duration // 09:00 기준 경과
}

func (w hangWrap) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if w.sc.HangFor > 0 && r.Header.Get("api-id") == "kt00005" {
		if e := w.now(); e >= w.sc.HangFrom && e < w.sc.HangTo {
			select {
			case <-time.After(w.sc.HangFor):
			case <-r.Context().Done():
				return
			}
		}
	}
	if w.sc.Latency > 0 {
		time.Sleep(w.sc.Latency)
	}
	w.h.ServeHTTP(rw, r)
}

func runBurst(t *testing.T, sc burstScenario, sigs []burstSignal) burstReport {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-10-15 09:00:00", session.KST)
	base = base.UTC()
	start := time.Now().Add(burstLead) // start 시점 = 09:00:00
	now := func() time.Time { return base.Add(time.Since(start)).Truncate(time.Millisecond) }
	since := func() time.Duration { return time.Since(start) }
	ms := func(tt time.Time) int64 { return tt.Sub(base).Milliseconds() }

	// 가짜 키움 — 가격 = 신호가 (시장가가 그 값에 찬다).
	spec, err := fakekiwoom.LoadSpec("../fakekiwoom/testdata/spec_subset.json")
	if err != nil {
		t.Fatal(err)
	}
	prices := map[string]float64{}
	for _, s := range sigs {
		prices[s.Code] = s.RefPrice
	}
	fk := fakekiwoom.New(fakekiwoom.Config{Spec: spec, Cash: sc.Cash, Prices: prices, Now: now})
	if sc.Fault994 {
		fk.AddFault(fakekiwoom.Fault{API: "kt00005", ReturnCode: -994, Msg: "[fake] -994 조회 실패", Count: 100000})
	}
	srv := httptest.NewServer(hangWrap{h: fk, sc: sc, now: since})
	defer srv.Close()
	kb, err := kiwoom.New(kiwoom.Config{AppKey: "fake", SecretKey: "fake", DataDir: t.TempDir(), APIURL: srv.URL, Now: now})
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenPath(filepath.Join(t.TempDir(), "cockpit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pol := protocol.DefaultPolicy()
	pol.TrustedKeys = map[string]ed25519.PublicKey{burstKid: pub}

	var bks []book.Book
	for scope, seed := range sc.Seeds {
		bks = append(bks, book.Book{Name: strings.TrimPrefix(scope, "kiwoom/"), Kid: burstKid, Scope: scope, Seed: math.Round(seed)})
	}
	books, err := book.New(bks, 0)
	if err != nil {
		t.Fatal(err) // ★ 1번 점검 — 합 > 100% 로 기동이 거부되면 여기서 걸린다
	}

	qs := quotes.New(kb, 3*time.Second, now)
	ref := quotes.NewRef(qs.Price, time.Minute, now) // cockpitd 와 같다 — 엔진은 캐시만, 집행기가 채운다
	refDev := 0.15                                   // config 기본값
	if sc.NoRefCheck {
		refDev = 0
	}
	cal, _ := session.NewCalendar(session.DefaultKRXHolidays)
	eng := engine.New(engine.Config{
		Mode: protocol.ModeLive, Policy: pol, TargetMaxAge: 180 * time.Second, MaxOrders: 5,
		Books: books, RefCheckPrice: ref.Cached, RefMaxDev: refDev,
		CanEnter: func(s protocol.Symbol, at time.Time) bool { return cal.CanEnter(s.Exchange, at) },
		Price:    qs.Price,
		Market: func(s protocol.Symbol) sizing.Market {
			return sizing.Market{LotSize: kb.LotSize(s), MinOrderValue: kb.MinOrderValue(s)}
		},
		Store: st,
	}, now())
	if err := eng.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	block := map[string]bool{}
	for _, l := range strings.Split(burstBlock, ",") {
		block[l] = true
	}
	notes := noteLog{mu: &sync.Mutex{}, short: map[string]int{}, late: map[string]int{}}
	x := executor.New(executor.Deps{
		Broker: kb, Store: st, Engine: eng, Mode: protocol.ModeLive, DaemonSHA: "burst",
		Log:   slog.New(notes),
		Quote: qs.Get, MaxPriceAge: 3 * time.Minute,
		Session: cal, EntryFillTimeout: 60 * time.Second, ExitCutoff: "15:15",
		BrokerExchange: "KRX", BlockStatus: block,
		RefCheck: ref.Fetch, RefMaxDev: refDev, Observe: qs.Put,
	})

	// 기록
	var mu sync.Mutex
	rows := map[string]*intentRow{}
	var order []string
	var errs []string
	ticks, stateFails, expiredSkips, shortNotes := 0, 0, 0, 0
	var maxTick, maxApply time.Duration

	wake := make(chan struct{}, 1)
	var wg sync.WaitGroup

	// /v1/state 의 계좌 — cmd/cockpitd accountProvider 와 같은 모양 (3초 기억 · 잔고 현재가 우선).
	var accMu sync.Mutex
	var accAt time.Time
	account := func() bool {
		accMu.Lock()
		defer accMu.Unlock()
		if !accAt.IsZero() && time.Since(accAt) < 3*time.Second {
			return true
		}
		if _, err := kb.Cash(ctx); err != nil {
			return false
		}
		hs, err := kb.Positions(ctx)
		if err != nil {
			return false
		}
		for _, h := range hs {
			if h.Price <= 0 {
				qs.Price(h.Symbol)
			}
		}
		accAt = time.Now()
		return true
	}

	// 집행 루프 (cmd/cockpitd runLoop 과 같은 모양)
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(5 * time.Second)
		fast := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		defer fast.Stop()
		note := func(r executor.Result, at time.Time) {
			mu.Lock()
			defer mu.Unlock()
			for _, e := range r.Errors {
				errs = append(errs, fmt.Sprintf("%d|%s", ms(at), e))
			}
			expiredSkips += r.Expired
			shortNotes += r.CapitalShort
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-tk.C:
			case <-fast.C:
				if !x.HasWorking() {
					continue
				}
				at := now()
				note(x.PollWorking(ctx, at), at)
				continue
			}
			at := now()
			t0 := time.Now()
			r := x.Tick(ctx, at)
			d := time.Since(t0)
			note(r, at)
			mu.Lock()
			ticks++
			if d > maxTick {
				maxTick = d
			}
			mu.Unlock()
		}
	}()

	// Desk 발행자 — 신호가 오면 바로, 그 외엔 1초마다 전체 스냅샷. 2초마다 /v1/state.
	desks := map[string][]burstSignal{}
	for _, s := range sigs {
		desks[s.Desk] = append(desks[s.Desk], s)
	}
	end := time.Duration(sigs[len(sigs)-1].TMs)*time.Millisecond + burstTTL + 10*time.Second
	for desk, list := range desks {
		desk, list := desk, list
		wg.Add(2)
		go func() { // 발행
			defer wg.Done()
			var tgts []map[string]any
			var seq uint64
			next := 0
			lastPub := -time.Hour
			for {
				el := since()
				if el > end || ctx.Err() != nil {
					return
				}
				added := false
				for next < len(list) && time.Duration(list[next].TMs)*time.Millisecond <= el {
					s := list[next]
					sigAt := base.Add(time.Duration(s.TMs) * time.Millisecond)
					iid := ids.NewAt(sigAt)
					tgts = append(tgts, map[string]any{
						"intent_id": iid, "slot": strings.TrimPrefix(desk, "kiwoom/"), "side": "long", "want": "open",
						"weight": sc.Weight,
						"symbol": map[string]any{"exchange": "KRX", "code": s.Code},
						"entry":  map[string]any{"mode": "market", "not_after": sigAt.Add(burstTTL), "ref_price": s.RefPrice},
						"exit":   map[string]any{"stop_price": math.Floor(s.RefPrice * 0.85)},
					})
					seed := math.Round(sc.Seeds[desk])
					mu.Lock()
					rows[iid] = &intentRow{IntentID: iid, Desk: desk, Code: s.Code, SignalMs: s.TMs, Ref: s.RefPrice,
						WantQty: math.Floor(sc.Weight * seed / s.RefPrice)}
					order = append(order, iid)
					mu.Unlock()
					next++
					added = true
				}
				if len(tgts) > 0 && (added || el-lastPub >= burstPublish) {
					seq++
					at := now()
					env := map[string]any{
						"v": 1, "typ": "intent.target", "id": ids.NewAt(at), "acct": "acc_burst",
						"ts": at, "exp": at.Add(60 * time.Second), "seq": seq,
						"body": map[string]any{"as_of_bar": at, "book_state": "normal", "scope": desk, "targets": tgts},
					}
					b, _ := json.Marshal(env)
					signed, err := protocol.Sign(b, burstKid, priv)
					if err != nil {
						panic(err)
					}
					t0 := time.Now()
					ack := eng.Apply(signed, at)
					d := time.Since(t0)
					lastPub = el
					mu.Lock()
					if d > maxApply {
						maxApply = d
					}
					for _, a := range ack.PerIntent {
						r := rows[a.IntentID]
						if r == nil {
							continue
						}
						ev := ackEvent{AtMs: ms(at), Status: a.Status}
						for _, c := range a.Codes {
							ev.Codes = append(ev.Codes, string(c))
						}
						if n := len(r.Acks); n == 0 || r.Acks[n-1].Status != ev.Status ||
							strings.Join(r.Acks[n-1].Codes, ",") != strings.Join(ev.Codes, ",") {
							r.Acks = append(r.Acks, ev)
						}
					}
					mu.Unlock()
					select {
					case wake <- struct{}{}:
					default:
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
		go func() { // /v1/state — 스냅샷 + 계좌 (accountProvider 와 같은 호출)
			defer wg.Done()
			for {
				if since() > end || ctx.Err() != nil {
					return
				}
				_ = eng.SnapshotAt(now())
				if !account() {
					mu.Lock()
					stateFails++
					mu.Unlock()
				}
				time.Sleep(burstStatePol)
			}
		}()
	}

	time.Sleep(time.Until(start.Add(end)))
	cancel()
	wg.Wait()

	// ── 집계 ──
	ledger, err := st.Ledger(context.Background(), store.LedgerQuery{Mode: protocol.ModeLive, Limit: 100000})
	if err != nil {
		t.Fatal(err)
	}
	rep := burstReport{Scenario: sc, Counts: map[string]int{}, Ticks: ticks,
		MaxTickMs: maxTick.Milliseconds(), MaxApplyMs: maxApply.Milliseconds(), StateFails: stateFails,
		Expired: expiredSkips, Short: shortNotes}
	for _, o := range ledger {
		r := rows[o.IntentID]
		if r == nil {
			continue
		}
		switch o.Phase {
		case "filled":
			r.GotQty += o.Qty
			rep.Spent += o.Qty * o.Price
			if o.SubmittedAt != nil && (r.SubmitMs == 0 || ms(*o.SubmittedAt) < r.SubmitMs) {
				r.SubmitMs = ms(*o.SubmittedAt)
			}
		case "rejected":
			r.Rejected = o.Detail
		}
	}
	for _, e := range errs {
		for _, iid := range order {
			if strings.Contains(e, iid) {
				rows[iid].Errors = append(rows[iid].Errors, e)
			}
		}
	}
	for _, iid := range order {
		r := rows[iid]
		lastLive := ""
		for _, a := range r.Acks {
			if a.AtMs <= r.SignalMs+burstTTL.Milliseconds() && !(len(a.Codes) == 1 && a.Codes[0] == "E_EXPIRED") {
				lastLive = a.Status + ":" + strings.Join(a.Codes, ",")
			}
		}
		zero := notes.short[iid] > 0
		late := notes.late[iid] > 0
		for _, e := range r.Errors {
			if strings.Contains(e, "매수가능수량 0") || strings.Contains(e, "855056") || strings.Contains(e, "주문가능금액 부족") {
				zero = true
			}
		}
		switch {
		case r.GotQty > 0 && r.GotQty >= r.WantQty:
			r.Outcome = "산 것(전량)"
		case r.GotQty > 0:
			r.Outcome = "계좌 축소"
		case zero:
			r.Outcome = "계좌 0주"
		case late:
			r.Outcome = "마감 지나 안 냄"
		case r.Rejected != "":
			r.Outcome = "기타 거절"
		case strings.Contains(lastLive, "E_CAPITAL"):
			r.Outcome = "E_CAPITAL 만료"
		case strings.Contains(lastLive, "E_RATE"):
			r.Outcome = "E_RATE 만료"
		case strings.HasPrefix(lastLive, "applied"):
			r.Outcome = "줄 서다 만료"
		default:
			r.Outcome = "기타(" + lastLive + ")"
		}
		rep.Counts[r.Outcome]++
		rep.Intents = append(rep.Intents, *r)
	}
	fs := fk.State()
	rep.Calls, rep.CashLeft = fs.Calls, fs.Cash

	// ── 글 ──
	var b strings.Builder
	fmt.Fprintf(&b, "■ %s — %s\n", sc.Name, sc.Note)
	keys := make([]string, 0, len(rep.Counts))
	for k := range rep.Counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-14s %2d\n", k, rep.Counts[k])
	}
	fmt.Fprintf(&b, "  쓴 돈 %.0f / 계좌 %.0f (남은 예수금 %.0f) · 틱 %d · 최장 틱 %dms · 최장 발행 판정 %dms · /v1/state 실패 %d\n",
		rep.Spent, sc.Cash, rep.CashLeft, rep.Ticks, rep.MaxTickMs, rep.MaxApplyMs, rep.StateFails)
	fmt.Fprintf(&b, "  마감 지나 안 낸 진입 %d · 계좌 0주 대기 표시 %d\n", rep.Expired, rep.Short)
	ck := make([]string, 0, len(rep.Calls))
	for k := range rep.Calls {
		ck = append(ck, k)
	}
	sort.Strings(ck)
	b.WriteString("  키움 호출:")
	for _, k := range ck {
		fmt.Fprintf(&b, " %s=%d", k, rep.Calls[k])
	}
	b.WriteString("\n  #  t(ms)  desk  code    want  got  submit-지연  결과 · 마지막 판정\n")
	for i, r := range rep.Intents {
		lag := "-"
		if r.SubmitMs > 0 {
			lag = fmt.Sprintf("%.1fs", float64(r.SubmitMs-r.SignalMs)/1000)
		}
		last := ""
		if n := len(r.Acks); n > 0 {
			var parts []string
			for _, a := range r.Acks {
				parts = append(parts, fmt.Sprintf("%+.1fs %s%s", float64(a.AtMs-r.SignalMs)/1000, a.Status,
					map[bool]string{true: "{" + strings.Join(a.Codes, ",") + "}", false: ""}[len(a.Codes) > 0]))
			}
			last = strings.Join(parts, " → ")
		}
		fmt.Fprintf(&b, "  %2d %6d  %-4s  %-6s %5.0f %4.0f  %-8s  %s · %s\n", i+1, r.SignalMs,
			strings.TrimPrefix(r.Desk, "kiwoom/gapdown_"), r.Code, r.WantQty, r.GotQty, lag, r.Outcome, last)
		for _, e := range r.Errors {
			if len(e) > 160 {
				e = e[:160]
			}
			fmt.Fprintf(&b, "        ! %s\n", e)
		}
	}
	rep.text = b.String()
	return rep
}
