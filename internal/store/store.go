// Package store 는 데몬의 로컬 저장소다 (SQLite 단일 파일).
//
// ★ 여기 있는 것과 없는 것을 헷갈리면 장부↔실물 괴리 사고가 난다.
//
//	있는 것: 브로커가 모르는 것 — 의도(intent_id)·무장된 stop·종결 이력·체결 지연 원장.
//	없는 것: 포지션의 진실. 그건 브로커가 SSOT 고, 매 reconcile 마다 조회해 대조한다.
//
// ★ 드라이버가 modernc.org/sqlite 인 이유는 취향이 아니다. 흔히 쓰는 mattn/go-sqlite3 는
// cgo 라 CGO_ENABLED=0 크로스컴파일이 깨지는데, 윈도우 PC 한 대에서 맥 ARM 바이너리를 뽑는
// 게 Go 를 고른 유일한 결정타였다. 드라이버 하나로 그 전제를 날릴 수 없다.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

//go:embed schema.sql
var schemaSQL string

const schemaVersion = 1

type Store struct{ db *sql.DB }

// Open 은 data-dir 아래 cockpit.db 를 연다.
func Open(dataDir string) (*Store, error) { return OpenPath(filepath.Join(dataDir, "cockpit.db")) }

func OpenPath(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" + // 쓰기 중에도 읽기가 막히지 않는다
		"&_pragma=busy_timeout(5000)" + // 대시보드 조회와 원장 기록이 겹칠 때 즉시 실패하지 않도록
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// ★ 단일 커넥션으로 제한한다. SQLite 는 쓰기가 직렬화되는데, 풀을 열어두면
	// "database is locked" 를 랜덤하게 만나고 그게 하필 09:00 에 터진다.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("스키마 적용 실패: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	if !v.Valid {
		_, err := s.db.ExecContext(ctx, `INSERT INTO schema_version(version) VALUES (?)`, schemaVersion)
		return err
	}
	if v.Int64 > schemaVersion {
		// 옛 바이너리가 새 DB 를 열면 모르는 컬럼을 조용히 무시하며 돈다. 그게 최악이다.
		return fmt.Errorf("DB 스키마 v%d 인데 이 바이너리는 v%d 까지만 안다 — 데몬을 업데이트할 것",
			v.Int64, schemaVersion)
	}
	return nil
}

// ── intents ─────────────────────────────────────────────────────────────────

type Intent struct {
	IntentID      string
	Slot          string
	Symbol        protocol.Symbol
	Side          string
	Qty           float64
	AvgEntryPrice float64
	StopArmed     float64
	TpPrice       float64
	TpOrderID     string
	TimeExitAt    *time.Time
	EntryAt       *time.Time
	ClosedAt      *time.Time
	CloseReason   string
}

func (s *Store) UpsertIntent(ctx context.Context, in Intent) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO intents (intent_id, slot, exchange, code, side, qty, avg_entry_price,
                     stop_armed, tp_price, tp_order_id, time_exit_at, entry_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(intent_id) DO UPDATE SET
  qty=excluded.qty, avg_entry_price=excluded.avg_entry_price,
  stop_armed=excluded.stop_armed, tp_price=excluded.tp_price,
  tp_order_id=excluded.tp_order_id, time_exit_at=excluded.time_exit_at,
  entry_at=COALESCE(intents.entry_at, excluded.entry_at),
  updated_at=excluded.updated_at`,
		in.IntentID, in.Slot, in.Symbol.Exchange, in.Symbol.Code, in.Side,
		in.Qty, in.AvgEntryPrice, in.StopArmed, in.TpPrice, nullStr(in.TpOrderID),
		nullTime(in.TimeExitAt), nullTime(in.EntryAt), nowStr())
	return err
}

// CloseIntent 는 목표를 종결시킨다. ★ 이 기록이 재진입을 막는 유일한 장치다.
func (s *Store) CloseIntent(ctx context.Context, intentID, reason string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE intents SET closed_at=COALESCE(closed_at, ?), close_reason=COALESCE(close_reason, ?), updated_at=?
WHERE intent_id=?`, ts(at), reason, nowStr(), intentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("모르는 intent_id %q", intentID)
	}
	return nil
}

