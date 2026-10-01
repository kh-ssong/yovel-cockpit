// cockpitsup — 콕핏 감시자. cockpitd 를 자식으로 띄우고, 죽으면 다시 띄우고, 살아 있는데 멈추면 죽이고 다시 띄운다.
//
//	cockpitsup -data-dir C:/cockpit/data -- --broker kiwoom --mode live --data-dir C:/cockpit/data ...
//
// ★ reflex 교훈:
//   - 종료 코드로는 크래시를 못 가린다 — 살아 있는데 멈춘 프로세스가 더 위험하다 → **하트비트의 last_tick** 을 본다.
//   - 크래시 루프: 10분에 5번 재시작이면 30분 쉰다 (같은 원인으로 계속 죽으며 주문을 반복하지 않게) + 알림.
//   - 감시자 자신이 둘 뜨면 봇이 둘 뜬다 (2026-07-16) → 감시자도 잠금을 잡는다. 콕핏 자체의 잠금이 2차 방어선.
//   - 알림 문구가 크래시 위치를 말해야 한다 — 종료 코드·마지막 틱 시각을 싣는다.
//
// 윈도에선 작업 스케줄러에 "로그온 시 실행 + 실패 시 다시 시작" 으로 이 감시자를 등록한다 (DEPLOY 참고).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/config"
	"github.com/kh-ssong/yovel-cockpit/internal/notify"
	"github.com/kh-ssong/yovel-cockpit/internal/proc"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("cockpitsup: %v", err)
	}
}

func run() error {
	_, _ = config.LoadDotEnv(".env")
	exe, _ := os.Executable()
	defBin := filepath.Join(filepath.Dir(exe), "cockpitd"+filepath.Ext(exe))
	bin := flag.String("bin", defBin, "cockpitd 실행 파일")
	dataDir := flag.String("data-dir", "", "★ 자식의 --data-dir 과 같게 (하트비트·잠금 위치)")
	hang := flag.Duration("hang", 3*time.Minute, "last_tick 이 이만큼 멈추면 멈춘 것으로 보고 다시 띄운다")
	grace := flag.Duration("grace", 2*time.Minute, "막 띄운 뒤 첫 틱을 기다리는 시간")
	flag.Parse()
	args := flag.Args()
	if *dataDir == "" {
		return errors.New("-data-dir 이 필요하다 (자식의 --data-dir 과 같게)")
	}

	beat := filepath.Join(*dataDir, "heartbeat.json")
	sup := filepath.Join(*dataDir, "supervisor.heartbeat.json")
	_ = os.MkdirAll(*dataDir, 0o755)
	_ = proc.WriteBeat(sup, proc.Beat{TS: time.Now().UTC(), PID: os.Getpid()})
	release, err := proc.Acquire(filepath.Join(*dataDir, "cockpitsup.lock"),
		proc.LockInfo{PID: os.Getpid(), Started: time.Now(), DataDir: *dataDir, Heartbeat: sup}, time.Now())
	if err != nil {
		return fmt.Errorf("감시자 잠금: %w", err)
	}
	defer release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var notifier notify.Notifier = notify.Nop{}
	if tok, chat := config.TelegramCreds(); tok != "" && chat != "" {
		tg := notify.NewTelegram(ctx, notify.TelegramConfig{Token: tok, ChatID: chat, Prefix: "[cockpitsup]"})
		notifier = tg
		defer tg.Wait()
	}

	var pol policy
	for ctx.Err() == nil {
		if until := pol.haltedUntil(time.Now()); !until.IsZero() {
			log.Printf("크래시 루프 — %s 까지 쉰다", until.Format("15:04"))
			if !sleepCtx(ctx, time.Until(until)) {
				break
			}
		}

		cmd := exec.Command(*bin, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		started := time.Now()
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("cockpitd 실행: %w", err)
		}
		log.Printf("cockpitd 시작 pid %d", cmd.Process.Pid)

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		reason := watch(ctx, done, beat, started, *grace, *hang, cmd, &sup)
		if ctx.Err() != nil {
			_ = cmd.Process.Kill()
			<-done
			log.Printf("감시자 종료 — cockpitd 정지")
			break
		}

		next := pol.onExit(time.Now(), time.Since(started))
		msg := fmt.Sprintf("🔁 콕핏 재시작 — %s (가동 %s)", reason, time.Since(started).Round(time.Second))
		if next.halt {
			msg = fmt.Sprintf("⛔ 콕핏 크래시 루프 — 10분에 %d번 재시작. 30분 쉰다. 마지막: %s", next.recent, reason)
		}
		log.Print(msg)
		notifier.Send(msg)
		if !sleepCtx(ctx, next.wait) {
			break
		}
	}
	return nil
}

// watch — 자식이 죽거나 멈출 때까지 기다리고, 그 사유를 준다.
func watch(ctx context.Context, done <-chan error, beat string, started time.Time, grace, hang time.Duration,
	cmd *exec.Cmd, sup *string) string {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return "감시자 종료"
		case err := <-done:
			if err == nil {
				return "정상 종료(코드 0)"
			}
			return "종료: " + err.Error()
		case now := <-t.C:
			_ = proc.WriteBeat(*sup, proc.Beat{TS: now.UTC(), PID: os.Getpid()})
			b, err := proc.ReadBeat(beat)
			if stuck, why := hung(now, started, b, err, grace, hang); stuck {
				_ = cmd.Process.Kill()
				<-done
				return "멈춤: " + why
			}
		}
	}
}

// hung — 살아 있는데 멈췄나. 순수 함수.
func hung(now, started time.Time, b proc.Beat, readErr error, grace, hang time.Duration) (bool, string) {
	if now.Sub(started) < grace {
		return false, ""
	}
	if readErr != nil || b.TS.Before(started) {
		return true, "하트비트가 없다"
	}
	if b.LastTick.IsZero() || b.LastTick.Before(started) {
		return true, "집행 루프가 한 번도 안 돌았다"
	}
	if age := now.Sub(b.LastTick); age > hang {
		return true, fmt.Sprintf("집행 루프가 %s 째 멈춤 (마지막 %s)", age.Round(time.Second), b.LastTick.Local().Format("15:04:05"))
	}
	return false, ""
}

// policy — 재시작 간격과 크래시 루프 정지. 순수 상태기계.
type policy struct {
	restarts []time.Time
	halt     time.Time
}

type next struct {
	wait   time.Duration
	halt   bool
	recent int
}

const (
	loopWindow = 10 * time.Minute
	loopMax    = 5
	haltFor    = 30 * time.Minute
)

func (p *policy) onExit(now time.Time, ran time.Duration) next {
	p.restarts = append(p.restarts, now)
	var recent []time.Time
	for _, r := range p.restarts {
		if now.Sub(r) <= loopWindow {
			recent = append(recent, r)
		}
	}
	p.restarts = recent
	if len(recent) >= loopMax {
		p.halt = now.Add(haltFor)
		p.restarts = nil
		return next{wait: 0, halt: true, recent: len(recent)}
	}
	// 오래 잘 돌다 죽었으면 곧바로, 금방 죽기를 반복하면 점점 길게 (5s → 15s → 60s).
	wait := 5 * time.Second
	if ran < time.Minute {
		switch len(recent) {
		case 1:
			wait = 5 * time.Second
		case 2:
			wait = 15 * time.Second
		default:
			wait = time.Minute
		}
	}
	return next{wait: wait, recent: len(recent)}
}

func (p *policy) haltedUntil(now time.Time) time.Time {
	if now.Before(p.halt) {
		return p.halt
	}
	return time.Time{}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
