package store

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// ── 재시작 복구용 (v5) ──────────────────────────────────────────────────────────
//
// 메모리에만 있던 것 중 재시작으로 잃으면 안 되는 것들: 소스별 seq · de-risk nonce ·
// 최근 종결(recent_closes) · paper 예수금. 앞의 둘은 따로 적고, 뒤의 둘은 원장에서 다시 만든다.

// SaveSeq — 소스(skey = typ|kid|scope)의 마지막 seq. 더 작은 값으로는 되돌리지 않는다.
func (s *Store) SaveSeq(ctx context.Context, skey string, seq uint64) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO source_seq (skey, seq, updated_at) VALUES (?,?,?)
ON CONFLICT(skey) DO UPDATE SET seq=MAX(source_seq.seq, excluded.seq), updated_at=excluded.updated_at`,
		skey, int64(seq), nowStr())
	return err
}

// LoadSeqs — 소스별 마지막 seq 전부.
func (s *Store) LoadSeqs(ctx context.Context) (map[string]uint64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT skey, seq FROM source_seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]uint64{}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = uint64(v)
	}
	return out, rows.Err()
}

// SaveNonce — 받아들인 de-risk 의 nonce 와 만료.
func (s *Store) SaveNonce(ctx context.Context, nonce string, exp time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO derisk_nonces (nonce, exp) VALUES (?,?)`, nonce, ts(exp))
	return err
}

// LoadNonces — 아직 만료되지 않은 nonce. 만료된 것은 지운다.
func (s *Store) LoadNonces(ctx context.Context, now time.Time) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT nonce, exp FROM derisk_nonces`)
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	var stale []string
	for rows.Next() {
		var n string
		var e sql.NullString
		if err := rows.Scan(&n, &e); err != nil {
			rows.Close()
			return nil, err
		}
		if t := parseTime(e); t != nil && t.After(now) {
			out[n] = *t
		} else {
			stale = append(stale, n)
		}
	}
	rows.Close()
	for _, n := range stale {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM derisk_nonces WHERE nonce=?`, n)
	}
	return out, nil
}

// RecentCloses — since 이후 종결된 로트 (state.snapshot.recent_closes 복원용, 오래된 것부터).
// 가격은 그 로트의 마지막 청산 체결가 (없으면 0 = 모름 — 결과 미상 매수·사후 감지 등).
func (s *Store) RecentCloses(ctx context.Context, since time.Time) ([]protocol.ClosedLot, error) {
	// ★ closed_at 은 RFC3339Nano 라 자릿수가 일정하지 않다 — 문자열 비교는 넉넉히 거르고 시각은 Go 에서 다시 본다.
	rows, err := s.db.QueryContext(ctx, `
SELECT i.intent_id, i.kid, i.scope, i.grp, i.exchange, i.code, COALESCE(i.close_reason, ''), i.closed_at,
       i.avg_entry_price,
       COALESCE((SELECT o.price FROM orders o WHERE o.intent_id = i.intent_id AND o.phase = 'exit_filled'
                 ORDER BY o.created_at DESC LIMIT 1), 0)
FROM intents i WHERE i.closed_at IS NOT NULL AND i.closed_at >= ?`, ts(since.Add(-time.Second)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.ClosedLot
	for rows.Next() {
		var c protocol.ClosedLot
		var closedAt sql.NullString
		var avg float64
		if err := rows.Scan(&c.IntentID, &c.Kid, &c.Scope, &c.Group, &c.Symbol.Exchange, &c.Symbol.Code,
			&c.Reason, &closedAt, &avg, &c.Price); err != nil {
			return nil, err
		}
		at := parseTime(closedAt)
		if at == nil || at.Before(since) {
			continue
		}
		c.At = *at
		if c.Price > 0 && avg > 0 {
			c.RealizedPct = c.Price/avg - 1
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// PaperCashFlow — paper 원장의 현금 흐름 합 (매도 대금 − 수수료 − 매수 대금 − 수수료)과
// paper 주문번호(`paper-N`·`paper-tp-N`)의 가장 큰 N.
//
// ★ paper 브로커는 메모리라, 재시작하면 예수금이 시드로 돌아가고 보유가 사라진다 — 열린 로트가
// "콕핏 밖 매도·가격 미상" 으로 닫힌다 (2026-10-15 실측). 원장이 paper 의 진실이므로 여기서 다시 만든다.
// 가격 미상 행(사후 감지)은 돈이 오가지 않은 것으로 본다.
func (s *Store) PaperCashFlow(ctx context.Context) (net float64, maxSeq int, err error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT phase, side, COALESCE(qty, 0), COALESCE(price, 0), COALESCE(fee_krw, 0), COALESCE(broker_order_id, '')
FROM orders WHERE mode = 'paper'`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var phase, side, oid string
		var qty, price, fee float64
		if err := rows.Scan(&phase, &side, &qty, &price, &fee, &oid); err != nil {
			return 0, 0, err
		}
		if n := paperSeq(oid); n > maxSeq {
			maxSeq = n
		}
		if price <= 0 || qty <= 0 {
			continue
		}
		switch {
		case phase == "filled" && side == "buy":
			net -= qty*price + fee
		case phase == "exit_filled" && side == "sell":
			net += qty*price - fee
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	// 걸어 둔 TP 번호도 본다 — 새 주문번호가 살아 있는 TP 와 겹치면 안 된다.
	trows, err := s.db.QueryContext(ctx, `SELECT tp_order_id FROM intents WHERE tp_order_id IS NOT NULL`)
	if err != nil {
		return 0, 0, err
	}
	defer trows.Close()
	for trows.Next() {
		var oid string
		if err := trows.Scan(&oid); err != nil {
			return 0, 0, err
		}
		if n := paperSeq(oid); n > maxSeq {
			maxSeq = n
		}
	}
	return net, maxSeq, trows.Err()
}

func paperSeq(id string) int {
	if !strings.HasPrefix(id, "paper-") {
		return 0
	}
	id = strings.TrimPrefix(strings.TrimPrefix(id, "paper-"), "tp-")
	n, _ := strconv.Atoi(id)
	return n
}
