// cockpitd — yovel cockpit 집행 데몬.
//
// 있는 것: 설정 · 로컬 API(토큰·Host·Origin 가드) · 다운링크 판정 · 계획 산출 ·
// 브로커 집행(paper/kiwoom) · 로컬 원장 · 대시보드 · 버전 노출 · 깨끗한 종료.
// ★ 아직 없는 것은 "목표를 실어 오는 transport"(릴레이) 하나다.
//
// 이 순서인 이유: 이 데몬이 나중에 실주문을 내므로 "누가 이 프로세스에 명령할 수 있는가" 를
// 기능보다 먼저 못박아야 한다. 그리고 계약은 transport 없이도 루프백으로 완결시킬 수 있다
// (POST /v1/downlink) — 그렇게 하면 MQTT 배선 버그와 계약 버그가 섞이지 않는다.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/kiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/paper"
	"github.com/kh-ssong/yovel-cockpit/internal/config"
	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/executor"
	"github.com/kh-ssong/yovel-cockpit/internal/httpapi"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/quotes"
	"github.com/kh-ssong/yovel-cockpit/internal/sizing"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
	"github.com/kh-ssong/yovel-cockpit/internal/version"
	"github.com/kh-ssong/yovel-cockpit/internal/webui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cockpitd:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Default()
	fs := flag.NewFlagSet("cockpitd", flag.ExitOnError)
	cfg.Bind(fs)
	showVersion := fs.Bool("version", false, "버전을 찍고 종료")
	debugLog := fs.Bool("debug", false, "DEBUG 로그")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	v := version.Get()
	if *showVersion {
		fmt.Printf("cockpitd %s (%s)\n", v.Version, v.SHA)
		return nil
	}
	if err := cfg.Finish(); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *debugLog {
		level = slog.LevelDebug
	}
	// 구조화 stdout 로그 — 서비스로 상주할 때 로그 수집이 파싱할 수 있어야 한다.
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	token, err := httpapi.LoadOrCreateToken(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("로컬 API 토큰: %w", err)
	}

	started := time.Now()

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("로컬 저장소: %w", err)
	}
	defer st.Close()

	br, err := buildBroker(cfg, log)
	if err != nil {
		return err
	}
	// ★ 시세원이 없으면 참조가를 모르고, 그러면 진입 계획이 E_SYMBOL 로 거절된다.
	// 그게 맞는 동작이다 — 가짜 가격을 채워 "계획이 나오는 것처럼" 보이게 두면
	// 배선이 빠진 상태와 정상 상태가 같아 보인다.
	qs := quotes.New(br, 3*time.Second, func() time.Time { return time.Now().UTC() })

	eng := engine.New(engine.Config{
		Mode:         cfg.Mode,
		Policy:       cfg.Policy,
		TargetMaxAge: cfg.TargetMaxAge,
		MaxOrders:    cfg.MaxOrdersPerTick,
		// ★ 자본은 사용자가 정한다. 엔진은 비중만 보낸다 (슬롯 사이 분배 = weight).
		EngineBudget: cfg.EngineBudget,
		Price:        qs.Price,
		Market: func(s protocol.Symbol) sizing.Market {
			return sizing.Market{LotSize: br.LotSize(s), MinOrderValue: br.MinOrderValue(s)}
		},
		Store: st,
	}, started)

	// ★ 재시작 복구를 건너뛰면 걸어둔 pause 가 풀리고, 이미 끝난 목표로 재진입한다.
	if err := eng.Restore(context.Background()); err != nil {
		return fmt.Errorf("상태 복구 실패: %w", err)
	}

	// wake — 목표가 도착하면 다음 틱을 기다리지 않고 즉시 집행한다.
	// 버퍼 1 + 논블로킹 송신이라 연달아 와도 쌓이지 않는다 (한 번 돌면 최신 목표가 반영된다).
	wake := make(chan struct{}, 1)

	// ★ 토큰을 페이지에 실어 준다. 브라우저의 최초 내비게이션에는 Authorization 헤더를 붙일
	// 수단이 없어서다 — 자격을 푼 게 아니라 전달 방법이 다른 것이다 (webui.Boot 주석 참조).
	var ui http.Handler
	if cfg.UI {
		ui = webui.Handler(webui.Boot{Token: token, Mode: string(cfg.Mode)})
	}

	srv := httpapi.New(httpapi.Options{
		Port:      cfg.Port,
		Token:     token,
		Mode:      cfg.Mode,
		StartedAt: started,
		Log:       log,
		Wake: func() {
			select {
			case wake <- struct{}{}:
			default:
			}
		},
		UI:      ui,
		Account: accountProvider(br, qs.Price, log),
	}, eng)
	if err := srv.Start(); err != nil {
		return fmt.Errorf("로컬 API 기동 실패: %w", err)
	}

	snap := eng.Snapshot()
	log.Info("cockpitd 시작",
		"version", v.Version, "sha", v.SHA, "dirty", v.Dirty,
		"mode", cfg.Mode, "addr", srv.Addr(), "data_dir", cfg.DataDir,
		"restored_positions", len(snap.Positions), "paused", snap.Guards.Paused,
		"broker", br.Name(), "engine_budget", cfg.EngineBudget,
		"trusted_keys", len(cfg.Policy.TrustedKeys),
		"accept_unsigned_derisk", cfg.Policy.AcceptUnsignedDerisk,
	)

	// ★ 조용히 아무것도 안 하는 상태를 조용히 두지 않는다.
	// 신뢰키가 없으면 서명된 진입 지시를 하나도 검증할 수 없다 = 이 데몬은 영원히 대기만 한다.
	if len(cfg.Policy.TrustedKeys) == 0 {
		log.Warn("신뢰키가 없다 — 서명된 목표를 하나도 받아들일 수 없다",
			"path", config.TrustedKeysPath(cfg.DataDir))
	}
	if cfg.Mode == protocol.ModeLive {
		log.Warn("live 모드 — 실주문이 나갈 수 있다")
	}
	// ★ 계정 바인딩이 없으면 "누구에게 온 목표인가" 를 안 본다. 혼자 루프백으로 굴릴 땐 무해하지만,
	// 릴레이나 여러 사용자가 끼는 순간 남의 (진짜 서명된) 목표가 이 계좌에서 집행될 수 있다.
	// 조용히 무방비인 상태를 조용히 두지 않는다.
	if cfg.Policy.Acct == "" {
		log.Warn("계정 바인딩 없음 — 어느 acct 로 온 목표든 받아들인다 (--acct 로 고정할 것)")
	} else {
		log.Info("계정 바인딩", "acct", cfg.Policy.Acct)
	}
	logCostModel(context.Background(), cfg, st, log)
	switch {
	case !cfg.UI:
	case webui.Built():
		// ★ 토큰은 안 찍는다 — 로그는 수집·전송될 수 있고, 주소만 있으면 페이지가 알아서 받아 간다.
		log.Info("대시보드", "url", fmt.Sprintf("http://127.0.0.1:%d/", cfg.Port))
	default:
		// 화면이 안 뜨는 이유를 데몬이 먼저 말한다. 브라우저에서 빈 화면을 만나고 나서
		// 원인을 찾게 두면, 그건 배선 문제인지 빌드 문제인지 구분이 안 된다.
		log.Warn("대시보드 산출물이 이 바이너리에 없다 — ui/ 에서 npm ci && npm run build 후 scripts/build.sh",
			"url", fmt.Sprintf("http://127.0.0.1:%d/", cfg.Port))
	}
	// ★ 복구된 pause 를 조용히 두면, 사용자는 봇이 도는 줄 알고 기다린다.
	if snap.Guards.Paused {
		log.Warn("이전 세션의 de-risk 가 아직 걸려 있다 — resume 전까지 신규 진입 없음")
	}

	exec := executor.New(executor.Deps{
		Broker: br, Store: st, Engine: eng, Mode: cfg.Mode, DaemonSHA: v.SHA, Log: log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go runLoop(ctx, exec, cfg.ReconcileInterval, wake, log)

	<-ctx.Done()

	log.Info("종료 신호 수신, 정리 중")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("로컬 API 종료 실패", "err", err)
	}
	log.Info("cockpitd 종료", "uptime_sec", int64(time.Since(started).Seconds()))
	return nil
}

