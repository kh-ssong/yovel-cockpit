package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// lossPrefix — 일일 손실 한도가 건 서킷브레이커의 사유 머리. 다른 사유(de-risk 등)로 걸린 건 건드리지 않는다.
const lossPrefix = "일일 손실 한도 "

type lossDecision struct {
	trip, reset bool
	reason      string
}

// decideLoss — 오늘(KST) 실현손익과 지금 브레이커 상태로 무엇을 할지. 순수 함수.
//
// ★ 신규 진입만 막는다 — 청산은 계속한다 (막으면 손실이 더 커질 수 있다).
// ★ 다음 거래일에 저절로 풀린다 (사유에 날짜가 박혀 있다). 재시작해도 유지된다 (가드 영속).
func decideLoss(limit, realized float64, today string, on bool, reason string) lossDecision {
	mine := strings.HasPrefix(reason, lossPrefix)
	switch {
	case on && mine && (limit <= 0 || !strings.HasPrefix(reason, lossPrefix+today)):
		return lossDecision{reset: true, reason: "일일 손실 한도 해제 (새 거래일 또는 한도 꺼짐)"}
	case !on && limit > 0 && realized <= -limit:
		return lossDecision{trip: true, reason: fmt.Sprintf("%s%s: 오늘 실현 %.0f원 ≤ −%.0f원", lossPrefix, today, realized, limit)}
	}
	return lossDecision{}
}

// watchLoss — 30초마다 오늘 실현손익을 원장에서 다시 계산해 한도와 비교한다.
func watchLoss(ctx context.Context, st *store.Store, eng *engine.Engine, mode protocol.Mode, limit float64,
	log *slog.Logger, alert func(key, text string)) {
	check := func() {
		now := time.Now().In(session.KST)
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, session.KST)
		realized, losses, err := st.DayRealized(ctx, mode, dayStart)
		if err != nil {
			log.Warn("일일 손실 계산 실패", "err", err)
			return
		}
		on, reason := eng.CircuitBreaker()
		d := decideLoss(limit, realized, now.Format("2006-01-02"), on, reason)
		switch {
		case d.trip:
			eng.SetCircuitBreaker(true, d.reason)
			log.Warn("★ "+d.reason+" — 신규 진입 중지 (청산은 계속)", "losses", losses)
			alert("loss-limit", "🛑 "+d.reason+fmt.Sprintf(" (손실 %d건) — 콕핏은 계속 돈다. 신규 진입만 멈췄고 청산은 계속한다", losses))
		case d.reset:
			eng.SetCircuitBreaker(false, d.reason)
			log.Info(d.reason)
			alert("loss-limit-reset", "✅ "+d.reason+" — 신규 진입 재개")
		}
	}
	check()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
