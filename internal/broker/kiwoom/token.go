package kiwoom

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// tokenStore — 접근토큰 SSOT.
//
// ★ 키움은 **1계정 1토큰**이다. 새로 발급하면 서버가 기존 토큰을 무효화한다.
// 실측 사고: 봇 하나가 8초 사이 3번 재발급했더니 다른 봇의 토큰이 죽어 개장부터
// 504건 연속 실패로 세션이 통째로 날아갔다.
// ⟹ 파일 하나를 SSOT 로 두고, 만료가 임박했을 때 **한 쪽만** 발급한다.
//
// ★ 콕핏은 별도 앱키를 전제하지만 그렇다고 이 장치를 빼지 않는다 —
// 사용자가 콕핏을 두 번 띄우는 순간 같은 사고가 그대로 재현되기 때문이다.
type tokenStore struct {
	mu sync.Mutex

	path    string
	appKey  string
	secret  string
	apiURL  string
	http    *http.Client
	now     func() time.Time
	token   string
	expires time.Time
}

// expiryBuffer — 만료 이만큼 전이면 갱신 대상. 서버측 조기 무효화 사례가 있어 넉넉히 잡는다.
const expiryBuffer = time.Hour

// tokenFile — ★ flat6(Python)와 **같은 파일·같은 포맷**을 쓴다.
//
// 같은 앱키를 쓰는 두 프로세스가 각자 발급하면 서로의 토큰을 죽인다(1계정 1토큰).
// 포맷이 다르면 파일을 공유해도 서로의 기록을 못 읽어 결국 각자 발급하게 되므로,
// 포맷 통일이 곧 공유의 전제다. flat6: {"token", "expires_dt": "%Y%m%d%H%M%S"}.
type tokenFile struct {
	Token     string `json:"token"`
	ExpiresDt string `json:"expires_dt"`           // "20260815235959" (키움 원본 표기)
	ExpiresAt string `json:"expires_at,omitempty"` // 구 콕핏 포맷 — 읽기만 지원
}

const kiwoomExpiryLayout = "20060102150405"

// parsedExpiry 는 두 표기를 모두 받아준다 (한쪽만 지원하면 상대 기록을 못 읽는다).
func (f tokenFile) parsedExpiry() (time.Time, bool) {
	if f.ExpiresDt != "" {
		if v, err := time.ParseInLocation(kiwoomExpiryLayout, strings.TrimSpace(f.ExpiresDt), time.Local); err == nil {
			return v, true
		}
	}
	if f.ExpiresAt != "" {
		if v, err := time.Parse(time.RFC3339, f.ExpiresAt); err == nil {
			return v, true
		}
	}
	return time.Time{}, false
}

func newTokenStore(path, appKey, secret, apiURL string, hc *http.Client, now func() time.Time) *tokenStore {
	return &tokenStore{
		path:   path,
		appKey: appKey, secret: secret, apiURL: apiURL,
		http: hc, now: now,
	}
}

// get 은 유효한 토큰을 준다. force 면 "지금 것이 무효" 라는 뜻이다.
//
// ★ force 라고 무조건 발급하지 않는다. 파일의 토큰이 내가 쓰던 것과 다르면
// 다른 프로세스가 이미 갱신한 것이므로 **그걸 채택하고 끝낸다**.
// 그러지 않으면 8005 를 만난 두 프로세스가 서로의 토큰을 죽이며 무한히 재발급한다.
func (t *tokenStore) get(ctx context.Context, force bool) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !force && t.fresh() {
		return t.token, nil
	}

	if f, err := t.readFile(); err == nil && f.Token != "" {
		exp, ok := f.parsedExpiry()
		switch {
		case force && f.Token != t.token:
			// 남이 이미 갱신했다. 발급하지 않고 갈아탄다.
			t.token, t.expires = f.Token, exp
			return t.token, nil
		case !force && ok && t.now().Add(expiryBuffer).Before(exp):
			t.token, t.expires = f.Token, exp
			return t.token, nil
		}
	}

	return t.issue(ctx)
}

func (t *tokenStore) fresh() bool {
	return t.token != "" && t.now().Add(expiryBuffer).Before(t.expires)
}

func (t *tokenStore) issue(ctx context.Context) (string, error) {
	body := map[string]string{
		"grant_type": "client_credentials",
		"appkey":     t.appKey,
		"secretkey":  t.secret,
	}
	var resp struct {
		Token      string `json:"token"`
		ExpiresDt  string `json:"expires_dt"`
		ReturnCode int    `json:"return_code"`
		ReturnMsg  string `json:"return_msg"`
	}
	if err := postJSON(ctx, t.http, t.apiURL+"/oauth2/token", nil, body, &resp); err != nil {
		return "", err
	}
	if resp.ReturnCode != 0 || resp.Token == "" {
		return "", fmt.Errorf("토큰 발급 실패 (%d): %s", resp.ReturnCode, resp.ReturnMsg)
	}

	exp := t.now().Add(12 * time.Hour) // expires_dt 가 없을 때의 보수적 기본값
	raw := strings.TrimSpace(resp.ExpiresDt)
	if v, err := time.ParseInLocation(kiwoomExpiryLayout, raw, time.Local); err == nil {
		exp = v
	} else {
		raw = exp.Format(kiwoomExpiryLayout)
	}
	t.token, t.expires = resp.Token, exp

	// 파일 기록 실패는 치명적이지 않다 — ★ fail-open: 공유가 안 될 뿐 이번 요청은 살린다.
	_ = t.writeFile(tokenFile{Token: resp.Token, ExpiresDt: raw})
	return t.token, nil
}

func (t *tokenStore) readFile() (tokenFile, error) {
	var f tokenFile
	b, err := os.ReadFile(t.path)
	if err != nil {
		return f, err
	}
	err = json.Unmarshal(b, &f)
	return f, err
}

func (t *tokenStore) writeFile(f tokenFile) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	// 원자적 교체 — 반쯤 쓰인 파일을 다른 프로세스가 읽으면 토큰을 잃는다.
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, t.path)
}

// isTokenInvalid — 8005(토큰 무효) 판정.
//
// ★ return_code 만 보면 안 된다. 국내 API 는 `return_code=8005` 로 오지만
// 해외 API 는 `return_code=3` + `return_msg` 안에 "[8005:Token이 유효하지 않습니다]" 로 온다.
// 코드만 보면 해외 쪽 토큰 만료를 영영 못 잡는다.
func isTokenInvalid(rc int, msg string) bool {
	return rc == 8005 || strings.Contains(msg, "8005")
}