// OpenIntents 는 아직 종결되지 않은 목표를 포지션 형태로 준다 (재시작 복구용).
func (s *Store) OpenIntents(ctx context.Context) ([]protocol.Position, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT intent_id, slot, exchange, code, qty, avg_entry_price, stop_armed, tp_order_id, entry_at
FROM intents WHERE closed_at IS NULL ORDER BY intent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []protocol.Position
	for rows.Next() {
		var p protocol.Position
		var tpOrder sql.NullString
		var entryAt sql.NullString
		if err := rows.Scan(&p.IntentID, &p.Slot, &p.Symbol.Exchange, &p.Symbol.Code,
			&p.Qty, &p.AvgEntryPrice, &p.StopArmed, &tpOrder, &entryAt); err != nil {
			return nil, err
		}
		p.TpOrderID = tpOrder.String
		p.EntryAt = parseTime(entryAt)
		out = append(out, p)
	}
	return out, rows.Err()
}

// TerminalIntents 는 이미 끝난 intent_id 집합.
//
// ★ 이게 없으면 stop 에 털린 자리에 같은 목표로 곧바로 재진입한다 — retained 목표는
// 재접속마다 그대로 다시 오고, 진입 창(not_after)이 아직 안 지났을 수 있기 때문이다.
func (s *Store) TerminalIntents(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT intent_id FROM intents WHERE closed_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// ── orders (매매기록 원장) ──────────────────────────────────────────────────

type Order struct {
	ID            string // ULID. 멱등키
	IntentID      string
	Phase         string
	Symbol        protocol.Symbol
	Side          string
	Qty           float64
	Price         float64
	BrokerOrderID string
	SignalTS      *time.Time
	SubmittedAt   *time.Time
	FilledAt      *time.Time
	SlippageBp    float64
	FeeKRW        float64
	ExitReason    string
	RealizedPct   float64
	BrokerCode    string
	Detail        string
	Mode          protocol.Mode // ★ paper | live. 빈 값 금지
	Source        Source        // ★ bot | manual
	DaemonSHA     string
	CreatedAt     time.Time
}

// Source — 이 주문을 누가 냈는가.
// ★ 사용자가 HTS 로 직접 판 것을 봇 청산으로 오인하면 형제 레그까지 잘못 청산된다.
type Source string

const (
	SourceBot    Source = "bot"
	SourceManual Source = "manual"
)

var ErrModeRequired = errors.New("mode 는 paper 또는 live 여야 한다 — 빈 값이면 원장이 합산돼 허위 손익이 된다")

// RecordOrder 는 원장에 한 줄 남긴다. 같은 ID 재기록은 무해하다 (QoS1 = at-least-once).
func (s *Store) RecordOrder(ctx context.Context, o Order) error {
	if o.Mode != protocol.ModePaper && o.Mode != protocol.ModeLive {
		return ErrModeRequired
	}
	if o.Source == "" {
		o.Source = SourceBot
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO orders (id, intent_id, phase, exchange, code, side, qty, price,
  broker_order_id, signal_ts, submitted_at, filled_at, slippage_bp, fee_krw,
  exit_reason, realized_pct, broker_code, detail, mode, source, daemon_sha, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.ID, o.IntentID, o.Phase, o.Symbol.Exchange, o.Symbol.Code, o.Side, o.Qty, o.Price,
		nullStr(o.BrokerOrderID), nullTime(o.SignalTS), nullTime(o.SubmittedAt), nullTime(o.FilledAt),
		o.SlippageBp, o.FeeKRW, nullStr(o.ExitReason), o.RealizedPct, nullStr(o.BrokerCode),
		nullStr(o.Detail), string(o.Mode), string(o.Source), nullStr(o.DaemonSHA), ts(o.CreatedAt))
	return err
}

// LedgerQuery — 원장 조회. ★ Mode 는 필수다.
// "전체 보기"를 기본값으로 두면 paper 와 live 가 합산돼 실계좌가 손실인데 수익으로 보인다.
type LedgerQuery struct {
	Mode  protocol.Mode
	Since *time.Time
	Limit int
}

func (s *Store) Ledger(ctx context.Context, q LedgerQuery) ([]Order, error) {
	if q.Mode != protocol.ModePaper && q.Mode != protocol.ModeLive {
		return nil, ErrModeRequired
	}
	if q.Limit <= 0 || q.Limit > 5000 {
		q.Limit = 500
	}
	since := "0001-01-01T00:00:00Z"
	if q.Since != nil {
		since = ts(*q.Since)
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT id, intent_id, phase, exchange, code, side, qty, price, broker_order_id,
       signal_ts, submitted_at, filled_at, slippage_bp, fee_krw, exit_reason,
       realized_pct, broker_code, detail, mode, source, daemon_sha, created_at
FROM orders WHERE mode = ? AND created_at >= ?
ORDER BY created_at DESC, id DESC LIMIT ?`, string(q.Mode), since, q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Order
	for rows.Next() {
		var o Order
		var brokerOrder, signalTS, submitted, filled, exitReason, brokerCode, detail, sha sql.NullString
		var created string
		if err := rows.Scan(&o.ID, &o.IntentID, &o.Phase, &o.Symbol.Exchange, &o.Symbol.Code,
			&o.Side, &o.Qty, &o.Price, &brokerOrder, &signalTS, &submitted, &filled,
			&o.SlippageBp, &o.FeeKRW, &exitReason, &o.RealizedPct, &brokerCode, &detail,
			&o.Mode, &o.Source, &sha, &created); err != nil {
			return nil, err
		}
		o.BrokerOrderID, o.ExitReason, o.BrokerCode, o.Detail, o.DaemonSHA =
			brokerOrder.String, exitReason.String, brokerCode.String, detail.String, sha.String
		o.SignalTS, o.SubmittedAt, o.FilledAt = parseTime(signalTS), parseTime(submitted), parseTime(filled)
		if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
			o.CreatedAt = t
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ── guards ──────────────────────────────────────────────────────────────────

type GuardState struct {
	Paused          bool
	BlockEntryUntil *time.Time
	CircuitBreaker  bool
	LiquidateAll    bool
	Reason          string
}

func (s *Store) LoadGuards(ctx context.Context) (GuardState, error) {
	var g GuardState
	var until, reason sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT paused, block_entry_until, circuit_breaker, liquidate_all, reason FROM guards WHERE id=1`).
		Scan(&g.Paused, &until, &g.CircuitBreaker, &g.LiquidateAll, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return GuardState{}, nil // 처음 기동 — 아무 가드도 안 걸린 상태가 맞다
	}
	if err != nil {
		return GuardState{}, err
	}
	g.BlockEntryUntil, g.Reason = parseTime(until), reason.String
	return g, nil
}

func (s *Store) SaveGuards(ctx context.Context, g GuardState) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO guards (id, paused, block_entry_until, circuit_breaker, liquidate_all, reason, updated_at)
VALUES (1,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  paused=excluded.paused, block_entry_until=excluded.block_entry_until,
  circuit_breaker=excluded.circuit_breaker, liquidate_all=excluded.liquidate_all,
  reason=excluded.reason, updated_at=excluded.updated_at`,
		g.Paused, nullTime(g.BlockEntryUntil), g.CircuitBreaker, g.LiquidateAll,
		nullStr(g.Reason), nowStr())
	return err
}

// ── outbox ──────────────────────────────────────────────────────────────────

type OutboxItem struct {
	Seq     int64
	ID      string
	Typ     protocol.Type
	Payload []byte
}

// Enqueue 는 업링크를 큐에 넣는다. 같은 ID 재삽입은 무시된다.
func (s *Store) Enqueue(ctx context.Context, id string, typ protocol.Type, payload []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO outbox (id, typ, payload, created_at) VALUES (?,?,?,?)`,
		id, string(typ), payload, nowStr())
	return err
}

func (s *Store) Pending(ctx context.Context, limit int) ([]OutboxItem, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, id, typ, payload FROM outbox WHERE sent_at IS NULL ORDER BY seq LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		if err := rows.Scan(&it.Seq, &it.ID, &it.Typ, &it.Payload); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) MarkSent(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `UPDATE outbox SET sent_at=? WHERE id=? AND sent_at IS NULL`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, nowStr(), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneSent 는 오래 전에 보낸 것을 지운다. 원장(orders)은 건드리지 않는다 — 큐만 정리한다.
func (s *Store) PruneSent(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM outbox WHERE sent_at IS NOT NULL AND sent_at < ?`, ts(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ── helpers ─────────────────────────────────────────────────────────────────

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func nowStr() string        { return ts(time.Now()) }

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func parseTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return nil
	}
	return &t
}
