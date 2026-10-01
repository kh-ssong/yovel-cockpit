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
