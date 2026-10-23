package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker/kiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/notify"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
	"github.com/kh-ssong/yovel-cockpit/internal/session"
	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// ── 증권사 원장 대조 ─────────────────────────────────────────────────────────
//
// 장 뒤 하루 한 번 (거래일 15:50 KST) 키움 원장을 받아 이 콕핏의 live 원장과 주문번호로 맞대 보고
// <data-dir>/broker/broker_ledger_<YYYYMMDD>.json 으로 남긴다 (user 2026-10-23 «원장 기록할 때 API 로
// 실제 매매내역 확인»). docs/ops.md «증권사 원장».
//
// ★ 콕핏 원장(order 행)은 고치지 않는다 — 체결 직후 키움 체결조회(ka10076)로 적은 그대로 둔다.
//   다른 건 수수료뿐이다: 주문별 수수료는 주문마다 10원 미만을 잘라 오고 실제 청구는 하루 합에 매긴다.
//   그 하루 합(ka10074)과 차이를 이 파일에 따로 남기고, 회고는 그 숫자를 쓴다.

// brokerLedgerAt — 장 마감(15:30) 단일가 체결까지 원장에 들어온 뒤.
const brokerLedgerAt = 15*time.Hour + 50*time.Minute

// brokerLedgerTries — 하루 시도 상한 (조회가 전부 실패할 때만 다시 한다 · 사이 10분).
const brokerLedgerTries = 3

type brokerLedgerFile struct {
	kiwoom.Ledger
	Check ledgerCheck `json:"check"`
}

// ledgerCheck — 콕핏 원장 ↔ 증권사 원장. 주문 수는 주문번호 기준.
type ledgerCheck struct {
	OrdersFrom    string   `json:"orders_from"` // ka10076 (오늘 · 주문별 수수료 있음) | kt00007 | "" (조회 실패)
	CockpitOrders int      `json:"cockpit_orders"`
	BrokerOrders  int      `json:"broker_orders"`
	Matched       int      `json:"matched"`
	Mismatch      []string `json:"mismatch,omitempty"`     // 같은 주문번호인데 종목·수량·가격·수수료가 다르다
	CockpitOnly   []string `json:"cockpit_only,omitempty"` // 콕핏엔 있는데 증권사엔 없다
	BrokerOnly    []string `json:"broker_only,omitempty"`  // 증권사엔 있는데 콕핏 원장 밖이다 (HTS 손주문 등)
	CockpitFeeKRW float64  `json:"cockpit_fee_krw"`
	// BrokerFeeKRW — ka10074 수수료+세금 = 실제 청구. FeeGapKRW = 증권사 − 콕핏.
	BrokerFeeKRW      *float64 `json:"broker_fee_krw,omitempty"`
	FeeGapKRW         *float64 `json:"fee_gap_krw,omitempty"`
	BrokerRealizedKRW *float64 `json:"broker_realized_krw,omitempty"` // ka10074 rlzt_pl (수수료·세금 뺀 뒤)
}

func (c ledgerCheck) clean() bool {
	return c.OrdersFrom != "" && len(c.Mismatch)+len(c.CockpitOnly)+len(c.BrokerOnly) == 0
}

