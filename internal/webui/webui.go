// Package webui 는 데몬이 서빙하는 로컬 대시보드(조종석 화면)다.
//
// ★ 이 패키지는 산출물을 embed 해서 내보내기만 한다 — 판정도, 상태도 없다.
// UI 가 자기 상태를 들고 있으면 "화면에 보이는 것" 과 "데몬이 아는 것" 이 갈리는데,
// 이 화면의 최악 실패가 정확히 그것이다(옛 상태를 현재처럼 보여주기).
//
// ★ dist 는 커밋하지 않는다 (.gitkeep 만 있다). 빌드 산출물을 저장소에 두면
// "소스는 고쳤는데 embed 된 건 옛 번들" 이라는 조용한 드리프트가 생기고, 그건 이 프로젝트가
// sha 를 ldflags 로 박아가며 막고 있는 바로 그 실패 모양이다. 대신 산출물이 없으면
// **안내 페이지**를 낸다 — 빈 화면이나 404 로 두면 "UI 가 없는 것" 과 "UI 가 깨진 것" 이
// 같아 보인다.
package webui

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist — `npm run build` 산출물. vite 의 outDir 이 여기를 가리킨다 (ui/vite.config.js).
//
//go:embed all:dist
var dist embed.FS

// bootMarker — index.html 안의 주입 지점. ui/index.html 에 그대로 들어 있고
// vite 는 주석을 보존하므로 산출물까지 따라온다.
const bootMarker = "<!--cockpit-boot-->"

// Boot 는 서버가 페이지에 심어 주는 부팅 정보.
//
// ★ 토큰을 여기 싣는 이유: 브라우저의 최초 내비게이션에는 Authorization 헤더를 붙일 수단이
// 없다. 그래서 정적 자원은 토큰 검사에서 빼고(httpapi.needsToken), 대신 페이지가 받아 가서
// 이후 /v1/* 호출에 Bearer 로 쓴다.
//
// 이게 안전한 이유는 세 겹이다:
//   - 이 서버는 127.0.0.1 에만 붙는다 (원격에서 도달 불가)
//   - Host / Origin 가드가 정적 경로에도 그대로 걸린다 → 다른 출처의 페이지가 던진 요청은 403
//   - CORS 헤더를 하나도 내보내지 않는다 → 크로스오리진 fetch 는 응답 본문을 **읽을 수 없다**
//
// 즉 이 토큰에 닿을 수 있는 건 이미 같은 PC 에서 같은 사용자로 도는 프로세스뿐인데,
// 그쪽은 어차피 {data-dir}/api-token 파일을 직접 읽을 수 있다. 새로 열리는 면이 없다.
type Boot struct {
	Token string `json:"token"`
	// Mode — paper | live. ★ 화면이 paper 를 live 로 착각해 보여주는 사고를 가장 싼 지점에서 막는다.
	Mode string `json:"mode"`
}

// ErrNotBuilt — UI 산출물이 embed 되지 않았다 (npm run build 를 안 돌린 개발 빌드).
var ErrNotBuilt = errors.New("UI 산출물이 없다 — ui/ 에서 npm ci && npm run build")

// Built 는 UI 가 실제로 박혀 있는지 답한다. 데몬이 기동 로그에 쓴다.
func Built() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return err == nil
}

type handler struct {
	files fs.FS
	index []byte // 부팅 정보가 주입된 index.html
	built bool
}

// Handler 는 embed 된 SPA 를 서빙한다. 산출물이 없으면 안내 페이지를 내는 핸들러를 준다.
func Handler(boot Boot) http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return &handler{}
	}
	raw, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return &handler{}
	}
	return &handler{files: sub, index: inject(raw, boot), built: true}
}

