package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/reconcile"
)

const tok = "test-token-0123456789abcdef"

type fakeEngine struct{ applied [][]byte }

func (f *fakeEngine) Snapshot() protocol.StateSnapshot {
	return protocol.StateSnapshot{
		AsOf:      time.Now().UTC(),
		Mode:      protocol.ModePaper,
		Positions: []protocol.Position{},
		Orphans:   []protocol.Symbol{},
	}
}

func (f *fakeEngine) Apply(raw []byte, _ time.Time) protocol.Ack {
	f.applied = append(f.applied, raw)
	return protocol.Ack{
		RefID:  "01J9Z8QK3M7X2ABCDEFGHJKMNP",
		Status: "rejected",
		Codes:  []protocol.RejectCode{protocol.CodeSig},
	}
}

func (f *fakeEngine) Plan(time.Time) reconcile.Plan { return reconcile.Plan{} }

func newTestServer() *Server { return newTestServerWith(&fakeEngine{}) }

func newTestServerWith(eng Engine) *Server {
	return New(Options{
		Port:      7737,
		Token:     tok,
		Mode:      protocol.ModePaper,
		StartedAt: time.Now(),
	}, eng)
}

func do(t *testing.T, path string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = "127.0.0.1:7737"
	r.Header.Set("Authorization", "Bearer "+tok)
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	newTestServer().Handler().ServeHTTP(w, r)
	return w
}

func TestHealthOK(t *testing.T) {
	w := do(t, "/v1/health", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	var got healthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.SHA == "" {
		t.Fatalf("health 가 비었다: %+v", got)
	}
	// ★ mode 가 빠지면 UI 가 paper 를 live 로 보여줄 수 있다.
	if got.Mode != protocol.ModePaper {
		t.Fatalf("mode=%q", got.Mode)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("상태 응답이 캐시 가능하다 — 옛 상태를 현재처럼 보여주게 된다")
	}
}

// ★ 토큰이 없으면 브라우저에 열린 아무 웹페이지나 데몬에 명령할 수 있다.
func TestTokenRequired(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"헤더 없음", ""},
		{"빈 토큰", "Bearer "},
		{"틀린 토큰", "Bearer wrong-token-value-here"},
		{"스킴 없음", tok},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, "/v1/health", func(r *http.Request) {
				r.Header.Del("Authorization")
				if c.header != "" {
					r.Header.Set("Authorization", c.header)
				}
			})
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("code=%d, 기대 401", w.Code)
			}
		})
	}
}

// ★ DNS 리바인딩: evil.com 을 127.0.0.1 로 해석시켜도 Host 헤더에는 evil.com 이 남는다.
func TestHostGuard(t *testing.T) {
	bad := []string{"evil.com", "evil.com:7737", "192.168.0.5:7737", "cockpit.local"}
	for _, h := range bad {
		t.Run(h, func(t *testing.T) {
			w := do(t, "/v1/health", func(r *http.Request) { r.Host = h })
			if w.Code != http.StatusForbidden {
				t.Fatalf("code=%d, 기대 403", w.Code)
			}
		})
	}
	good := []string{"127.0.0.1:7737", "localhost:7737", "[::1]:7737", "localhost"}
	for _, h := range good {
		t.Run(h, func(t *testing.T) {
			w := do(t, "/v1/health", func(r *http.Request) { r.Host = h })
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d, 기대 200", w.Code)
			}
		})
	}
}

func TestOriginGuard(t *testing.T) {
	bad := []string{"https://evil.com", "http://attacker.example:8080", "null", "file://"}
	for _, o := range bad {
		t.Run("deny "+o, func(t *testing.T) {
			w := do(t, "/v1/health", func(r *http.Request) { r.Header.Set("Origin", o) })
			if w.Code != http.StatusForbidden {
				t.Fatalf("code=%d, 기대 403", w.Code)
			}
		})
	}
	good := []string{"tauri://localhost", "http://localhost:1420", "http://127.0.0.1:7737"}
	for _, o := range good {
		t.Run("allow "+o, func(t *testing.T) {
			w := do(t, "/v1/health", func(r *http.Request) { r.Header.Set("Origin", o) })
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d, 기대 200 (body=%s)", w.Code, w.Body)
			}
		})
	}
}

func TestStateEndpoint(t *testing.T) {
	w := do(t, "/v1/state", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	var snap protocol.StateSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Mode == "" {
		t.Fatal("mode 없는 스냅샷 — paper/live 합산 사고의 입구다")
	}
}

// 다운링크 엔드포인트도 같은 가드를 받는다 — 그리고 거절은 HTTP 오류가 아니라 ack 로 나온다.
func TestDownlinkGoesThroughGuardsAndReturnsAck(t *testing.T) {
	eng := &fakeEngine{}
	srv := newTestServerWith(eng)

	post := func(mutate func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/downlink", strings.NewReader(`{"v":1}`))
		r.Host = "127.0.0.1:7737"
		r.Header.Set("Authorization", "Bearer "+tok)
		if mutate != nil {
			mutate(r)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}

	if w := post(func(r *http.Request) { r.Header.Del("Authorization") }); w.Code != http.StatusUnauthorized {
		t.Fatalf("무토큰 code=%d", w.Code)
	}
	if len(eng.applied) != 0 {
		t.Fatal("가드를 못 통과한 요청이 엔진까지 갔다")
	}

	w := post(nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	var ack protocol.Ack
	if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
		t.Fatal(err)
	}
	// ★ 거절을 HTTP 4xx 로 표현하지 않는다. MQTT 에는 상태코드가 없어서,
	// 두 경로가 다른 모양이면 그게 곧 배선 버그가 된다.
	if ack.Status != "rejected" || len(ack.Codes) == 0 {
		t.Fatalf("ack=%+v", ack)
	}
	if len(eng.applied) != 1 {
		t.Fatalf("엔진 호출 %d회", len(eng.applied))
	}
}

func TestBindsToLoopbackOnly(t *testing.T) {
	// 0.0.0.0 에 붙으면 같은 와이파이의 아무나 접근할 수 있다.
	if got := newTestServer().Addr(); got != "127.0.0.1:7737" {
		t.Fatalf("addr=%q", got)
	}
}
