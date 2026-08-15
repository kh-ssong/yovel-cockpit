package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

const tok = "test-token-0123456789abcdef"

type fakeState struct{}

func (fakeState) Snapshot() protocol.StateSnapshot {
	return protocol.StateSnapshot{
		AsOf:      time.Now().UTC(),
		Mode:      protocol.ModePaper,
		Positions: []protocol.Position{},
		Orphans:   []protocol.Symbol{},
	}
}

func newTestServer() *Server {
	return New(Options{
		Port:      7737,
		Token:     tok,
		Mode:      protocol.ModePaper,
		StartedAt: time.Now(),
	}, fakeState{})
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

func TestBindsToLoopbackOnly(t *testing.T) {
	// 0.0.0.0 에 붙으면 같은 와이파이의 아무나 접근할 수 있다.
	if got := newTestServer().Addr(); got != "127.0.0.1:7737" {
		t.Fatalf("addr=%q", got)
	}
}