// checkLedger — 순수 함수. day = YYYYMMDD (한국 날짜). orders = 그날 언저리의 live 원장 행.
func checkLedger(day string, orders []store.Order, l kiwoom.Ledger) ledgerCheck {
	type agg struct {
		code, side    string
		qty, amt, fee float64
	}
	mine := map[string]*agg{}
	for _, o := range orders {
		if (o.Phase != "filled" && o.Phase != "exit_filled") || o.Qty <= 0 || o.BrokerOrderID == "" {
			continue
		}
		at := o.CreatedAt
		if o.FilledAt != nil {
			at = *o.FilledAt
		}
		if at.In(session.KST).Format("20060102") != day {
			continue
		}
		a := mine[o.BrokerOrderID]
		if a == nil {
			a = &agg{code: o.Symbol.Code, side: o.Side}
			mine[o.BrokerOrderID] = a
		}
		a.qty += o.Qty
		a.amt += o.Qty * o.Price
		a.fee += o.FeeKRW
	}

	var c ledgerCheck
	ids := make([]string, 0, len(mine))
	for id, a := range mine {
		ids = append(ids, id)
		c.CockpitFeeKRW += a.fee
	}
	sort.Strings(ids)
	c.CockpitOrders = len(ids)
	if fee, pl, ok := l.DayTotals(); ok {
		gap := fee - c.CockpitFeeKRW
		c.BrokerFeeKRW, c.FeeGapKRW, c.BrokerRealizedKRW = &fee, &gap, &pl
	}

	theirs, from, withFee := l.Orders()
	c.OrdersFrom = from
	if from == "" {
		return c
	}
	c.BrokerOrders = len(theirs)
	byID := map[string]kiwoom.LedgerOrder{}
	for _, b := range theirs {
		byID[b.OrderID] = b
		if _, ok := mine[b.OrderID]; !ok {
			c.BrokerOnly = append(c.BrokerOnly, fmt.Sprintf("%s %s %s %g@%.0f", b.OrderID, b.Code, b.Side, b.Qty, b.Price))
		}
	}
	for _, id := range ids {
		a := mine[id]
		b, ok := byID[id]
		if !ok {
			c.CockpitOnly = append(c.CockpitOnly, fmt.Sprintf("%s %s %s %g", id, a.code, a.side, a.qty))
			continue
		}
		var d []string
		if b.Code != a.code || b.Side != a.side {
			d = append(d, fmt.Sprintf("종목 %s/%s≠%s/%s", a.code, a.side, b.Code, b.Side))
		}
		if math.Abs(b.Qty-a.qty) > 1e-9 {
			d = append(d, fmt.Sprintf("수량 %g≠%g", a.qty, b.Qty))
		}
		if px := a.amt / a.qty; math.Abs(b.Price-px) > 0.5 {
			d = append(d, fmt.Sprintf("가격 %.2f≠%.2f", px, b.Price))
		}
		if withFee && math.Abs(b.FeeKRW-a.fee) > 0.5 {
			d = append(d, fmt.Sprintf("수수료 %.0f≠%.0f", a.fee, b.FeeKRW))
		}
		if len(d) > 0 {
			c.Mismatch = append(c.Mismatch, id+" "+a.code+" "+strings.Join(d, " · "))
			continue
		}
		c.Matched++
	}
	return c
}

// ledgerSummary — 텔레그램 한 줄. 깨끗하면 숫자만, 아니면 무엇이 어긋났는지 앞의 몇 건.
func ledgerSummary(day string, c ledgerCheck, path string) string {
	d := day[4:6] + "/" + day[6:]
	fee := ""
	if c.BrokerFeeKRW != nil {
		fee = fmt.Sprintf(" · 수수료+세금 증권사 %s / 콕핏 %s (%+.0f)", notify.KRW(*c.BrokerFeeKRW),
			notify.KRW(c.CockpitFeeKRW), *c.FeeGapKRW)
	}
	pl := ""
	if c.BrokerRealizedKRW != nil {
		pl = " · 실현 " + notify.KRW(*c.BrokerRealizedKRW) + "원"
	}
	if c.clean() {
		return fmt.Sprintf("🧾 증권사 원장 %s · 주문 %d건 일치%s%s", d, c.Matched, fee, pl)
	}
	if c.OrdersFrom == "" {
		return fmt.Sprintf("⚠️ 증권사 원장 %s · 주문 체결 조회 실패 — 대조 못 함%s%s (%s)", d, fee, pl, path)
	}
	s := fmt.Sprintf("⚠️ 증권사 원장 %s · 일치 %d · 불일치 %d · 콕핏에만 %d · 장부 밖 %d%s%s",
		d, c.Matched, len(c.Mismatch), len(c.CockpitOnly), len(c.BrokerOnly), fee, pl)
	for _, l := range append(append(append([]string{}, c.Mismatch...), c.CockpitOnly...), c.BrokerOnly...) {
		if strings.Count(s, "\n") >= 5 {
			s += "\n  ↳ …"
			break
		}
		s += "\n  ↳ " + l
	}
	return s + "\n  (" + path + ")"
}

