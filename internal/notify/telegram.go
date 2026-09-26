// Package notify 는 사람에게 알린다 (텔레그램).
//
// ★ 알림은 **절대 매매를 막지 않는다.** 보내기는 별도 고루틴이 하고, 큐가 차면 버린다(버린 수는 센다).
// 텔레그램이 느리거나 죽었다고 청산이 늦어지면, 알림이 사고를 만든 것이다.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Notifier — 한 줄 보내기. 구현은 비동기여야 한다.
type Notifier interface {
	Send(text string)
}

// Nop — 설정이 없을 때.
type Nop struct{}

func (Nop) Send(string) {}

type TelegramConfig struct {
	Token  string
	ChatID string
	// APIURL — 테스트용. 비면 https://api.telegram.org
	APIURL string
	HTTP   *http.Client
	Log    *slog.Logger
	// Prefix — 모든 메시지 앞에 붙는다 (예: "[cockpit·PAPER]"). 여러 콕핏이 한 채팅방을 쓸 때 구분용.
	Prefix string
}

type Telegram struct {
	cfg     TelegramConfig
	q       chan string
	dropped atomic.Int64
	wg      sync.WaitGroup
	sleep   func(time.Duration)
}

// NewTelegram 은 보내기 고루틴을 띄운다. ctx 가 끝나면 남은 큐를 잠깐 비우고 멈춘다.
func NewTelegram(ctx context.Context, cfg TelegramConfig) *Telegram {
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.telegram.org"
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	t := &Telegram{cfg: cfg, q: make(chan string, 256), sleep: time.Sleep}
	t.wg.Add(1)
	go t.run(ctx)
	return t
}

// Send — 큐에 넣고 바로 돌아온다. 큐가 차 있으면 버린다.
func (t *Telegram) Send(text string) {
	if t.cfg.Prefix != "" {
		text = t.cfg.Prefix + " " + text
	}
	select {
	case t.q <- text:
	default:
		t.dropped.Add(1)
	}
}

// Wait — 보내기 고루틴이 끝날 때까지 (종료 시).
func (t *Telegram) Wait() { t.wg.Wait() }

func (t *Telegram) run(ctx context.Context) {
	defer t.wg.Done()
	for {
		select {
		case <-ctx.Done():
			// 종료 알림 정도는 나가게 잠깐 비운다.
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case msg := <-t.q:
					t.deliver(context.Background(), msg)
				default:
					return
				}
			}
			return
		case msg := <-t.q:
			if n := t.dropped.Swap(0); n > 0 {
				msg = fmt.Sprintf("(알림 %d 건이 큐 초과로 버려졌다)\n%s", n, msg)
			}
			t.deliver(ctx, msg)
		}
	}
}

// deliver — 한 통 보낸다. 429 면 retry_after 만큼 기다렸다 한 번 더. 그 밖의 실패는 로그만.
func (t *Telegram) deliver(ctx context.Context, text string) {
	for attempt := 0; attempt < 2; attempt++ {
		retry, err := t.post(ctx, text)
		if err == nil {
			return
		}
		if retry > 0 && attempt == 0 {
			t.sleep(retry)
			continue
		}
		// ★ 토큰은 로그에 안 찍는다 — URL 에 들어가므로 에러 문자열에서도 빼야 한다.
		t.cfg.Log.Warn("텔레그램 전송 실패", "err", err)
		return
	}
}

func (t *Telegram) post(ctx context.Context, text string) (retryAfter time.Duration, err error) {
	body, _ := json.Marshal(map[string]any{
		"chat_id": t.cfg.ChatID, "text": text, "disable_web_page_preview": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.cfg.APIURL+"/bot"+t.cfg.Token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("요청 생성 실패")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.cfg.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("네트워크 오류") // ★ err 원문에 토큰 든 URL 이 들어 있다
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusOK {
		return 0, nil
	}
	var r struct {
		Description string `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	_ = json.Unmarshal(raw, &r)
	return time.Duration(r.Parameters.RetryAfter) * time.Second,
		fmt.Errorf("HTTP %d: %s", resp.StatusCode, r.Description)
}
