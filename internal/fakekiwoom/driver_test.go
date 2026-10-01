package fakekiwoom_test

// 콕핏의 **실제 키움 드라이버**를 스펙 기반 가짜 서버에 붙여 본다.
// 여기서 통과한다 = 드라이버가 스펙에 없는 필드를 보내지 않고, 스펙의 모든 응답 필드가 섞여 와도
// 파싱하고, 아래 장애 상황에서 약속대로 행동한다.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/kiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/fakekiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

var (
	ctx = context.Background()
	sam = protocol.Symbol{Exchange: "KRX", Code: "005930"}
)

func setup(t *testing.T, cash float64) (*fakekiwoom.Server, *kiwoom.Broker) {
	t.Helper()
	spec, err := fakekiwoom.LoadSpec("testdata/spec_subset.json")
	if err != nil {
		t.Fatal(err)
	}
	fk := fakekiwoom.New(fakekiwoom.Config{Spec: spec, Cash: cash, Prices: map[string]float64{"005930": 286_500}})
	srv := httptest.NewServer(fk)
	t.Cleanup(srv.Close)
	br, err := kiwoom.New(kiwoom.Config{
		AppKey: "fake", SecretKey: "fake", DataDir: t.TempDir(), APIURL: srv.URL,
		FillTimeout: 1500 * time.Millisecond, FillPoll: 50 * time.Millisecond,
		Sleep: func(d time.Duration) { time.Sleep(minDur(d, 50*time.Millisecond)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return fk, br
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func buy(t *testing.T, br *kiwoom.Broker, qty float64) (broker.Fill, error) {
	t.Helper()
	return br.Buy(ctx, broker.OrderRequest{Symbol: sam, Qty: qty, RefPrice: 286_500})
}

// 기본 왕복 — 매수 → 잔고 → 시세 → 매도. 수수료·세금은 체결 조회(ka10076) 실측이다.
func TestRoundTrip(t *testing.T) {
	fk, br := setup(t, 10_000_000)
	f, err := buy(t, br, 3)
	if err != nil || f.Qty != 3 || f.Price != 286_500 || f.Partial || f.FeeKRW <= 0 {
		t.Fatalf("매수 %+v %v", f, err)
	}
	hs, err := br.Positions(ctx)
	if err != nil || len(hs) != 1 || hs[0].Symbol.Code != "005930" || hs[0].Qty != 3 {
		t.Fatalf("잔고 %+v %v", hs, err) // 잔고 stk_cd 는 "A005930" 으로 온다
	}
	cash, _ := br.Cash(ctx)
	if cash.Deposit <= 0 || cash.Seed <= 0 {
		t.Fatalf("현금 %+v", cash)
	}
	if q, err := br.Quote(ctx, sam); err != nil || q.Price != 286_500 {
		t.Fatalf("시세 %+v %v", q, err) // cur_prc 는 "+286500"
	}
	s, err := br.Sell(ctx, broker.OrderRequest{Symbol: sam, Qty: 3, RefPrice: 286_500})
	if err != nil || s.Qty != 3 || s.FeeKRW <= f.FeeKRW { // 매도는 세금까지
		t.Fatalf("매도 %+v %v", s, err)
	}
	if st := fk.State(); len(st.Holdings) != 0 {
		t.Fatalf("다 팔았는데 남았다 %+v", st.Holdings)
	}
}

// 분할체결 — 전량이 찰 때까지 기다린다 (조기 반환하면 잔량이 장부 밖 유령).
func TestSplitFillWaitsForAll(t *testing.T) {
	fk, br := setup(t, 10_000_000)
	fk.SetScenario(fakekiwoom.Scenario{Fill: "split", Chunks: 3, IntervalMs: 200})
	f, err := buy(t, br, 9)
	if err != nil || f.Qty != 9 || f.Partial {
		t.Fatalf("%+v %v", f, err)
	}
}

// ★ 체결이 멈춤 — 시간 초과 시 잔량을 **취소**하고 체결분만 부분으로 돌려준다.
func TestStallCancelsRemainder(t *testing.T) {
	fk, br := setup(t, 10_000_000)
	fk.SetScenario(fakekiwoom.Scenario{Fill: "stall", StallFrac: 0.5})
	f, err := buy(t, br, 10)
	if err != nil || !f.Partial || f.Qty != 5 {
		t.Fatalf("%+v %v", f, err)
	}
	st := fk.State()
	if len(st.Orders) != 1 || st.Orders[0].Status != "cancelled" {
		t.Fatalf("★ 잔량 주문이 살아 있다 — 나중에 체결되면 유령: %+v", st.Orders)
	}
}

// 855056 — 거부 문구의 "N주 매수가능" 으로 한 번만 다시 낸다.
func TestMarginRejectRetriesOnce(t *testing.T) {
	fk, br := setup(t, 10_000_000)
	fk.AddFault(fakekiwoom.Fault{API: "kt10000", ReturnCode: 20,
		Msg: "[2000](855056:매수증거금이 부족합니다. 6주 매수가능)"})
	f, err := buy(t, br, 9)
	if err != nil || f.Qty != 6 {
		t.Fatalf("%+v %v", f, err)
	}
}

// ★ 가장 위험한 모양 — 주문은 **들어갔는데** 응답만 502. 드라이버는 다시 쏘지 않고 "나갔을 수 있다" 로 올린다.
func TestAppliedButHTTP502IsMaybeSent(t *testing.T) {
	fk, br := setup(t, 10_000_000)
	fk.AddFault(fakekiwoom.Fault{API: "kt10000", HTTP: 502, Applied: true})
	_, err := buy(t, br, 3)
	if !errors.Is(err, broker.ErrMaybeSent) {
		t.Fatalf("%v", err)
	}
	st := fk.State()
	if len(st.Orders) != 1 || st.Holdings["005930"] != 3 {
		t.Fatalf("주문 %d 건 (1 이어야 — 재시도 금지), 보유 %v", len(st.Orders), st.Holdings)
	}
}

// 토큰 만료(8005) — 한 번 재발급하고 계속한다.
func TestTokenExpiryReissues(t *testing.T) {
	fk, br := setup(t, 10_000_000)
	if _, err := br.Cash(ctx); err != nil {
		t.Fatal(err)
	}
	fk.RotateToken()
	if _, err := br.Cash(ctx); err != nil {
		t.Fatalf("8005 뒤 재발급 실패: %v", err)
	}
	if n := fk.State().Calls["au10001"]; n != 2 {
		t.Fatalf("토큰 발급 %d 번", n)
	}
}

// TP 위임 — 지정가가 걸리고, 가격이 닿으면 체결되고, 주문번호로 체결을 확인한다.
func TestDelegatedTPFillsWhenPriceReaches(t *testing.T) {
	fk, br := setup(t, 10_000_000)
	if _, err := buy(t, br, 3); err != nil {
		t.Fatal(err)
	}
	id, err := br.PlaceTP(ctx, sam, 3, 292_300) // 호가단위로 올림 → 292,500
	if err != nil || id == "" {
		t.Fatalf("%q %v", id, err)
	}
	if st, _ := br.LimitStatus(ctx, sam, id); st.FilledQty != 0 {
		t.Fatalf("가격 전에 체결 %+v", st)
	}
	fk.SetPrice("005930", 293_000)
	st, err := br.LimitStatus(ctx, sam, id)
	if err != nil || st.FilledQty != 3 || st.AvgPrice != 292_500 {
		t.Fatalf("%+v %v", st, err)
	}
}

// 스펙에 없는 요청 필드는 거절된다 — 드라이버가 필드 이름을 틀리면 여기서 드러난다.
func TestUnknownRequestFieldRejected(t *testing.T) {
	spec, _ := fakekiwoom.LoadSpec("testdata/spec_subset.json")
	srv := httptest.NewServer(fakekiwoom.New(fakekiwoom.Config{Spec: spec, Prices: map[string]float64{"005930": 1}}))
	defer srv.Close()
	tok := post(t, srv.URL+"/oauth2/token", "", map[string]string{"grant_type": "client_credentials", "appkey": "a", "secretkey": "b"})["token"].(string)
	r := post(t, srv.URL+"/api/dostk/mrkcond", tok, map[string]string{"stk_cd": "005930", "stock_code": "x"})
	if r["return_code"].(float64) == 0 || !strings.Contains(r["return_msg"].(string), "stock_code") {
		t.Fatalf("%+v", r)
	}
}

func post(t *testing.T, url, tok string, body any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("api-id", "ka10007")
	if tok != "" {
		req.Header.Set("authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// 공식 스펙 전체 파일로도 뜬다 (최상위에 API 가 아닌 항목이 섞여 있다). 파일이 없으면 건너뛴다.
func TestLoadsFullSpecIfPresent(t *testing.T) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, "Downloads", "kiwoom-rest-api-spec.json")
	if _, err := os.Stat(p); err != nil {
		t.Skip("전체 스펙 없음")
	}
	if _, err := fakekiwoom.LoadSpec(p); err != nil {
		t.Fatal(err)
	}
}