// buildBroker — mode 와 broker 설정에 따라 드라이버를 고른다.
//
// ★ paper 브로커라도 시세는 진짜를 쓰는 게 낫다. 키움 자격증명이 있으면 시세만 키움에서
// 받아 페이퍼로 체결시킨다 — 가짜 가격으로 만든 페이퍼 성과는 아무것도 증명하지 못한다.
func buildBroker(cfg config.Config, log *slog.Logger) (broker.Broker, error) {
	appKey, secret := config.KiwoomCreds()

	if cfg.Broker == "kiwoom" {
		if appKey == "" || secret == "" {
			return nil, fmt.Errorf("키움 자격증명이 없다 — COCKPIT_KIWOOM_APPKEY / COCKPIT_KIWOOM_SECRET 환경변수로 줄 것 (★ 플래그로 주면 ps 에 노출된다)")
		}
		return kiwoom.New(kiwoom.Config{
			AppKey: appKey, SecretKey: secret, DataDir: cfg.DataDir, Mock: cfg.KiwoomMock,
			TokenFile: cfg.KiwoomTokenFile,
		})
	}

	// 편도 비용. ★ 0 으로 두지 않는다 — 비용 0 시뮬은 손익분기 근처 전략의 판정을 뒤집는다.
	// ★★ 값은 이제 설정이다(옛 하드코딩 대칭 15bp 는 매수에 없는 비용을 물렸다).
	//    그리고 기본값도 추정치라, 진짜 요율은 `--paper-fee-bp-*` 로 **자기 원장에서 재서** 넣는다.
	pcfg := paper.Config{
		Cash: cfg.EngineBudget, Lot: 1,
		FeeBpBuy:  cfg.PaperFeeBpBuy,
		FeeBpSell: cfg.PaperFeeBpSell,
		SlipBp:    cfg.PaperSlipBp,
	}
	if appKey != "" && secret != "" {
		kw, err := kiwoom.New(kiwoom.Config{
			AppKey: appKey, SecretKey: secret, DataDir: cfg.DataDir, Mock: cfg.KiwoomMock,
			TokenFile: cfg.KiwoomTokenFile,
		})
		if err == nil {
			log.Info("paper 브로커에 키움 실시세를 물린다")
			src := quotes.New(kw, 3*time.Second, func() time.Time { return time.Now().UTC() })
			pcfg.Price = src.Price
		} else {
			log.Warn("키움 시세원 연결 실패 — 가격 없이 돈다 (진입 계획은 E_SYMBOL 로 거절된다)", "err", err)
		}
	} else {
		// ★ 옛 문구("진입 계획은 항상 E_SYMBOL 로 거절된다")는 **사실이 아니었다** —
		//   엔진이 `entry.ref_price` 를 실은 목표는 시세 없이도 진입시킨다(실측 2026-08-16).
		//   실제로 죽는 건 진입이 아니라 **청산**이다. 틀린 경고는 없는 경고보다 나쁘다.
		log.Warn("시세원이 없다 — ref_price 로 진입은 되지만 **TP·스톱이 평가되지 않는다** " +
			"(시간청산만 남는다). 키움 자격증명을 주면 paper 도 실시세로 돈다")
	}
	return paper.New(pcfg), nil
}

