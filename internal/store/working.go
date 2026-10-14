package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

// WorkingOrder — 접수됐지만 아직 끝나지 않은 주문.
type WorkingOrder struct {
	OrderID  string `json:"order_id"`
	IntentID string `json:"intent_id"`
	// Purpose — entry | exit. Reason — 청산 사유 (flat·time·stop·reduce·derisk …).
	Purpose string          `json:"purpose"`
	Reason  string          `json:"reason,omitempty"`
	Side    string          `json:"side"`
	Symbol  protocol.Symbol `json:"symbol"`
	// Qty — 낸 수량 (금액 주문은 추정). RefPrice — 슬리피지 기준가.
	Qty      float64 `json:"qty"`
	RefPrice float64 `json:"ref_price,omitempty"`

	Kid   string `json:"kid,omitempty"`
	Scope string `json:"scope,omitempty"`
	// Target — 진입 목표 (체결되면 로트의 청산 조건·group 이 여기서 온다). AsOfBar — 신호 시각.
	Target  *protocol.Target `json:"target,omitempty"`
	AsOfBar time.Time        `json:"as_of_bar,omitempty"`

	SubmittedAt time.Time `json:"submitted_at"` // 브로커 드라이버가 찍은 접수 시각 (원장용)
	// PlacedAt — 집행기 시계로 낸 시각. 마감·지연 판정은 이 시계로 한다 (판정과 같은 시계).
	PlacedAt time.Time `json:"placed_at"`
	// Deadline — 진입 주문은 이 시각을 넘으면 잔량을 취소한다 (늦은 체결은 다른 가격이다).
	Deadline        time.Time `json:"deadline,omitempty"`
	CancelRequested bool      `json:"cancel_requested,omitempty"`
	// Unconfirmed — **보내기 전에** 남긴 기록이다 (주문번호 없음, OrderID 는 임시 값).
	// 접수 응답을 받으면 진짜 주문번호 행으로 바뀐다(ConfirmWorking). 재시작했는데 이게 남아 있으면
	// 보내는 도중 죽은 것이다 — 주문이 나갔는지 모른다 (executor.resolveUnconfirmed).
	Unconfirmed bool `json:"unconfirmed,omitempty"`
}

// ConfirmWorking — 임시 기록(보내기 전)을 접수된 주문으로 바꾼다. ★ 한 트랜잭션 — 사이에 죽어도
// "임시와 진짜가 둘 다 열림" 이나 "둘 다 없음" 이 생기지 않는다.
func (s *Store) ConfirmWorking(ctx context.Context, tempID string, w WorkingOrder) error {
	raw, err := json.Marshal(w)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE working_orders SET done_at=? WHERE order_id=? AND done_at IS NULL`,
		nowStr(), tempID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO working_orders (order_id, intent_id, payload, created_at) VALUES (?,?,?,?)
ON CONFLICT(order_id) DO UPDATE SET payload=excluded.payload`,
		w.OrderID, w.IntentID, string(raw), nowStr()); err != nil {
		return err
	}
	return tx.Commit()
}

// PutWorking — 접수 즉시 기록 (같은 주문번호면 덮는다).
func (s *Store) PutWorking(ctx context.Context, w WorkingOrder) error {
	raw, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO working_orders (order_id, intent_id, payload, created_at) VALUES (?,?,?,?)
ON CONFLICT(order_id) DO UPDATE SET payload=excluded.payload`,
		w.OrderID, w.IntentID, string(raw), nowStr())
	return err
}

// FinishWorking — 끝난 주문 (체결·취소). 행은 지우지 않는다 — 사후 감사용.
func (s *Store) FinishWorking(ctx context.Context, orderID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE working_orders SET done_at=? WHERE order_id=? AND done_at IS NULL`,
		ts(at), orderID)
	return err
}

// OpenWorking — 아직 끝나지 않은 주문들 (재시작 복구용).
func (s *Store) OpenWorking(ctx context.Context) ([]WorkingOrder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM working_orders WHERE done_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkingOrder
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var w WorkingOrder
		if err := json.Unmarshal([]byte(raw), &w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// KnownOrderIDs — 콕핏이 낸 적 있는 주문번호 전부 (체결·진행 중·위임 TP). 콕핏 밖 매도를 가려낼 때 뺀다.
func (s *Store) KnownOrderIDs(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	for _, q := range []string{
		`SELECT broker_order_id FROM orders WHERE broker_order_id IS NOT NULL AND broker_order_id <> ''`,
		`SELECT order_id FROM working_orders`,
		`SELECT tp_order_id FROM intents WHERE tp_order_id IS NOT NULL AND tp_order_id <> ''`,
	} {
		rows, err := s.db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		rows.Close()
	}
	return out, nil
}
