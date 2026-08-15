// cockpitd — yovel cockpit 집행 데몬.
//
// ★ 아직 브로커도 릴레이도 없다. 있는 것: 설정 · 로컬 API(토큰·Host·Origin 가드) ·
// 다운링크 판정 · 계획 산출 · 버전 노출 · 깨끗한 종료.
// 빠진 것은 "그 계획을 실제로 내는 손"과 "목표를 실어 오는 transport" 다.
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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/config"
	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/httpapi"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/version"
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

	// ★ 브로커가 아직 없다 = 참조가를 모른다 = 진입 계획이 E_SYMBOL 로 거절된다.
	// 그게 맞는 동작이다. 가짜 가격을 채워 "계획이 나오는 것처럼" 보이게 두면,
	// 배선이 빠진 상태와 정상 상태가 같아 보인다.
	eng := engine.New(engine.Config{
		Mode:         cfg.Mode,
		Policy:       cfg.Policy,
		TargetMaxAge: cfg.TargetMaxAge,
		MaxOrders:    cfg.MaxOrdersPerTick,
		SlotCapital:  func(string) float64 { return 0 },
	}, started)

	srv := httpapi.New(httpapi.Options{
		Port:      cfg.Port,
		Token:     token,
		Mode:      cfg.Mode,
		StartedAt: started,
		Log:       log,
	}, eng)
	if err := srv.Start(); err != nil {
		return fmt.Errorf("로컬 API 기동 실패: %w", err)
	}

	log.Info("cockpitd 시작",
		"version", v.Version, "sha", v.SHA, "dirty", v.Dirty,
		"mode", cfg.Mode, "addr", srv.Addr(), "data_dir", cfg.DataDir,
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
