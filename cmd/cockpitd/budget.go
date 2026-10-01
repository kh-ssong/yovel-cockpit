package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// budgetCheckAt — 매일 점검 시각 (현지). 장 전이라 D-205 보유가 없고, 해외 원화배정 같은
// 밤사이 변화가 이미 반영된 뒤다.
const budgetCheckHour, budgetCheckMin = 8, 50

// budgetVerdict — 예산을 계좌가 감당하는가.
//
// ★ 비교 대상은 Seed(L1 현금 한도)다. Orderable 은 보유 증거금이 영구히 깎아 먹어 과소평가하고
// (reflex 실측 −49%), Deposit 은 해외 원화배정으로 **빠져나간 돈**을 못 보여준다
// (reflex 2026-08-04: 배정 137만 → 매수 79% 실패, 국내 API 에선 흔적이 안 보였다).
//
// ★ 이미 들고 있는 자리(open)는 현금이 아니라 주식으로 있으므로 예산에서 빼고 본다.
// 남은 예산 = budget − open 이 Seed 보다 크면, 뒤에 오는 진입이 전부 E_CAPITAL·거부가 된다.
func budgetVerdict(budget, open float64, c broker.Cash) (ok bool, msg string) {
	if budget <= 0 {
		return true, ""
	}
	if c.Seed <= 0 {
		return true, "브로커가 현금 한도(L1)를 주지 않는다 — 예산 점검 불가"
	}
	need := budget - open
	if need <= c.Seed {
		return true, ""
	}
	return false, fmt.Sprintf("예산이 계좌를 넘는다: 남은 예산 %.0f원(예산 %.0f − 보유 %.0f) > 현금 한도 %.0f원 "+
		"(%.0f%% 만 집행 가능 — 뒤 순위 진입은 거부된다. 해외 원화배정·출금·다른 봇 사용을 확인할 것)",
		need, budget, open, c.Seed, c.Seed/need*100)
}

// nextBudgetCheck — now 이후 첫 점검 시각.
func nextBudgetCheck(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), budgetCheckHour, budgetCheckMin, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// watchBudget — 기동 때 한 번, 그 뒤 매일 08:50 에 예산을 계좌와 대조한다.
//
// ★ 막지 않고 **크게 경고만** 한다. 줄여서 집행할지는 사람이 정할 일이고, 조용히 예산을
// 깎으면 "왜 이 크기로 샀는지" 를 원장에서 재현할 수 없게 된다.
func watchBudget(ctx context.Context, br broker.Broker, budget float64,
	positions func() []protocol.Position, log *slog.Logger, alert func(key, text string)) {

	check := func() {
		c, err := br.Cash(ctx)
		if err != nil {
			log.Warn("예산 점검: 잔고 조회 실패", "err", err)
			return
		}
		open := 0.0
		for _, p := range positions() {
			open += p.Qty * p.AvgEntryPrice
		}
		ok, msg := budgetVerdict(budget, open, c)
		switch {
		case !ok:
			log.Warn("★★ "+msg, "engine_budget", budget, "seed", c.Seed,
				"orderable", c.Orderable, "deposit", c.Deposit)
			alert("budget", "💰 "+msg)
		case msg != "":
			log.Info(msg, "broker", br.Name())
		default:
			log.Info("예산 점검 통과", "engine_budget", budget, "open", open, "seed", c.Seed)
		}
	}

	check()
	for {
		t := time.NewTimer(time.Until(nextBudgetCheck(time.Now())))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			check()
		}
	}
}
