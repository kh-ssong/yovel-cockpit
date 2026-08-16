// Package httpapi 는 데스크톱 셸(Tauri)이 붙는 로컬 API 다.
//
// ★ 데몬은 HTML 을 모른다 — JSON 만 낸다. 서버가 HTML 조각을 반환하는 구조로 시작하면
// 나중에 Tauri 셸로 옮길 때 UI 를 전부 다시 쓰게 된다.
// (Options.UI 로 정적 번들을 통째로 얹는 건 별개다 — 데몬이 파일을 나르는 것이지 화면을
// 만드는 게 아니라서, Tauri 가 같은 번들을 그대로 가져가면 서버 쪽은 손댈 게 없다.)
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
	"strconv"
	"strings"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/reconcile"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
	"github.com/kh-ssong/yovel-cockpit/internal/version"
)

// Engine 은 데몬이 들고 있는 상태와 판정. 상태는 데몬 소유, UI 는 stateless.
type Engine interface {
	Snapshot() protocol.StateSnapshot
	// Apply 는 다운링크 한 통을 판정해 반영한다. 반환값이 곧 업링크 ack.
	Apply(raw []byte, now time.Time) protocol.Ack
	// Plan 은 지금 무엇을 할지 계산한다 (주문은 내지 않는다).
	Plan(now time.Time) reconcile.Plan
	// Ledger 는 매매기록. ★ mode 없이는 조회할 수 없다.
	Ledger(ctx context.Context, mode protocol.Mode, limit int) ([]store.Order, error)
}

// maxDownlinkBytes — 목표 스냅샷은 포지션 수백 개여도 수십 KB 다.
const maxDownlinkBytes = 1 << 20

type Options struct {
	Port      int
	Token     string
	Mode      protocol.Mode
	StartedAt time.Time
	Log       *slog.Logger
	// Wake — 다운링크를 처리한 직후 호출된다.
	//
	// ★ 이게 없으면 목표가 도착해도 다음 집행 틱(기본 5초)까지 기다린다.
	// 스캘핑처럼 수명이 분 단위인 신호에서는 그 5초가 신호를 통째로 무의미하게 만든다.
	// 논블로킹이어야 한다 — 여기서 막히면 다운링크 응답이 늦어진다.
	Wake func()

	// Account — 계좌 상태 공급자. nil 이면 `/v1/state` 에 `account` 가 안 실린다.
	//
	// ★ 엔진이 아니라 여기서 받는 이유 = 잔고는 **브로커의 사실**이지 엔진의 판단이 아니다.
	// 엔진에 넣으면 판단 계층이 계좌를 알게 되고, 그 순간 "잔고를 보고 목표를 바꾸는"
	// 경로가 생긴다 — 배분은 콕핏이 하되 **엔진은 계좌를 모른다** 는 경계가 무너진다.
	Account func(ctx context.Context) *protocol.Account

	// UI — 로컬 대시보드(정적 번들). nil 이면 안 서빙한다.
	//
	// ★ 이 경로만 Bearer 검사에서 빠진다 (needsToken). 브라우저의 최초 내비게이션에는
	// Authorization 헤더를 붙일 수단이 없어서다. 대신 Host·Origin 가드는 그대로 걸리고,
	// CORS 헤더를 하나도 안 내보내므로 다른 출처의 페이지는 응답을 **읽을 수 없다**.
	// 자격은 페이지에 주입돼 이후 /v1/* 호출에 Bearer 로 쓰인다 (webui.Boot).
	UI http.Handler
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
	mux.HandleFunc("GET /v1/ledger", s.handleLedger)

	// 대시보드는 마지막에 건다 — 남는 경로 전부(`/`, `/assets/...`)를 받는다.
	if opt.UI != nil {
		mux.Handle("GET /", opt.UI)
	}

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
		if needsToken(r.URL.Path) && !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// needsToken — 자격이 필요한 경로.
//
// ★ 화이트리스트가 아니라 "/v1/ 이면 필요" 로 쓴 이유: 앞으로 추가되는 엔드포인트가
// 기본적으로 **보호받는 쪽**에 떨어져야 한다. 목록을 두면 새 엔드포인트를 목록에 넣는 걸
// 잊는 순간 그게 곧 무인증 구멍이고, 그 실수는 조용하다.
//
// 정적 대시보드(그 외 전부)는 토큰 없이 나간다. 브라우저 내비게이션에 헤더를 붙일 수단이
// 없어서다. 이건 자격을 푼 게 아니라 자격을 **전달하는 방법**을 바꾼 것이다 —
// 페이지가 토큰을 받아 가고, 그 페이지에 닿으려면 이미 Host·Origin 가드를 통과해야 한다.
func needsToken(path string) bool {
	return strings.HasPrefix(path, "/v1/")
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
	snap := s.eng.Snapshot()
	if s.opt.Account != nil {
		snap.Account = s.opt.Account(r.Context())
	}
	writeJSON(w, http.StatusOK, snap)
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

	// ★ 응답을 보낸 뒤에 깨운다. 집행을 기다렸다 응답하면 발행자가 그만큼 붙들린다.
	if s.opt.Wake != nil {
		s.opt.Wake()
	}
}

type ledgerResponse struct {
	// ★ AsOf 를 매번 싣는다. 지하철·엘리베이터 간헐 연결에서 캐시된 옛 원장을
	// 현재처럼 보여주는 게 이 화면의 최악 실패다. UI 는 이걸로 "n분 전" 을 그린다.
	AsOf   time.Time     `json:"as_of"`
	Mode   protocol.Mode `json:"mode"`
	Count  int           `json:"count"`
	Orders []store.Order `json:"orders"`
}

// handleLedger — 매매기록 조회.
//
// ★ mode 를 기본값으로 채우지 않는다. "전체 보기"가 기본이면 paper 와 live 가 합산돼
// 실계좌가 손실인데 수익으로 보인다 (실측: live 15건 −18,725원 vs paper 63건 +49,884원).
// 그래서 호출자가 반드시 고르게 하고, 안 고르면 400 이다.
func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	if s.eng == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "엔진이 아직 없다"})
		return
	}
	mode := protocol.Mode(r.URL.Query().Get("mode"))
	if mode != protocol.ModePaper && mode != protocol.ModeLive {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "mode=paper 또는 mode=live 를 명시할 것 — 합산하면 허위 손익이 된다",
		})
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	orders, err := s.eng.Ledger(r.Context(), mode, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if orders == nil {
		orders = []store.Order{}
	}
	writeJSON(w, http.StatusOK, ledgerResponse{
		AsOf: time.Now().UTC(), Mode: mode, Count: len(orders), Orders: orders,
	})
}
