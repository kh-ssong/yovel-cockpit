package kiwoom

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 연속조회 — 응답 헤더 cont-yn=Y 면 next-key 를 실어 다음 쪽을 묻고, 쪽마다 본문을 그대로 남긴다.
// 조회 하나가 실패해도 나머지는 계속한다. 지난 날짜면 당일 전용(ka10076)은 묻지 않는다.
func TestLedgerFollowsPagesAndKeepsGoing(t *testing.T) {
	var mu sync.Mutex
	asked := map[string][]string{} // api-id → 받은 next-key 들
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := r.Header.Get("api-id")
		mu.Lock()
		asked[api] = append(asked[api], r.Header.Get("next-key"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case api == "ka10170" && r.Header.Get("next-key") == "":
			w.Header().Set("cont-yn", "Y")
			w.Header().Set("next-key", "K2")
			w.Write([]byte(`{"return_code":0,"tdy_trde_diary":[{"stk_cd":"005930","sell_qty":"1"}]}`))
		case api == "ka10170":
			w.Write([]byte(`{"return_code":0,"tdy_trde_diary":[{"stk_cd":"000660","sell_qty":"2"}]}`))
		case api == "kt00015":
			w.Write([]byte(`{"return_code":1,"return_msg":"조회 오류"}`))
		default:
			w.Write([]byte(`{"return_code":0}`))
		}
	}))
	defer srv.Close()

	tok := t.TempDir() + "/kiwoom_token.json"
	os.WriteFile(tok, []byte(`{"token":"daemon","expires_dt":"20261023235959"}`), 0o600)
	now := time.Date(2026, 10, 23, 16, 0, 0, 0, kst)
	b, err := New(Config{AppKey: "k", SecretKey: "s", DataDir: t.TempDir(), TokenFile: tok,
		NoTokenIssue: true, APIURL: srv.URL, HTTP: srv.Client(),
		Now: func() time.Time { return now }, Sleep: func(time.Duration) {}})
	if err != nil {
		t.Fatal(err)
	}

	l := b.Ledger(ctx, now)
	if l.Date != "20261023" || !l.OK() {
		t.Fatalf("date %s ok %v", l.Date, l.OK())
	}
	got := map[string]LedgerSection{}
	for _, s := range l.Sections {
		got[s.API] = s
	}
	if n := len(got["ka10170"].Pages); n != 2 {
		t.Fatalf("ka10170 %d쪽 — 연속조회를 안 따라갔다", n)
	}
	if !strings.Contains(string(got["ka10170"].Pages[1]), "000660") {
		t.Fatalf("둘째 쪽 본문이 그대로가 아니다: %s", got["ka10170"].Pages[1])
	}
	if keys := asked["ka10170"]; len(keys) != 2 || keys[1] != "K2" {
		t.Fatalf("next-key 를 실어 묻지 않았다: %v", keys)
	}
	if got["kt00015"].Error == "" {
		t.Fatal("거부된 조회가 오류로 안 남았다")
	}
	if _, ok := got["ka10076"]; !ok {
		t.Fatal("오늘이면 체결(ka10076)도 묻는다")
	}
	var req map[string]string
	b2, _ := json.Marshal(got["ka10074"].Request)
	json.Unmarshal(b2, &req)
	if req["strt_dt"] != "20261023" || req["end_dt"] != "20261023" {
		t.Fatalf("ka10074 요청 %v", req)
	}

	past := b.Ledger(ctx, now.AddDate(0, 0, -1))
	for _, s := range past.Sections {
		if s.API == "ka10076" {
			t.Fatal("지난 날짜에 당일 전용 체결 조회를 했다")
		}
	}
}

// 발급 금지 모드 — 파일 토큰이 살아 있으면 갱신 여유 안이어도 쓰고, 무효(8005)면 발급하지 않고 멈춘다.
func TestNoTokenIssueNeverIssues(t *testing.T) {
	f := newFake()
	var invalid atomic.Bool
	f.on(apiQuote, func(map[string]any) any {
		if invalid.Load() {
			return map[string]any{"return_code": 3, "return_msg": "[8005:Token이 유효하지 않습니다]"}
		}
		return map[string]any{"return_code": 0, "cur_prc": "1000"}
	})
	srv := f.start(t)
	cl := &clock{t: time.Date(2026, 8, 16, 23, 30, 0, 0, kst)} // 만료 30분 전 = 갱신 여유 안
	path := t.TempDir() + "/kiwoom_token.json"
	os.WriteFile(path, []byte(`{"token":"daemon","expires_dt":"20260816235959"}`), 0o600)
	b, err := New(Config{AppKey: "k", SecretKey: "s", DataDir: t.TempDir(), TokenFile: path,
		NoTokenIssue: true, APIURL: srv.URL, HTTP: srv.Client(), Now: cl.now,
		Sleep: func(time.Duration) {}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Quote(ctx, sym); err != nil {
		t.Fatalf("살아 있는 데몬 토큰을 못 썼다: %v", err)
	}
	invalid.Store(true)
	if _, err := b.Quote(ctx, sym); !errors.Is(err, ErrTokenIssueOff) {
		t.Fatalf("무효 토큰에서 멈추지 않았다: %v", err)
	}
	cl.advance(time.Hour) // 만료 뒤
	if _, err := b.Quote(ctx, sym); !errors.Is(err, ErrTokenIssueOff) {
		t.Fatalf("만료 토큰에서 멈추지 않았다: %v", err)
	}
	if f.tokens != 0 {
		t.Fatalf("★ 발급 금지인데 %d회 발급했다 — 데몬 토큰이 죽는다", f.tokens)
	}
}

// 지난 날짜는 ka10076 이 없어 kt00007 로 읽는다 — "A" 프리픽스·앞자리 0 · 주문별 수수료 없음.
func TestLedgerOrdersFallBackToKt00007(t *testing.T) {
	l := Ledger{Sections: []LedgerSection{
		{API: "ka10076", Error: "거부"},
		{API: "kt00007", Pages: []json.RawMessage{
			json.RawMessage(`{"acnt_ord_cntr_prps_dtl":[{"ord_no":"0070959","stk_cd":"A225190","io_tp_nm":"현금매수 K","cntr_qty":"0000000023","cntr_uv":"0000004025"}]}`),
			json.RawMessage(`{"acnt_ord_cntr_prps_dtl":[{"ord_no":"0487561","stk_cd":"A069500","io_tp_nm":"현금매도","cntr_qty":"0000000003","cntr_uv":"0000086177"},
				{"ord_no":"0487999","stk_cd":"A069500","io_tp_nm":"현금매도","cntr_qty":"0000000000","cntr_uv":"0000000000"}]}`)}},
		{API: "ka10074", Pages: []json.RawMessage{json.RawMessage(`{"trde_cmsn":"470","trde_tax":"21","rlzt_pl":"-1616"}`)}},
	}}
	got, from, withFee := l.Orders()
	if from != "kt00007" || withFee || len(got) != 2 {
		t.Fatalf("from %s fee %v %+v", from, withFee, got)
	}
	if got[0] != (LedgerOrder{OrderID: "0070959", Code: "225190", Side: "buy", Qty: 23, Price: 4025}) ||
		got[1].Side != "sell" || got[1].Qty != 3 || got[1].Price != 86177 {
		t.Fatalf("%+v", got)
	}
	fee, pl, ok := l.DayTotals()
	if !ok || fee != 491 || pl != -1616 {
		t.Fatalf("하루 합 %v %v %v", fee, pl, ok)
	}
}