// runLoop — 집행 루프. ★ 목표 수신과 무관하게 주기적으로 돈다.
// 목표가 안 와도 브로커 실상태는 바뀔 수 있고(TP 체결·수동 매도), 그걸 못 보면 장부가 썩는다.
func runLoop(ctx context.Context, exec *executor.Executor, interval time.Duration,
	wake <-chan struct{}, log *slog.Logger) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-wake: // 목표 도착 — 틱을 기다리지 않는다
		case <-t.C:
		}

		res := exec.Tick(ctx, time.Now().UTC())

		// 아무 일도 없었으면 조용히 넘긴다 — 5초마다 로그를 찍으면 진짜 사건이 묻힌다.
		if res.Entered+res.Exited+res.StopsArmed+res.TpPlaced+res.ClosedByBroker == 0 &&
			len(res.Errors) == 0 && len(res.Mismatch) == 0 {
			continue
		}
		log.Info("집행", "entered", res.Entered, "exited", res.Exited,
			"stops", res.StopsArmed, "tp", res.TpPlaced, "closed_by_broker", res.ClosedByBroker,
			"mismatch", res.Mismatch, "errors", res.Errors)
	}
}

// logCostModel — paper 채점에 쓰는 **비용 전제**를 기동 때마다 찍고, 라이브 원장에서
// 관측된 실제 요율과 나란히 보여준다.
//
// ★★ 왜 로그에 박나 — paper 원장의 손익은 사람이 읽는 숫자다. 그 숫자가 어떤 비용 가정
// 위에서 나왔는지 화면 어디에도 없으면, **틀린 가정이 조용히 결론이 된다**
// (잘못 채워진 측정값은 빈 값보다 나쁘다 — 채워져 있으니 아무도 다시 안 본다).
//
// ★★ 수수료율은 **사용자마다 다르다** (비대면 개설 이벤트·등급 우대·증권사 정책).
// 그래서 "좋은 기본값" 이라는 건 없고, 정답은 자기 원장에서 재는 것이다. 라이브 체결이
// 쌓이는 순간 관측치가 나오고, 설정과 어긋나면 **그 어긋남이 바로 보인다.**
func logCostModel(ctx context.Context, cfg config.Config, st *store.Store, log *slog.Logger) {
	log.Info("paper 비용 모델 (편도 bp, ★ 추정치)",
		"fee_buy_bp", cfg.PaperFeeBpBuy,
		"fee_sell_bp", cfg.PaperFeeBpSell,
		"slip_bp", cfg.PaperSlipBp)

	obs, err := st.ObservedCost(ctx)
	if err != nil {
		log.Warn("라이브 요율 관측 실패", "err", err)
		return
	}
	if !obs.HasBuy() && !obs.HasSell() {
		// ★ "관측 없음" 을 "0bp" 로 뭉개지 않는다 — 수수료가 공짜인 것처럼 보인다.
		log.Info("라이브 체결이 없어 실제 요율은 아직 관측되지 않았다 " +
			"(체결이 한 건이라도 생기면 여기 찍힌다 → 그 값으로 --paper-fee-bp-* 를 맞출 것)")
		return
	}
	log.Info("라이브 원장에서 관측된 실제 요율 (편도 bp)",
		"fee_buy_bp", obs.BuyBp, "buy_fills", obs.BuyFills,
		"fee_sell_bp", obs.SellBp, "sell_fills", obs.SellFills)

	const tol = 2.0 // bp
	if obs.HasBuy() && math.Abs(obs.BuyBp-cfg.PaperFeeBpBuy) > tol {
		log.Warn("★ 매수 요율이 설정과 다르다 — paper 손익이 실제와 갈린다",
			"설정", cfg.PaperFeeBpBuy, "관측", obs.BuyBp)
	}
	if obs.HasSell() && math.Abs(obs.SellBp-cfg.PaperFeeBpSell) > tol {
		log.Warn("★ 매도 요율이 설정과 다르다 — paper 손익이 실제와 갈린다",
			"설정", cfg.PaperFeeBpSell, "관측", obs.SellBp)
	}
}


