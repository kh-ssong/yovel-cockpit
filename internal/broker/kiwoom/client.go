package kiwoom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
)

// postJSON 은 한 번 쏘고 JSON 으로 받는다 (재시도 없음 — 재시도는 호출자가 판단).
func postJSON(ctx context.Context, hc *http.Client, url string, headers map[string]string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return httpError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

type httpError struct {
	Status int
	Body   string
}

func (e httpError) Error() string {
	b := e.Body
	if len(b) > 200 {
		b = b[:200]
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, b)
}

func (e httpError) retryable() bool {
	switch e.Status {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable:
		return true
	}
	return false
}

// envelope — 모든 응답이 공유하는 결과 코드.
//
// ★ 성공 판정 = HTTP 200/201 **그리고** return_code == 0.
// HTTP 200 만 보고 성공으로 읽으면 "주문 거부" 를 "주문 성공" 으로 기록하게 된다.
type envelope struct {
	ReturnCode int    `json:"return_code"`
	ReturnMsg  string `json:"return_msg"`
}

// call 은 api-id 를 붙여 요청하고, 재시도와 토큰 재발급을 처리한다.
//
// ★ 토큰 재발급은 **요청당 한 번만**. 무한 재발급이 곧 1계정 1토큰 사고의 형태다.
func (b *Broker) call(ctx context.Context, apiID, path string, body any, out any) error {
	var lastErr error
	tokenRetried := false

	for attempt := 0; attempt < 3; attempt++ {
		b.pace.wait(apiID, b.now, b.sleep)
		tok, err := b.tokens.get(ctx, false)
		if err != nil {
			return fmt.Errorf("토큰: %w", err)
		}

		var probe struct {
			envelope
		}
		raw := json.RawMessage{}
		err = postJSON(ctx, b.http, b.apiURL+path, map[string]string{
			"api-id":        apiID,
			"authorization": "Bearer " + tok,
		}, body, &raw)

		if err != nil {
			var he httpError
			// ★ 주문(매수·매도)은 5xx 를 재시도하지 않는다 (2026-09-27). 서버가 받았는지 모르는 상태에서
			// 다시 쏘면 **같은 신호로 두 번 산다.** 429 는 "처리 안 했다" 는 뜻이라 재시도해도 된다.
			// 취소·조회는 다시 해도 결과가 같으므로 그대로 재시도한다.
			isHTTP := asHTTPError(err, &he)
			if isOrderAPI(apiID) {
				switch {
				case !isHTTP, he.Status >= 500:
					// 네트워크 오류·5xx — 서버가 받았는지 모른다.
					return fmt.Errorf("%s: %w — %w", apiID, err, ErrMaybeSent)
				case he.Status != http.StatusTooManyRequests:
					return fmt.Errorf("%s: %w", apiID, err) // 4xx = 거부. 나가지 않았다
				}
			}
			if isHTTP && he.retryable() && attempt < 2 {
				lastErr = err
				b.sleep(backoff(attempt))
				continue
			}
			return fmt.Errorf("%s: %w", apiID, err)
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return fmt.Errorf("%s: 응답 파싱: %w", apiID, err)
		}

		if isTokenInvalid(probe.ReturnCode, probe.ReturnMsg) && !tokenRetried {
			tokenRetried = true
			if _, err := b.tokens.get(ctx, true); err != nil {
				return fmt.Errorf("%s: 토큰 갱신: %w", apiID, err)
			}
			continue
		}
		if isRateLimited(probe.ReturnCode, probe.ReturnMsg) && attempt < 2 {
			// ★ "허용된 요청 개수를 초과[1700]" — 키움이 **처리하지 않았다**고 말해 준 거절이라
			// 주문이어도 다시 내도 이중 주문이 아니다. 잠깐 쉬고 다시 (reflex 는 이걸 토큰 오류로 오판한 적이 있다).
			lastErr = rejectError{APIID: apiID, Code: probe.ReturnCode, Msg: probe.ReturnMsg}
			b.sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		if probe.ReturnCode != 0 {
			return rejectError{APIID: apiID, Code: probe.ReturnCode, Msg: probe.ReturnMsg}
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(raw, out)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%s: 재시도 소진", apiID)
	}
	return lastErr
}

// rejectError — 키움이 요청을 **받고 거부했다**(return_code ≠ 0). 주문이면 나가지 않은 것이다.
//
// ★ 타입으로 두는 이유: 거부 메시지에 쓸모 있는 답이 실려 온다
// (예: "[2000](855056:매수증거금이 부족합니다. 6주 매수가능)"). 문자열로 뭉개면 못 꺼낸다.
type rejectError struct {
	APIID string
	Code  int
	Msg   string
}

func (e rejectError) Error() string {
	return fmt.Sprintf("%s 거부 (%d): %s", e.APIID, e.Code, e.Msg)
}

// Unwrap — 거부 문구를 브로커 공통 오류로 옮긴다 (집행기가 드라이버를 몰라도 가리게).
// 800033 = 매도가능수량 부족(잠김 또는 미보유), 855056 = 매수증거금 부족.
func (e rejectError) Unwrap() error {
	switch {
	case strings.Contains(e.Msg, "800033") || strings.Contains(e.Msg, "매도가능"):
		return broker.ErrNotEnoughShare
	case strings.Contains(e.Msg, "855056") || strings.Contains(e.Msg, "증거금"):
		return broker.ErrInsufficient
	}
	return nil
}

// marginAllowRe — 855056(매수증거금 부족) 거부에 실려 오는 "N주 매수가능".
var marginAllowRe = regexp.MustCompile(`([0-9][0-9,]*)\s*주\s*매수\s*가능`)

// marginAllowance — 매수증거금 부족 거부면 키움이 말해준 가능 수량을 준다.
func marginAllowance(err error) (float64, bool) {
	var re rejectError
	if !errors.As(err, &re) || !strings.Contains(re.Msg, "855056") {
		return 0, false
	}
	m := marginAllowRe.FindStringSubmatch(re.Msg)
	if m == nil {
		return 0, false
	}
	return numOK(m[1])
}

// ErrMaybeSent — broker.ErrMaybeSent 그대로 (집행기가 드라이버를 몰라도 가릴 수 있게).
var ErrMaybeSent = broker.ErrMaybeSent

func isOrderAPI(apiID string) bool { return apiID == apiBuy || apiID == apiSell }

// isRateLimited — 키움 요청 한도 초과 (rc=5, "[1700]").
func isRateLimited(rc int, msg string) bool {
	return strings.Contains(msg, "1700") || (rc == 5 && strings.Contains(msg, "초과"))
}

// pacer — API 별 최소 간격. 키움 REST 는 API ID 마다 초당 ~5회라, 여유를 둬 4회(250ms)로 맞춘다.
// ★ 09:01 처럼 여러 종목이 몰리면 주문가능수량·주문·체결조회가 한꺼번에 나간다 — 한도를 넘으면
// 키움이 거절하고, 그 거절을 다른 오류로 읽으면 진입을 잃는다.
type pacer struct {
	mu   sync.Mutex
	last map[string]time.Time
}

const pacerGap = 250 * time.Millisecond

func (p *pacer) wait(apiID string, now func() time.Time, sleep func(time.Duration)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil {
		p.last = map[string]time.Time{}
	}
	if l, ok := p.last[apiID]; ok {
		if d := pacerGap - now().Sub(l); d > 0 {
			sleep(d)
		}
	}
	p.last[apiID] = now()
}

func backoff(attempt int) time.Duration { return time.Duration(1<<attempt) * time.Second }

func asHTTPError(err error, target *httpError) bool {
	he, ok := err.(httpError)
	if ok {
		*target = he
	}
	return ok
}
