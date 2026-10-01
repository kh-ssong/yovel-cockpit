package upbit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
)

// ── 인증 ────────────────────────────────────────────────────────────────────

// token 은 업비트 JWT(HS256)를 만든다. 외부 라이브러리 없이 짓는 이유는 규격이 이것뿐이라서다.
//
// ★ query_hash 는 **실제로 보내는 문자열**의 해시여야 한다. 해시용 문자열과 전송용 문자열을
// 따로 만들면 인코딩 차이(`:`·`+`)로 401 "verify the query of Jwt" 가 난다 (reflex-agent
// 2026-07-18 실계좌). 그래서 호출자가 한 번 만든 query 를 URL 과 해시에 **같이** 쓴다.
func (b *Broker) token(query string) (string, error) {
	if b.cfg.AccessKey == "" || b.cfg.SecretKey == "" {
		return "", errNoKey
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	claims := map[string]string{
		"access_key": b.cfg.AccessKey,
		"nonce":      hex.EncodeToString(nonce),
	}
	if query != "" {
		h := sha512.Sum512([]byte(query))
		claims["query_hash"] = hex.EncodeToString(h[:])
		claims["query_hash_alg"] = "SHA512"
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(b.cfg.SecretKey))
	mac.Write([]byte(signing))
	return signing + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// ── 속도 제한 ───────────────────────────────────────────────────────────────

// limiter — 요청 사이 최소 간격.
//
// ★ 한도는 **계좌 단위**다(주문 8/s). 인스턴스마다 버킷을 두면 슬롯 수만큼 한도가 곱해진다
// (reflex-agent 2026-07-20). 콕핏은 계좌당 데몬 하나라 드라이버 하나에 버킷 하나면 된다.
type limiter struct {
	mu   sync.Mutex
	gap  time.Duration
	last time.Time
}

func (l *limiter) wait(now func() time.Time, sleep func(time.Duration)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if d := l.gap - now().Sub(l.last); d > 0 && !l.last.IsZero() {
		sleep(d)
	}
	l.last = now()
}

// ── 요청 ────────────────────────────────────────────────────────────────────

// apiError — 업비트 4xx 의 본문 `{"error":{"name","message"}}`.
//
// ★ name 을 삼키지 않는다. "주문 실패" 로 뭉개면 잔고 잠김(insufficient_funds_ask)을
// 일반 실패로 읽고 무한 재시도하게 된다 (reflex-agent 2026-07-21).
type apiError struct {
	Status  int
	Name    string
	Message string
}

func (e apiError) Error() string {
	return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Name, e.Message)
}

// Unwrap — 이름을 브로커 공통 오류로 옮긴다. 호출자가 errors.Is 로 원인을 가린다.
func (e apiError) Unwrap() error {
	switch e.Name {
	case "insufficient_funds_bid", "under_min_total_bid":
		return broker.ErrInsufficient
	case "insufficient_funds_ask", "under_min_total_ask":
		return broker.ErrNotEnoughShare
	}
	return nil
}

// errNoKey — 시세 전용(NewPublic) 드라이버로 인증 동작을 불렀다.
var errNoKey = errors.New("업비트 키 없음 — 시세 전용 드라이버로는 잔고·주문을 할 수 없다")

// errMaybeSent — broker.ErrMaybeSent (집행기가 드라이버를 몰라도 가릴 수 있게).
var errMaybeSent = broker.ErrMaybeSent

// do 는 인증 요청을 보낸다.
//
// ★ 재시도 규칙이 메서드마다 다르다:
//   - 429 → 전부 재시도. 서버가 **처리하지 않았다**고 말해준 것이다.
//   - 5xx·네트워크 오류 → GET/DELETE 만 재시도. POST(주문)는 **재시도하지 않는다** —
//     서버가 받았는지 모르는 상태에서 다시 쏘면 같은 신호로 두 번 산다.
func (b *Broker) do(ctx context.Context, method, path string, params url.Values, out any) error {
	return b.request(ctx, method, path, params, out, true)
}

// public — 시세 API. ★ 인증 헤더를 붙이지 않는다 (시세 서버가 받는 규격이 아니다).
// 속도 제한 버킷은 같이 쓴다 — 한 프로세스의 요청 총량을 한 곳에서 센다.
func (b *Broker) public(ctx context.Context, path string, params url.Values, out any) error {
	return b.request(ctx, http.MethodGet, path, params, out, false)
}

func (b *Broker) request(ctx context.Context, method, path string, params url.Values, out any, auth bool) error {
	query := ""
	if len(params) > 0 {
		query = params.Encode()
	}

	for attempt := 0; ; attempt++ {
		b.limit.wait(b.now, b.sleep)

		tok := ""
		if auth {
			t, err := b.token(query)
			if err != nil {
				return err
			}
			tok = t
		}

		var body io.Reader
		target := b.apiURL + path
		if method == http.MethodPost {
			// ★ 본문은 params 를 JSON 으로 옮긴 것. 해시는 위 query 로 계산했다 —
			// url.Values.Encode 가 키를 정렬하고 json.Marshal(map) 도 정렬하므로 순서가 같다.
			m := map[string]string{}
			for k := range params {
				m[k] = params.Get(k)
			}
			raw, err := json.Marshal(m)
			if err != nil {
				return err
			}
			body = bytes.NewReader(raw)
		} else if query != "" {
			target += "?" + query
		}

		req, err := http.NewRequestWithContext(ctx, method, target, body)
		if err != nil {
			return err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
		}

		status, raw, err := b.send(req)
		retryable := false
		switch {
		case err != nil:
			retryable = method != http.MethodPost
			if !retryable {
				return fmt.Errorf("%s %s: %v — %w", method, path, err, errMaybeSent)
			}
		case status == http.StatusTooManyRequests:
			retryable = true
		case status >= 500:
			retryable = method != http.MethodPost
			if !retryable {
				return fmt.Errorf("%s %s: HTTP %d — %w", method, path, status, errMaybeSent)
			}
		case status != http.StatusOK && status != http.StatusCreated:
			var e struct {
				Error struct {
					Name    string `json:"name"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(raw, &e)
			return fmt.Errorf("%s %s: %w", method, path,
				apiError{Status: status, Name: e.Error.Name, Message: e.Error.Message})
		default:
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("%s %s: 응답 파싱: %w", method, path, err)
			}
			return nil
		}

		if !retryable || attempt >= 2 {
			if err == nil {
				err = fmt.Errorf("HTTP %d", status)
			}
			return fmt.Errorf("%s %s: 재시도 소진: %v", method, path, err)
		}
		b.sleep(time.Duration(1<<attempt) * time.Second)
	}
}

func (b *Broker) send(req *http.Request) (int, []byte, error) {
	resp, err := b.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

// ── 숫자 ────────────────────────────────────────────────────────────────────

// fnum — 업비트는 같은 필드를 어떤 API 에선 문자열("0.0005")로, 어떤 API 에선 숫자로 준다.
// 둘 다 받는다. null 은 0.
type fnum float64

func (f *fnum) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("숫자가 아니다: %s", b)
	}
	*f = fnum(v)
	return nil
}

// volStr — 코인 수량 문자열. ★ 소수 8자리에서 **내림**한다 (올리면 가진 것보다 많이 팔려 든다).
func volStr(v float64) string {
	return strconv.FormatFloat(floor8(v), 'f', -1, 64)
}

func floor8(v float64) float64 {
	// 1e-9 여유 — 0.1+0.2 같은 표현 오차로 한 단위가 깎이는 걸 막는다.
	return float64(int64(v*1e8+1e-9)) / 1e8
}

// priceStr — 가격 문자열. 정수면 정수로, 아니면 필요한 만큼만.
func priceStr(p float64) string {
	return strconv.FormatFloat(p, 'f', -1, 64)
}