// deref — 로그에 포인터 주소 대신 값 (없으면 nil).
func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

type ledgerSource interface {
	Ledger(ctx context.Context, day time.Time) kiwoom.Ledger
}

// runBrokerLedger — 그날 원장을 받아 대조하고 파일로 쓴다. 조회가 전부 실패하면 쓰지 않는다.
func runBrokerLedger(ctx context.Context, src ledgerSource, st *store.Store, now time.Time,
	path string) (ledgerCheck, error) {
	l := src.Ledger(ctx, now)
	if !l.OK() {
		var errs []string
		for _, s := range l.Sections {
			errs = append(errs, s.API+": "+s.Error)
		}
		return ledgerCheck{}, fmt.Errorf("증권사 원장 조회가 전부 실패: %s", strings.Join(errs, " | "))
	}
	k := now.In(session.KST)
	since := time.Date(k.Year(), k.Month(), k.Day(), 0, 0, 0, 0, session.KST)
	orders, err := st.Ledger(ctx, store.LedgerQuery{Mode: protocol.ModeLive, Since: &since, Limit: 5000})
	if err != nil {
		return ledgerCheck{}, fmt.Errorf("콕핏 원장: %w", err)
	}
	c := checkLedger(l.Date, orders, l)
	b, err := json.MarshalIndent(brokerLedgerFile{Ledger: l, Check: c}, "", "  ")
	if err != nil {
		return c, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return c, err
	}
	// 읽는 쪽(mac-sync)이 반쯤 쓴 파일을 집어 가지 않게 바꿔 끼운다.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return c, err
	}
	return c, os.Rename(tmp, path)
}

// ledgerChecked — 데몬이 이미 대조까지 해서 쓴 파일이 있다 (brokerctl 로 손으로 뽑은 파일은 check 가 없다).
func ledgerChecked(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var f struct {
		Check *json.RawMessage `json:"check"`
	}
	return json.Unmarshal(b, &f) == nil && f.Check != nil
}

// watchBrokerLedger — 거래일 15:50 이 지나면 그날 것을 한 번. 그 뒤에 켜져도 (자정 전이면) 따라잡는다.
func watchBrokerLedger(ctx context.Context, src ledgerSource, st *store.Store, cal *session.Calendar,
	dir string, log *slog.Logger, send func(string)) {
	tries := map[string]int{}
	var last time.Time
	tick := func() {
		now := time.Now().In(session.KST)
		day := now.Format("20060102")
		mid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, session.KST)
		if !cal.TradingDay(now) || now.Sub(mid) < brokerLedgerAt || tries[day] >= brokerLedgerTries ||
			(tries[day] > 0 && now.Sub(last) < 10*time.Minute) {
			return
		}
		path := filepath.Join(dir, "broker_ledger_"+day+".json")
		if ledgerChecked(path) {
			tries[day] = brokerLedgerTries
			return
		}
		tries[day]++
		last = now
		c, err := runBrokerLedger(ctx, src, st, now, path)
		if err != nil {
			log.Warn("증권사 원장 대조 실패", "day", day, "try", tries[day], "err", err)
			if tries[day] >= brokerLedgerTries {
				send(fmt.Sprintf("⚠️ 증권사 원장 %s/%s · %d번 다 실패 — 오늘 대조 없음: %v", day[4:6], day[6:], tries[day], err))
			}
			return
		}
		tries[day] = brokerLedgerTries
		log.Info("증권사 원장 대조", "day", day, "path", path, "orders_from", c.OrdersFrom,
			"cockpit", c.CockpitOrders, "broker", c.BrokerOrders, "matched", c.Matched,
			"mismatch", len(c.Mismatch), "cockpit_only", len(c.CockpitOnly), "broker_only", len(c.BrokerOnly),
			"cockpit_fee", c.CockpitFeeKRW, "broker_fee", deref(c.BrokerFeeKRW), "broker_realized", deref(c.BrokerRealizedKRW))
		send(ledgerSummary(day, c, path))
	}
	tick()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
