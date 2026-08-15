// cockpitd — yovel cockpit 집행 데몬.
//
// ★ 현재는 골격이다. 브로커 연결도, 릴레이 연결도, reconcile 루프도 아직 없다.
// 있는 것: 설정 · 로컬 API(토큰·Host·Origin 가드) · 버전 노출 · 깨끗한 종료.
//
// 골격을 먼저 세우는 이유는 순서 때문이다. 이 데몬이 나중에 실주문을 내므로,
// "누가 이 프로세스에 명령할 수 있는가" 를 기능보다 먼저 못박아야 한다.
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
	state := &stubState{cfg: cfg, started: started}

	srv := httpapi.New(httpapi.Options{
		Port:      cfg.Port,
		Token:     token,
		Mode:      cfg.Mode,
		StartedAt: started,
		Log:       log,
	}, state)
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

// stubState 는 아직 아무 데도 연결되지 않은 상태 제공자다.
//
// ★ 빈 스냅샷을 "포지션 없음" 으로 보이게 두면 안 된다 — 실제로는 "아직 아무것도 안 봤음" 이다.
// 그래서 guards.target_stale 을 true 로 둔다: 목표를 받은 적이 없으므로 진입 금지 상태가 맞다.
type stubState struct {
	cfg     config.Config
	started time.Time
}

func (s *stubState) Snapshot() protocol.StateSnapshot {
	v := version.Get()
	return protocol.StateSnapshot{
		AsOf: time.Now().UTC(),
		Daemon: protocol.DaemonInfo{
			Version:   v.Version,
			SHA:       v.SHA,
			StartedAt: &s.started,
		},
		Mode: s.cfg.Mode,
		Guards: protocol.Guards{
			Paused:      true, // 배선이 없으므로 아무것도 하지 않는다
			TargetStale: true,
		},
		Positions: []protocol.Position{},
		Orphans:   []protocol.Symbol{},
	}
}
