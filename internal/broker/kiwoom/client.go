package kiwoom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
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
			if ok := asHTTPError(err, &he); ok && he.retryable() && attempt < 2 {
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
		if probe.ReturnCode != 0 {
			return fmt.Errorf("%s 거부 (%d): %s", apiID, probe.ReturnCode, probe.ReturnMsg)
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

func backoff(attempt int) time.Duration { return time.Duration(1<<attempt) * time.Second }

func asHTTPError(err error, target *httpError) bool {
	he, ok := err.(httpError)
	if ok {
		*target = he
	}
	return ok
}