// inject 는 부팅 정보를 index.html 에 심는다.
//
// ★ 순수 함수로 뽑아 둔 이유는 테스트다 — embed 는 컴파일 타임이라 산출물이 없는 CI 에서는
// 핸들러 경로를 통째로 못 밟는다. 주입 규약이 깨지는 건 여기서 잡는다.
func inject(html []byte, boot Boot) []byte {
	// json.Marshal 로 감싼다. 토큰은 base64url 이라 지금은 이스케이프가 필요 없지만,
	// "지금은 안전한 값" 에 기대어 문자열을 이어 붙이는 습관이 나중에 주입 구멍이 된다.
	payload, err := json.Marshal(boot)
	if err != nil {
		return html
	}
	tag := []byte(`<script>window.__COCKPIT__=` + string(payload) + `;</script>`)

	if i := bytes.Index(html, []byte(bootMarker)); i >= 0 {
		out := make([]byte, 0, len(html)+len(tag))
		out = append(out, html[:i]...)
		out = append(out, tag...)
		out = append(out, html[i+len(bootMarker):]...)
		return out
	}
	// 마커가 없으면 </head> 앞에 넣는다. ★ 조용히 포기하지 않는 이유: 토큰이 안 실리면
	// 화면은 뜨는데 모든 API 호출이 401 이라, 사용자에겐 "UI 가 고장" 으로만 보인다.
	if i := bytes.Index(html, []byte("</head>")); i >= 0 {
		out := make([]byte, 0, len(html)+len(tag))
		out = append(out, html[:i]...)
		out = append(out, tag...)
		out = append(out, html[i:]...)
		return out
	}
	return append(tag, html...)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.built {
		writeNotBuilt(w)
		return
	}

	clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	if clean == "/" || clean == "/index.html" {
		h.writeIndex(w)
		return
	}

	f, err := h.files.Open(strings.TrimPrefix(clean, "/"))
	if err != nil {
		// ★ SPA 폴백(모르는 경로 → index)을 두지 않는다. 클라이언트 라우팅이 없는 한 화면이고,
		// 폴백이 있으면 오타 난 자산 요청이 HTML 로 200 을 받아 "왜 안 뜨지" 로 바뀐다.
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	rs, ok := f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		http.NotFound(w, r)
		return
	}
	noStore(w)
	http.ServeContent(w, r, st.Name(), st.ModTime(), rs)
}

func (h *handler) writeIndex(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	noStore(w)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.index)
}

// noStore — 자산까지 캐시를 끈다.
//
// ★ 파일명에 해시가 붙으니 자산은 캐시해도 되지만, 그렇게 하면 "데몬은 새 코드인데 화면은
// 캐시된 옛 UI" 라는 상태가 만들어진다. 로컬 루프백에서 아낄 대역폭보다, 그 드리프트가 비싸다.
// index.html 에는 토큰도 들어 있어 어차피 저장되면 안 된다.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

func writeNotBuilt(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	noStore(w)
	// ★ 503 인 이유: 200 으로 안내문을 내면 스크립트·헬스체크가 "UI 정상" 으로 읽는다.
	// 데몬 자체는 멀쩡하므로(주문은 계속 나간다) 여기서 죽이지도 않는다.
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(notBuiltHTML))
}

const notBuiltHTML = `<!doctype html>
<html lang="ko"><head><meta charset="utf-8"><title>yovel cockpit — UI 미빌드</title>
<style>
 body{background:#0b0e14;color:#c9d1d9;font:15px/1.7 ui-monospace,SFMono-Regular,Menlo,monospace;
      margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center}
 main{max-width:44rem;padding:2rem}
 h1{font-size:1.1rem;color:#e6edf3;margin:0 0 1rem}
 code{background:#161b22;padding:.15rem .4rem;border-radius:4px;color:#7ee787}
 p{margin:.6rem 0}
 .dim{color:#8b949e}
</style></head><body><main>
<h1>UI 가 이 바이너리에 들어 있지 않다</h1>
<p>데몬은 정상이다 — 주문·집행·API 는 그대로 돈다. 화면만 없다.</p>
<p>빌드: <code>cd ui &amp;&amp; npm ci &amp;&amp; npm run build</code>
   → 다시 <code>scripts/build.sh</code></p>
<p class="dim">산출물(<code>internal/webui/dist</code>)은 일부러 커밋하지 않는다.
   커밋해 두면 "소스는 고쳤는데 박힌 건 옛 번들" 이 조용히 생긴다.</p>
<p class="dim">그동안은 API 로 직접 볼 수 있다:
   <code>curl -H "Authorization: Bearer $(cat {data-dir}/api-token)" http://127.0.0.1:7737/v1/state</code></p>
</main></body></html>
`
