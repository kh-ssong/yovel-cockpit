package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TokenPath — UI 가 읽어갈 로컬 토큰 파일.
func TokenPath(dataDir string) string { return filepath.Join(dataDir, "api-token") }

// LoadOrCreateToken 은 로컬 API 토큰을 읽거나 새로 만든다.
//
// ★ localhost 라도 토큰이 필요한 이유: 브라우저로 아무 웹페이지나 열어둔 상태에서
// 그 페이지가 127.0.0.1 로 요청을 던질 수 있다. 토큰이 없으면 악성 페이지가
// 사용자 데몬에 주문을 낼 수 있다 — "로컬이니까 안전" 은 틀린 직관이다.
//
// 파일 권한 0600 은 유닉스에서만 실효가 있다. Windows 에서는 사용자 프로필 아래
// (%AppData%) 라는 위치 자체가 방어이고, 파일 권한은 추가 보증이 아니다.
func LoadOrCreateToken(dataDir string) (string, error) {
	p := TokenPath(dataDir)
	b, err := os.ReadFile(p)
	switch {
	case err == nil:
		tok := strings.TrimSpace(string(b))
		if len(tok) < 16 {
			return "", fmt.Errorf("%s 의 토큰이 너무 짧다 — 파일을 지우면 새로 만든다", p)
		}
		return tok, nil
	case errors.Is(err, os.ErrNotExist):
	default:
		return "", err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}
