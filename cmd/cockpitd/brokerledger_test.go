package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker/kiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

func page(s string) []json.RawMessage { return []json.RawMessage{json.RawMessage(s)} }

// 10/23 실측 모양 — 주문별 수수료는 10원 미만을 잘라 0 으로 오고, 하루 합(ka10074)은 더 크다.
func fakeLedger() kiwoom.Ledger {
	return kiwoom.Ledger{Date: "20261023", Sections: []kiwoom.LedgerSection{
		{API: "ka10074", Pages: page(`{"return_code":0,"trde_cmsn":"40","trde_tax":"46","rlzt_pl":"-4641"}`)},
		{API: "ka10076", Pages: page(`{"return_code":0,"cntr":[
			{"ord_no":"0585361","stk_cd":"293490","io_tp_nm":"-매도","cntr_qty":"2","cntr_pric":"11580","tdy_trde_cmsn":"0","tdy_trde_tax":"46"},
			{"ord_no":"0094019","stk_cd":"293490","io_tp_nm":"+매수","cntr_qty":"2","cntr_pric":"11770","tdy_trde_cmsn":"0","tdy_trde_tax":"0"},
			{"ord_no":"0100001","stk_cd":"005930","io_tp_nm":"+매수","cntr_qty":"3","cntr_pric":"70000","tdy_trde_cmsn":"30","tdy_trde_tax":"0"},
			{"ord_no":"0200002","stk_cd":"000660","io_tp_nm":"-매도","cntr_qty":"1","cntr_pric":"200000","tdy_trde_cmsn":"30","tdy_trde_tax":"0"},
			{"ord_no":"0300003","stk_cd":"000660","io_tp_nm":"+매수","cntr_qty":"0","cntr_pric":"0","tdy_trde_cmsn":"0","tdy_trde_tax":"0"}]}`)},
	}}
}

func row(id, phase, side, code string, qty, px, fee float64, at time.Time) store.Order {
	return store.Order{ID: id + phase, Phase: phase, Side: side, BrokerOrderID: id,
		Symbol: protocol.Symbol{Exchange: "KRX", Code: code}, Qty: qty, Price: px, FeeKRW: fee,
		FilledAt: &at, CreatedAt: at, Mode: protocol.ModeLive}
}

func TestCheckLedgerMatchesByOrderNo(t *testing.T) {
	at := time.Date(2026, 10, 23, 10, 0, 0, 0, session.KST)
	orders := []store.Order{
		row("0585361", "exit_filled", "sell", "293490", 2, 11580, 46, at),
		// 부분체결로 두 줄에 나눠 적힌 매수 — 주문번호로 합쳐 본다.
		row("0094019", "filled", "buy", "293490", 1, 11760, 0, at),
		row("0094019", "filled", "buy", "293490", 1, 11780, 0, at.Add(time.Second)),
		row("0100001", "filled", "buy", "005930", 2, 70000, 30, at),                    // 수량이 다르다
		row("0400004", "filled", "buy", "035420", 1, 150000, 20, at),                   // 증권사에 없다
		row("0500005", "rejected", "buy", "035420", 0, 0, 0, at),                       // 대조 대상 아님
		row("0600006", "exit_filled", "sell", "035420", 1, 1, 0, at.AddDate(0, 0, -1)), // 다른 날
	}
	c := checkLedger("20261023", orders, fakeLedger())

	if c.OrdersFrom != "ka10076" || c.CockpitOrders != 4 || c.BrokerOrders != 4 {
		t.Fatalf("from %s · 콕핏 %d · 증권사 %d", c.OrdersFrom, c.CockpitOrders, c.BrokerOrders)
	}
	if c.Matched != 2 {
		t.Fatalf("일치 %d (부분체결 두 줄을 합쳐 봐야 한다) %+v", c.Matched, c)
	}
	if len(c.Mismatch) != 1 || !strings.Contains(c.Mismatch[0], "수량 2≠3") {
		t.Fatalf("불일치 %v", c.Mismatch)
	}
	if len(c.CockpitOnly) != 1 || !strings.HasPrefix(c.CockpitOnly[0], "0400004") {
		t.Fatalf("콕핏에만 %v", c.CockpitOnly)
	}
	if len(c.BrokerOnly) != 1 || !strings.HasPrefix(c.BrokerOnly[0], "0200002 000660 sell") {
		t.Fatalf("장부 밖 %v (체결 0 인 주문은 빼야 한다)", c.BrokerOnly)
	}
	if c.CockpitFeeKRW != 96 || c.BrokerFeeKRW == nil || *c.BrokerFeeKRW != 86 || *c.FeeGapKRW != -10 {
		t.Fatalf("수수료 콕핏 %v 증권사 %v 차 %v", c.CockpitFeeKRW, deref(c.BrokerFeeKRW), deref(c.FeeGapKRW))
	}
	if *c.BrokerRealizedKRW != -4641 {
		t.Fatalf("실현 %v", *c.BrokerRealizedKRW)
	}
	if c.clean() {
		t.Fatal("어긋났는데 깨끗하다고 한다")
	}
	msg := ledgerSummary("20261023", c, "/x.json")
	for _, want := range []string{"⚠️", "10/23", "불일치 1", "콕핏에만 1", "장부 밖 1", "0400004", "/x.json"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("알림에 %q 가 없다:\n%s", want, msg)
		}
	}
}

func TestLedgerSummaryClean(t *testing.T) {
	fee, gap, pl := 1576.0, 280.0, -4641.0
	c := ledgerCheck{OrdersFrom: "ka10076", Matched: 40, CockpitFeeKRW: 1296,
		BrokerFeeKRW: &fee, FeeGapKRW: &gap, BrokerRealizedKRW: &pl}
	got := ledgerSummary("20261023", c, "/x.json")
	want := "🧾 증권사 원장 10/23 · 주문 40건 일치 · 수수료+세금 증권사 1,576 / 콕핏 1,296 (+280) · 실현 -4,641원"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

type fakeSource struct {
	l kiwoom.Ledger
	n int
}

func (f *fakeSource) Ledger(context.Context, time.Time) kiwoom.Ledger { f.n++; return f.l }

// 파일은 키움 응답 그대로 + check. 조회가 전부 실패하면 파일을 쓰지 않는다.
func TestRunBrokerLedgerWritesFile(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Date(2026, 10, 23, 10, 0, 0, 0, session.KST)
	if err := st.RecordOrder(context.Background(), row("0585361", "exit_filled", "sell", "293490", 2, 11580, 46, at)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "broker", "broker_ledger_20261023.json")
	now := time.Date(2026, 10, 23, 15, 50, 0, 0, session.KST)

	if _, err := runBrokerLedger(context.Background(), &fakeSource{l: kiwoom.Ledger{Date: "20261023",
		Sections: []kiwoom.LedgerSection{{API: "ka10074", Error: "거부"}}}}, st, now, path); err == nil {
		t.Fatal("전부 실패했는데 오류가 없다")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("전부 실패했는데 파일을 썼다")
	}

	c, err := runBrokerLedger(context.Background(), &fakeSource{l: fakeLedger()}, st, now, path)
	if err != nil {
		t.Fatal(err)
	}
	if c.CockpitOrders != 1 || c.Matched != 1 {
		t.Fatalf("%+v", c)
	}
	if !ledgerChecked(path) {
		t.Fatal("데몬이 쓴 파일인데 check 가 없다")
	}
	var f map[string]json.RawMessage
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"date", "sections", "check"} {
		if _, ok := f[k]; !ok {
			t.Fatalf("파일에 %s 가 없다", k)
		}
	}
}