// accountProvider — 「계좌가 불어나는지 줄어드는지」를 `/v1/state` 에 싣는다.
//
// ★ 실패를 0 으로 채우지 않는다. 잔고 조회가 실패했는데 0 을 내면 사용자는 파산한 줄 알고,
// 화면은 "정상적으로 0원" 과 구분되지 않는다 — 빈 값이 낫다 (nil = account 필드 자체가 없음).
//
// ★ 평가금액에 현재가를 못 구하면 **평단으로 대신 채우고 그 사실을 센다**(`stale_holdings`).
// 그러면 Equity 가 실제와 다른데, 그걸 조용히 두면 손익 그래프가 조용히 틀린다.
func accountProvider(br broker.Broker, price func(protocol.Symbol) (float64, bool),
	log *slog.Logger) func(context.Context) *protocol.Account {
	return func(ctx context.Context) *protocol.Account {
		cash, err := br.Cash(ctx)
		if err != nil {
			log.Warn("잔고 조회 실패 — account 를 싣지 않는다", "err", err)
			return nil
		}
		holdings, err := br.Positions(ctx)
		if err != nil {
			log.Warn("보유 조회 실패 — account 를 싣지 않는다", "err", err)
			return nil
		}
		acc := &protocol.Account{
			Deposit: cash.Deposit, Orderable: cash.Orderable, Currency: cash.Currency,
		}
		for _, h := range holdings {
			px, ok := price(h.Symbol)
			if !ok || px <= 0 {
				px = h.AvgPrice // 모르는 것 ≠ 없는 것 — 평단으로라도 값을 매긴다
				acc.StaleHoldings++
			}
			acc.Holdings += px * h.Qty
		}
		acc.Equity = acc.Deposit + acc.Holdings
		return acc
	}
}
