package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInjectAtMarker(t *testing.T) {
	html := []byte(`<!doctype html><head><!--cockpit-boot--><title>x</title></head><body></body>`)
	out := string(inject(html, Boot{Token: "tok-123", Mode: "paper"}))

	if strings.Contains(out, bootMarker) {
		t.Fatal("마커가 남아 있다 — 치환이 아니라 덧붙였다")
	}
	if !strings.Contains(out, `window.__COCKPIT__=`) || !strings.Contains(out, `"token":"tok-123"`) {
		t.Fatalf("부팅 정보가 안 실렸다: %s", out)
	}
	// ★ 마커 자리(=<head> 안)에 들어가야 앱 스크립트보다 먼저 실행된다.
	if strings.Index(out, "window.__COCKPIT__") > strings.Index(out, "</head>") {
		t.Fatal("주입 위치가 </head> 뒤다 — 앱이 토큰을 못 본다")
	}
}

// ★ 마커가 없는 산출물이 와도 토큰은 실려야 한다. 안 실리면 화면은 뜨는데 모든 호출이
// 401 이라, 사용자에게는 원인 없는 "UI 고장" 으로만 보인다.
func TestInjectFallbackWithoutMarker(t *testing.T) {
	out := string(inject([]byte(`<!doctype html><head><title>x</title></head><body></body>`),
		Boot{Token: "t", Mode: "live"}))
	if !strings.Contains(out, "window.__COCKPIT__") {
		t.Fatalf("마커 없을 때 폴백 주입이 안 됐다: %s", out)
	}
	if strings.Index(out, "window.__COCKPIT__") > strings.Index(out, "</head>") {
		t.Fatal("폴백 주입이 </head> 뒤다")
	}
}

// 토큰에 따옴표류가 섞여도 스크립트가 깨지지 않아야 한다 (문자열 이어붙이기 금지).
func TestInjectEscapes(t *testing.T) {
	out := string(inject([]byte(`<head><!--cockpit-boot--></head>`),
		Boot{Token: `a"</script><script>alert(1)//`, Mode: "paper"}))
	if strings.Contains(out, `<script>alert(1)`) {
		t.Fatalf("주입 이스케이프가 깨졌다: %s", out)
	}
	var boot Boot
	raw := out[strings.Index(out, "=")+1 : strings.LastIndex(out, ";</script>")]
	if err := json.Unmarshal([]byte(raw), &boot); err != nil {
		t.Fatalf("주입된 JSON 이 깨졌다: %v (%s)", err, raw)
	}
}

// dist 가 비어 있는 개발 빌드에서도 침묵하지 않는다.
func TestNotBuiltServesGuidance(t *testing.T) {
	if Built() {
		t.Skip("이 빌드에는 UI 산출물이 박혀 있다 — 미빌드 경로는 볼 수 없다")
	}
	rec := httptest.NewRecorder()
	Handler(Boot{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("미빌드인데 %d — 200 이면 헬스체크가 정상으로 읽는다", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "npm run build") {
		t.Fatal("안내에 빌드 방법이 없다")
	}
}
