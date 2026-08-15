// Package httpapi 는 데스크톱 셸(Tauri)이 붙는 로컬 API 다.
//
// ★ 데몬은 HTML 을 모른다 — JSON 만 낸다. 서버가 HTML 조각을 반환하는 구조로 시작하면
// 나중에 Tauri 셸로 옮길 때 UI 를 전부 다시 쓰게 된다.
//
// ★ 그리고 이 서버는 UI 의 사이드카가 아니다. 창을 닫아도 데몬은 계속 돈다 —
// 이 봇이 일하는 시간이 하필 사용자가 화면을 안 보는 시간(장 시작 직후·새벽)이라서다.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/reconcile"
	"github.com/kh-ssong/yovel-cockpit/internal/version"
)

// Engine 은 데몬이 들고 있는 상태와 판정. 상태는 데몬 소유, UI 는 stateless.
type Engine interface {
	Snapshot() protocol.StateSnapshot
	// Apply 는 다운링크 한 통을 판정해 반영한다. 반환값이 곧 업링크 ack.
	Apply(raw []byte, now time.Time) protocol.Ack
	// Plan 은 지금 무엇을 할지 계산한다 (주문은 내지 않는다).
	Plan(now time.Time) reconcile.Plan
}

// maxDownlinkBytes — 목표 스냅샷은 포지션 수백 개여도 수십 KB 다.
const maxDownlinkBytes = 1 << 20

type Options struct {
	Port      int
	Token     string
	Mode      protocol.Mode
	StartedAt time.Time
	Log       *slog.Logger
}

type Server struct {
	opt  Options
	eng  Engine
	http *http.Server
}

func New(opt Options, eng Engine) *Server {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	s := &Server{opt: opt, eng: eng}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/state", s.handleState)
	mux.HandleFunc("GET /v1/plan", s.handlePlan)
	// ★ 릴레이가 붙기 전까지 다운링크를 흘려 넣는 루프백 경로.
	// 위험해 보이지만 실제로는 아니다: 진입 intent 는 pitwall 서명이 있어야만 통과하므로,
	// 이 엔드포인트를 두드릴 수 있어도 서명키 없이는 주문을 만들 수 없다.
	mux.HandleFunc("POST /v1/downlink", s.handleDownlink)

	s.http = &http.Server{
		// ★ 127.0.0.1 에만 붙는다. 0.0.0.0 이면 같은 와이파이의 아무나 접근할 수 있다.
		Addr:              net.JoinHostPort("127.0.0.1", fmt.Sprint(opt.Port)),
		Handler:           s.guard(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

func (s *Server) Addr() string { return s.http.Addr }

func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return err
	}
	go func() {
		if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.opt.Log.Error("로컬 API 종료됨", "err", err)
		}
	}()
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

// Handler 는 테스트가 net 없이 쓰기 위한 진입점.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// guard 는 세 겹을 건다. 하나라도 빠지면 나머지가 무의미해지는 조합이다.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1) Host — DNS 리바인딩 방어.
		//    공격자가 evil.com 을 127.0.0.1 로 해석시켜 브라우저의 동일출처 검사를 우회하는 수법.
		//    이때 Host 헤더에는 evil.com 이 남으므로 여기서 잘린다.
		if !localHost(r.Host) {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		// 2) Origin — 웹페이지가 던진 요청인지 본다.
		if o := r.Header.Get("Origin"); o != "" && !allowedOrigin(o) {
			http.Error(w, "bad origin", http.StatusForbidden)
			return
		}
		// 3) 토큰 — 위 둘을 통과해도 자격은 따로 본다.
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimSpace(h[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.opt.Token)) == 1
}

func localHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	switch strings.ToLower(strings.Trim(h, "[]")) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func allowedOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "tauri":
		return true // Tauri 셸이 쓰는 커스텀 스킴
	case "http", "https":
		return localHost(u.Host)
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 이 응답은 캐시되면 안 된다 — 옛 상태를 현재처럼 보여주는 게 이 UI 의 최악 실패다.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type healthResponse struct {
	OK        bool          `json:"ok"`
	Version   string        `json:"version"`
	SHA       string        `json:"sha"`
	Dirty     bool          `json:"dirty,omitempty"`
	Mode      protocol.Mode `json:"mode"`
	UptimeSec int64         `json:"uptime_sec"`
	Now       time.Time     `json:"now"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	v := version.Get()
	writeJSON(w, http.StatusOK, healthResponse{
		OK:      true,
		Version: v.Version,
		SHA:     v.SHA,
		Dirty:   v.Dirty,
		// ★ mode 를 health 에 싣는다. UI 가 paper 를 live 로 착각해 보여주는 사고를
		// 가장 싼 지점에서 막는다.
		Mode:      s.opt.Mode,
		UptimeSec: int64(time.Since(s.opt.StartedAt).Seconds()),
		Now:       time.Now().UTC(),
	})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if s.eng == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "엔진이 아직 없다"})
		return
	}
	writeJSON(w, http.StatusOK, s.eng.Snapshot())
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	if s.eng == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "엔진이 아직 없다"})
		return
	}
	writeJSON(w, http.StatusOK, s.eng.Plan(time.Now().UTC()))
}

func (s *Server) handleDownlink(w http.ResponseWriter, r *http.Request) {
	if s.eng == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "엔진이 아직 없다"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDownlinkBytes))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
		return
	}
	ack := s.eng.Apply(raw, time.Now().UTC())

	// ★ 거절도 200 으로 준다. HTTP 상태코드가 아니라 ack 가 프로토콜의 결과 표현이고,
	// 릴레이(MQTT)에는 상태코드라는 게 없다 — 두 경로가 다른 모양이면 그게 곧 배선 버그가 된다.
	writeJSON(w, http.StatusOK, ack)
	s.opt.Log.Info("다운링크 처리", "typ", ack.RefTyp, "status", ack.Status, "codes", ack.Codes)
}
