package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

func getWith(t *testing.T, opt Options, path string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	opt.Port, opt.Token, opt.Mode, opt.StartedAt = 7737, tok, protocol.ModePaper, time.Now()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = "127.0.0.1:7737"
	if auth {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	New(opt, &fakeEngine{}).Handler().ServeHTTP(w, r)
	return w
}

func TestLotsAndHoldingsRoutes(t *testing.T) {
	opt := Options{
		Lots: func(context.Context) (any, error) { return map[string]any{"lots": []string{"A1"}}, nil },
		Holdings: func(context.Context) (any, error) {
			return nil, errors.New("브로커 보유 조회 실패")
		},
	}
	if w := getWith(t, opt, "/v1/lots", true); w.Code != 200 || !strings.Contains(w.Body.String(), "A1") {
		t.Fatalf("lots %d %s", w.Code, w.Body)
	}
	// ★ 브로커 조회 실패는 빈 목록이 아니라 오류다 — 빈 목록이면 전 로트가 위험(short)으로 보인다.
	if w := getWith(t, opt, "/v1/holdings", true); w.Code != 500 {
		t.Fatalf("holdings 실패가 %d 로 나갔다", w.Code)
	}
	// 공급자가 없으면 503, 토큰이 없으면 401 — 로트는 계좌 정보다.
	if w := getWith(t, Options{}, "/v1/lots", true); w.Code != 503 {
		t.Fatalf("미연결 %d", w.Code)
	}
	if w := getWith(t, opt, "/v1/lots", false); w.Code != 401 {
		t.Fatalf("토큰 없이 %d", w.Code)
	}
}
