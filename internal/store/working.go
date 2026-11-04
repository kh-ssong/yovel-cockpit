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

	// Sweep — 집행 방식 sweep 의 부모 주문이면 그 진행 상태 (OrderID 는 "sweep-…" 내부 번호 · 증권사 주문은
	// 조각마다 따로다). 조각이 끝날 때마다 이 행을 고쳐 쓴다 — 재시작 뒤 이어서 간다 (executor/sweep.go).
	Sweep *Sweep `json:"sweep,omitempty"`
}

// Sweep — sweep 한 번의 진행 상태.
type Sweep struct {
	// Limit — 매수 = cap_price (매도1 이 이 값 이하일 때만 IOC) · 매도 = floor_price (매수1 이 이 값 이상일 때만).
	Limit float64 `json:"limit"`
	// Until — 창 끝. 넘으면 잔량을 시장가로 낸다.
	Until time.Time `json:"until"`
	// MaxSlipBp — 매수 잔량 시장가의 상한 (Limit 대비 bp). 매도1 이 넘으면 잔량을 포기한다. 0 = 없음.
	MaxSlipBp int `json:"max_slip_bp,omitempty"`

	RefAsk1 float64   `json:"ref_ask1,omitempty"`
	RefBid1 float64   `json:"ref_bid1,omitempty"`
	RefAt   time.Time `json:"ref_at,omitempty"`

	Fills []protocol.SweepFill `json:"fills,omitempty"`
	// OrderIDs — 낸 조각의 증권사 주문번호 전부 (0 체결 포함). IOCs — 낸 IOC 수.
	OrderIDs []string `json:"order_ids,omitempty"`
	IOCs     int      `json:"iocs,omitempty"`
	// Child — 지금 나가 있는 조각 (한 번에 하나뿐 — 앞 조각의 체결을 모르는 채 다음을 내면 더 산다).
	Child *SweepChild `json:"child,omitempty"`
	// NextBook — 이 시각 전엔 호가를 다시 묻지 않는다 (같은 호가에 IOC 를 거듭 내지 않게 · 조회 한도).
	NextBook time.Time `json:"next_book,omitempty"`
	// Ended — 창을 닫은 사유 (protocol.ExecReport.Ended). 비면 아직 창 안이다.
	Ended string `json:"ended,omitempty"`
	// RestDone — 창 끝 잔량 처리가 끝났다 (시장가 체결·포기·거부). 다음 단계는 마무리뿐이다.
	RestDone bool `json:"rest_done,omitempty"`
	// IOCErrors — 연달아 거부된 IOC 수. 넘치면 창을 일찍 닫고 잔량 시장가로 간다 (거부만 반복하지 않게).
	IOCErrors int    `json:"ioc_errors,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// SweepChild — 나가 있는 조각 주문 하나.
type SweepChild struct {
	OrderID string  `json:"order_id,omitempty"`
	Kind    string  `json:"kind"` // ioc | rest
	Qty     float64 `json:"qty"`
	// Price — IOC 지정가 (rest 는 0 = 시장가).
	Price       float64   `json:"price,omitempty"`
	PlacedAt    time.Time `json:"placed_at"`
	SubmittedAt time.Time `json:"submitted_at,omitempty"`
	// Unconfirmed — 보내기 전에 남긴 기록 (접수 응답 전). 재시작했는데 남아 있으면 나갔는지 모른다.
	Unconfirmed bool `json:"unconfirmed,omitempty"`
	// CancelSent — IOC 잔량 확인용 취소를 이미 보냈다.
	CancelSent bool `json:"cancel_sent,omitempty"`
}

// Filled — 지금까지 찬 수량 · 금액 · 수수료.
func (s *Sweep) Filled() (qty, amount, fee float64) {
	for _, f := range s.Fills {
		qty += f.Qty
		amount += f.Qty * f.Price
		fee += f.FeeKRW
	}
	return qty, amount, fee
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
//
// ★ sweep 은 한 진입·청산에 증권사 주문이 여럿이다 — 원장 exec·진행 중 sweep 의 조각 번호도 센다.
// 빠뜨리면 자기 IOC 매도를 «콕핏 밖 매도» 로 읽어 같은 종목 형제 로트를 잘못 닫는다.
func (s *Store) KnownOrderIDs(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	if err := s.sweepOrderIDs(ctx, out); err != nil {
		return nil, err
	}
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

// sweepOrderIDs — sweep 조각의 주문번호 (끝난 것은 원장 exec, 진행 중인 것은 working_orders payload).
func (s *Store) sweepOrderIDs(ctx context.Context, out map[string]bool) error {
	rows, err := s.db.QueryContext(ctx, `SELECT exec FROM orders WHERE exec IS NOT NULL AND exec <> ''`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var r protocol.ExecReport
		if json.Unmarshal([]byte(raw), &r) == nil {
			for _, id := range r.OrderIDs {
				out[id] = true
			}
			for _, f := range r.Fills {
				out[f.OrderID] = true
			}
		}
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT payload FROM working_orders WHERE payload LIKE '%"sweep"%'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var w WorkingOrder
		if json.Unmarshal([]byte(raw), &w) != nil || w.Sweep == nil {
			continue
		}
		for _, id := range w.Sweep.OrderIDs {
			out[id] = true
		}
		if c := w.Sweep.Child; c != nil && c.OrderID != "" {
			out[c.OrderID] = true
		}
	}
	return rows.Err()
}
