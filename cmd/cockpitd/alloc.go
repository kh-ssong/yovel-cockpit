package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/alloc"
	"github.com/kh-ssong/yovel-cockpit/internal/book"
	"github.com/kh-ssong/yovel-cockpit/internal/engine"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
)

// 자동 배분 (docs/allocation.md) — 몫은 기동 때와 매일 08:50 에 계산해 그날 고정한다.

// applyAlloc — 오늘 몫을 계산해 엔진에 건다. 자동 배분이 꺼져 있으면 아무것도 안 한다.
// 날이 안 바뀌었으면 다시 계산하지 않는다 (장중 재계산 = 장중 크기 변경).
func applyAlloc(eng *engine.Engine, books *book.Set, now time.Time, log *slog.Logger) {
	if !books.Alloc().On() {
		return
	}
	if cur := eng.AllocPlan(); cur != nil && cur.Day == alloc.Day(now) {
		return
	}
	p := alloc.Compute(books, now)
	eng.SetAlloc(p)
	log.Info("자동 배분 — 오늘 몫 (하루 고정)", "day", p.Day, "capital", p.Capital,
		"night_cap", p.NightCap, "overnight_pct", p.OvernightPct, "borrow_x", p.BorrowX, "reserve_trades", p.ReserveTrades)
	for _, b := range p.Books {
		switch {
		case b.Fixed:
			log.Info("  고정 장부 (seed 대로)", "name", b.Name, "hold", b.Hold, "seed", b.Base)
		case !b.Enabled:
			log.Info("  꺼진 장부 (몫 없음)", "name", b.Name)
		default:
			log.Info("  장부 몫", "name", b.Name, "hold", b.Hold, "share", b.Share,
				"base", round0(b.Base), "cap", round0(b.Cap), "per_trade_at_w0.05", round0(b.Base*0.05))
		}
	}
}

func round0(v float64) float64 { return float64(int64(v + 0.5)) }

// allocReportAt — 하루 요약 시각 (KST). 장 마감 정산·밤 보유가 다 잡힌 뒤.
const allocReportHour, allocReportMin = 15, 40

// watchAllocReport — 매일 15:40 에 자본 효율 요약을 로그로 남긴다 (텔레그램이 켜져 있으면 같이).
func watchAllocReport(ctx context.Context, eng *engine.Engine, log *slog.Logger, send func(string)) {
	for {
		now := time.Now().In(session.KST)
		next := time.Date(now.Year(), now.Month(), now.Day(), allocReportHour, allocReportMin, 0, 0, session.KST)
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		r := eng.AllocReport(time.Now())
		if r == nil {
			continue
		}
		msg := allocSummary(r)
		log.Info(msg)
		if send != nil {
			send("⚖️ " + msg)
		}
	}
}

// allocSummary — 사람이 읽는 한 줄 요약.
func allocSummary(r *engine.AllocReport) string {
	t := r.Today
	var b strings.Builder
	fmt.Fprintf(&b, "자본 효율 %s — 낮 가동률 평균 %.0f%% · 최대 %.0f%%", t.Day, t.DayUtilAvg*100, t.DayUtilMax*100)
	if t.NightUtil >= 0 {
		fmt.Fprintf(&b, " · 밤 %.0f%%", t.NightUtil*100)
	}
	miss := alloc.Totals(t.Missed)
	if len(miss) == 0 {
		b.WriteString(" · 돈 때문에 놓친 신호 0")
	} else {
		b.WriteString(" · 놓친 신호")
		for _, k := range alloc.SortedBooks(miss) {
			fmt.Fprintf(&b, " %s %d건(%.0f원)", k, miss[k].N, miss[k].KRW)
		}
	}
	if sh := alloc.Totals(t.Shrunk); len(sh) > 0 {
		b.WriteString(" · 줄여 산 것")
		for _, k := range alloc.SortedBooks(sh) {
			fmt.Fprintf(&b, " %s %d건(−%.0f원)", k, sh[k].N, sh[k].KRW)
		}
	}
	if len(t.HoldMismatch) > 0 {
		fmt.Fprintf(&b, " · ★ hold 어긋남 %v", t.HoldMismatch)
	}
	return b.String()
}
