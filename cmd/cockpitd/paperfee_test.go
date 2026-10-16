package main

import (
	"testing"

	"github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/paper"
)

// 사용자 요율(books.json paper_fees)은 요율만 덮는다 — 업비트 최소 주문 규칙은 그대로, 0/0 도 그대로.
func TestApplyUserPaperFees(t *testing.T) {
	pcfg := paper.Config{Lot: 1, FeeBpBuy: 1.5, FeeBpSell: 21.5,
		Rules: map[string]paper.Rule{"UPBIT": {Lot: 1e-8, MinOrderValue: 5000, FeeBpBuy: 5, FeeBpSell: 5}}}
	applyUserPaperFees(&pcfg, map[string]book.Fee{"KRX": {Buy: 0, Sell: 20}, "UPBIT": {Buy: 0, Sell: 0}})

	k := pcfg.Rules["KRX"]
	if !k.FeeSet || k.FeeBpBuy != 0 || k.FeeBpSell != 20 || k.Lot != 1 {
		t.Fatalf("KRX: %+v", k)
	}
	u := pcfg.Rules["UPBIT"]
	if !u.FeeSet || u.FeeBpBuy != 0 || u.FeeBpSell != 0 || u.MinOrderValue != 5000 || u.Lot != 1e-8 {
		t.Fatalf("UPBIT: %+v", u)
	}
}
