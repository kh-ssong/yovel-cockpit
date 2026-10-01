package main

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/config"
	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/executor"
	"github.com/kh-ssong/yovel-cockpit/internal/proc"
	"github.com/kh-ssong/yovel-cockpit/internal/version"
)

// liveness — 집행 루프가 마지막으로 돈 시각과 진행 중 주문 수. 루프 고루틴이 쓰고 하트비트 고루틴이 읽는다.
type liveness struct {
	lastTick atomic.Int64 // unix ms
	working  atomic.Int64
}

func (l *liveness) mark(x *executor.Executor) {
	l.lastTick.Store(time.Now().UnixMilli())
	l.working.Store(int64(x.WorkingCount()))
}

// beatLoop — 30초마다 하트비트를 쓴다. 감시자(cockpitsup)는 last_tick 이 멈추면 "살아 있는데 멈췄다" 로 본다.
// ★ 하트비트를 집행 루프와 **다른 고루틴**에서 쓴다 — 같은 곳에서 쓰면 루프가 멈출 때 하트비트도 멈춰
// 구분이 안 되지만, 여기선 ts 는 계속 가고 last_tick 만 멈춘다 (reflex 2026-08-18: 같은 맥락의 감지기는 같이 죽는다).
func beatLoop(ctx context.Context, path string, live *liveness, eng *engine.Engine, v version.Info,
	cfg config.Config, brokerName string, log *slog.Logger) {
	write := func() {
		b := proc.Beat{
			TS: time.Now().UTC(), PID: os.Getpid(), Version: v.Version, SHA: v.SHA,
			Mode: string(cfg.Mode), Broker: brokerName,
			OpenLots: len(eng.Positions()), WorkingOrders: int(live.working.Load()),
		}
		if ms := live.lastTick.Load(); ms > 0 {
			b.LastTick = time.UnixMilli(ms).UTC()
		}
		if err := proc.WriteBeat(path, b); err != nil {
			log.Warn("하트비트 쓰기 실패", "err", err)
		}
	}
	write()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			write()
		}
	}
}

// accountLockKey — 같은 브로커 키(=계좌)로 두 콕핏이 뜨지 않게. paper 는 계좌가 없다.
func accountLockKey(cfg config.Config) string {
	switch cfg.Broker {
	case "kiwoom":
		if k, _ := config.KiwoomCreds(); k != "" {
			key := k
			if cfg.KiwoomAPIURL != "" {
				key += "@" + cfg.KiwoomAPIURL // 가짜 키움 테스트는 실계좌와 다른 잠금
			}
			return proc.AccountKey("kiwoom", key)
		}
	case "upbit":
		if k, _ := config.UpbitCreds(); k != "" {
			return proc.AccountKey("upbit", k)
		}
	}
	return ""
}
