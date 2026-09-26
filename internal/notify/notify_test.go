package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

type fakeTG struct {
	mu    sync.Mutex
	texts []string
	fail  int // 앞의 n 번은 429
}

func (f *fakeTG) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Too Many Requests",
			"parameters": map[string]int{"retry_after": 1}})
		return
	}
	var m map[string]any
	json.NewDecoder(r.Body).Decode(&m)
	f.texts = append(f.texts, m["text"].(string))
	w.Write([]byte(`{"ok":true}`))
}

func (f *fakeTG) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

func TestTelegramSendsAndRetries429(t *testing.T) {
	f := &fakeTG{fail: 1}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	tg := NewTelegram(ctx, TelegramConfig{Token: "SECRET", ChatID: "1", APIURL: srv.URL, Prefix: "[c]"})
	tg.sleep = func(time.Duration) {}

	tg.Send("hello")
	deadline := time.Now().Add(2 * time.Second)
	for len(f.got()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	tg.Wait()
	if g := f.got(); len(g) != 1 || g[0] != "[c] hello" {
		t.Fatalf("%v", g)
	}
}

// ★ 알림은 매매를 막지 않는다 — 텔레그램이 멈춰 있어도 Send 는 즉시 돌아온다.
func TestSendNeverBlocks(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tg := NewTelegram(ctx, TelegramConfig{Token: "x", ChatID: "1", APIURL: srv.URL})

	start := time.Now()
	for i := 0; i < 1000; i++ {
		tg.Send("spam")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("Send 가 막혔다")
	}
	if tg.dropped.Load() == 0 {
		t.Fatal("큐 초과분을 세지 않았다")
	}
}

// ★ 토큰은 URL 에 들어간다 — 실패 로그에 새면 봇 조종권이 로그 파일로 나간다.
func TestTokenNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	tg := NewTelegram(ctx, TelegramConfig{Token: "SUPERSECRET", ChatID: "1",
		APIURL: "http://127.0.0.1:1", Log: slog.New(slog.NewTextHandler(&buf, nil))})
	tg.Send("x")
	time.Sleep(300 * time.Millisecond)
	cancel()
	tg.Wait()
	if strings.Contains(buf.String(), "SUPERSECRET") {
		t.Fatalf("토큰이 로그에 찍혔다: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "텔레그램 전송 실패") {
		t.Fatalf("실패가 로그에 안 남았다: %s", buf.String())
	}
}

func TestFillFormat(t *testing.T) {
	sym := protocol.Symbol{Exchange: "KRX", Code: "005930"}
	buy := Fill(store.Order{Phase: "filled", Side: "buy", Symbol: sym, Qty: 12, Price: 71200,
		SlippageBp: 3.1, Mode: protocol.ModePaper}, "d205")
	if buy != "🟢 [PAPER] 매수 d205 · 005930 12주 @71,200 (슬립 +3.1bp)" {
		t.Fatalf("%q", buy)
	}
	exit := Fill(store.Order{Phase: "exit_filled", Side: "sell", Symbol: sym, Qty: 12, Price: 73000,
		ExitReason: "time", RealizedPct: 0.0253, Mode: protocol.ModeLive}, "d205")
	if exit != "🔵 [LIVE] 청산 d205 · 005930 12주 @73,000 · ⏰ 시간청산 · +2.53%" {
		t.Fatalf("%q", exit)
	}
	coin := Fill(store.Order{Phase: "filled", Symbol: protocol.Symbol{Exchange: "UPBIT", Code: "KRW-BTC"},
		Qty: 0.00052604, Price: 114_059_000, Mode: protocol.ModePaper}, "")
	if !strings.Contains(coin, "0.00052604개") || !strings.Contains(coin, "114,059,000") {
		t.Fatalf("%q", coin)
	}
}

func TestThrottle(t *testing.T) {
	th := NewThrottle(10 * time.Minute)
	now := time.Unix(0, 0)
	if !th.Allow("a", now) || th.Allow("a", now.Add(time.Minute)) || !th.Allow("b", now) {
		t.Fatal("같은 키는 막고 다른 키는 통과해야 한다")
	}
	if !th.Allow("a", now.Add(11*time.Minute)) {
		t.Fatal("간격이 지나면 다시 통과해야 한다")
	}
}
