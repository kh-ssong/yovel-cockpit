package lots

import (
	"testing"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

var (
	now = time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC)
	sam = protocol.Symbol{Exchange: "KRX", Code: "005930"}
	skh = protocol.Symbol{Exchange: "KRX", Code: "000660"}
)

func at(min int) *time.Time { t := now.Add(-time.Duration(min) * time.Minute); return &t }

// 사용자가 든 예: 삼성전자 로트 셋 — D-205 두 번(분할매수) + otto 스윙 하나.
func positions() []protocol.Position {
	return []protocol.Position{
		{IntentID: "A1", Group: "d205-005930", Kid: "flat6-1", Scope: "intraday/d205", Symbol: sam,
			Qty: 10, EntryQty: 10, AvgEntryPrice: 71000, EntryAt: at(30), StopArmed: 60350},
		{IntentID: "A2", Group: "d205-005930", Kid: "flat6-1", Scope: "intraday/d205", Symbol: sam,
			Qty: 5, EntryQty: 5, AvgEntryPrice: 72500, EntryAt: at(10), StopArmed: 61600},
		{IntentID: "B1", Kid: "otto-1", Scope: "swing/rsi", Symbol: sam,
			Qty: 10, EntryQty: 20, AvgEntryPrice: 70000, EntryAt: at(600), TpArmed: 77000},
	}
}

func bookOf(kid, scope string) string {
	return map[string]string{"intraday/d205": "d205", "swing/rsi": "rsi"}[scope]
}

func TestLotsKeepEachEntrySeparate(t *testing.T) {
	ls, gs := Build(Inputs{
		Now: now, Positions: positions(), BookOf: bookOf,
		Price:    func(protocol.Symbol) (float64, bool) { return 73000, true },
		Realized: map[string]float64{"B1": 50000}, // B1 은 절반을 이미 팔았다
	})
	if len(ls) != 3 {
		t.Fatalf("%d 로트", len(ls))
	}
	byID := map[string]Lot{}
	for _, l := range ls {
		byID[l.IntentID] = l
	}
	// ★ 로트마다 제 진입가로 평가한다 — HTS 합산 평단으로 섞지 않는다.
	if byID["A1"].UnrealizedKRW != 20000 || byID["A2"].UnrealizedKRW != 2500 || byID["B1"].UnrealizedKRW != 30000 {
		t.Fatalf("로트 평가 %+v", byID)
	}
	if byID["B1"].Book != "rsi" || byID["B1"].RealizedKRW != 50000 || byID["A1"].HeldSec != 1800 {
		t.Fatalf("%+v", byID["B1"])
	}
	// 분할매수 두 로트는 한 포지션(group)으로도 보인다.
	if len(gs) != 1 || gs[0].Lots != 2 || gs[0].Qty != 15 || gs[0].UnrealizedKRW != 22500 {
		t.Fatalf("group %+v", gs)
	}
	if want := (10*71000.0 + 5*72500) / 15; gs[0].AvgEntryPrice != want {
		t.Fatalf("group 평단 %v, 기대 %v", gs[0].AvgEntryPrice, want)
	}
}

// 가격을 모르면 평가손익을 비운다 — 0 원 평가는 전액 손실처럼 보인다.
func TestUnknownPriceLeavesUnrealizedEmpty(t *testing.T) {
	ls, gs := Build(Inputs{Now: now, Positions: positions()})
	for _, l := range ls {
		if l.Price != 0 || l.UnrealizedKRW != 0 || l.CostKRW == 0 {
			t.Fatalf("%+v", l)
		}
	}
	if !gs[0].PriceUnknown {
		t.Fatal("group 이 가격 미상을 표시하지 않았다")
	}
}

func TestReconcileAgainstBroker(t *testing.T) {
	ls, _ := Build(Inputs{Now: now, Positions: append(positions(),
		protocol.Position{IntentID: "C1", Scope: "intraday/d205", Symbol: skh, Qty: 3, AvgEntryPrice: 200000})})
	hs := Reconcile(ls, []broker.Holding{
		{Symbol: sam, Qty: 40, AvgPrice: 70786}, // 로트 25 + 직접 산 15
		{Symbol: skh, Qty: 1, AvgPrice: 200000}, // 로트는 3 인데 실물 1 — 콕핏이 모르는 매도
		{Symbol: protocol.Symbol{Exchange: "KRX", Code: "035720"}, Qty: 7},
	}, func(protocol.Symbol) float64 { return 1 })

	if len(hs) != 3 {
		t.Fatalf("%+v", hs)
	}
	// ★ 위험한 것(short)이 맨 앞.
	if hs[0].Symbol != skh || hs[0].Status != StatusShort || hs[0].OffBook != -2 {
		t.Fatalf("short %+v", hs[0])
	}
	for _, h := range hs[1:] {
		switch h.Symbol.Code {
		case "005930":
			if h.Status != StatusExternal || h.LotsQty != 25 || h.OffBook != 15 || len(h.Lots) != 3 || h.BrokerAvg != 70786 {
				t.Fatalf("삼성 %+v", h)
			}
		case "035720":
			if h.Status != StatusExternal || h.LotsQty != 0 {
				t.Fatalf("장부 밖 종목 %+v", h)
			}
		}
	}
}
