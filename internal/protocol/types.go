// Package protocol 은 yovel v1 릴레이 프로토콜의 Go 측 구현이다.
//
// ★ SSOT 는 이 파일이 아니라 저장소 루트의 docs/protocol.md + schema/v1/*.json 이다.
// 여기 타입이 스키마와 어긋나면 types_test.go 의 골든 테스트가 먼저 깨진다
// (schema/examples/ 의 실제 예제를 그대로 언마셜해 본다).
package protocol

import (
	"encoding/json"
	"time"
)

// Type 은 봉투의 typ. 모르는 값은 실행하지 않는다 (E_UNSUPPORTED_TYPE).
type Type string

const (
	TypeIntentTarget   Type = "intent.target"
	TypeIntentPosition Type = "intent.position" // 예약만 — 구현 금지 (protocol.md §4.3)
	TypeCmdDerisk      Type = "cmd.derisk"
	TypeStateSnapshot  Type = "state.snapshot"
	TypeEventOrder     Type = "event.order"
	TypeHeartbeat      Type = "heartbeat"
	TypePresence       Type = "presence"
	TypeAck            Type = "ack"
)

// Version 은 이 구현이 말할 수 있는 프로토콜 메이저. 토픽 경로의 v1 과 일치해야 한다.
const Version = 1

// RejectCode 는 ack 에 싣는 거절 사유. 침묵 대신 항상 코드를 보낸다.
type RejectCode string

const (
	CodeSig             RejectCode = "E_SIG"
	CodeExpired         RejectCode = "E_EXPIRED"
	CodeSkew            RejectCode = "E_SKEW"
	CodeReplay          RejectCode = "E_REPLAY"
	CodeSchema          RejectCode = "E_SCHEMA"
	CodeUnsupportedType RejectCode = "E_UNSUPPORTED_TYPE"
	CodeMode            RejectCode = "E_MODE"
	CodePaused          RejectCode = "E_PAUSED"
	CodeMarketClosed    RejectCode = "E_MARKET_CLOSED"
	CodeSymbol          RejectCode = "E_SYMBOL"
	CodeCapital         RejectCode = "E_CAPITAL"
	CodeLocalGuard      RejectCode = "E_LOCAL_GUARD"
	CodeBroker          RejectCode = "E_BROKER"
	CodeOrphan          RejectCode = "E_ORPHAN"
	CodeRate            RejectCode = "E_RATE"
	// CodeTerminal — 이미 종결된 intent_id 로 다시 진입하려 했다.
	// ★ retained 목표는 재접속마다 그대로 다시 오므로, 이 코드가 없으면 stop 에 털린 자리에
	// 같은 목표로 곧바로 재진입한다.
	CodeTerminal RejectCode = "E_TERMINAL"
)

// Envelope 는 모든 메시지가 공유하는 껍데기.
//
// ★ Body 를 json.RawMessage 로 두는 이유: 서명 검증은 수신한 원본 바이트 위에서 해야 한다.
// 구조체로 왕복시키면 모르는 필드가 소실되어 서명이 깨진다 (forward compat 도 함께 깨진다).
type Envelope struct {
	V     int             `json:"v"`
	Typ   Type            `json:"typ"`
	ID    string          `json:"id"`
	Acct  string          `json:"acct"`
	TS    time.Time       `json:"ts"`
	Exp   *time.Time      `json:"exp,omitempty"`
	Nonce string          `json:"nonce,omitempty"`
	Seq   *uint64         `json:"seq,omitempty"`
	Sig   *Signature      `json:"sig,omitempty"`
	Body  json.RawMessage `json:"body"`
}

// Signature — Ed25519. 대상은 sig 를 제외한 봉투의 RFC 8785 JCS 정규화 바이트 (verify.go).
type Signature struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Val string `json:"val"` // base64(64 bytes)
}

type Symbol struct {
	Exchange string `json:"exchange"`
	Code     string `json:"code"`
}

// ── 다운링크 ────────────────────────────────────────────────────────────────

type BookState string

const (
	BookNormal     BookState = "normal"
	BookDegrossing BookState = "degrossing"
	BookHalted     BookState = "halted"
)

// SafeBookState 는 모르는 값을 halted 로 해석한다 (안전한 쪽. protocol.md §11).
func SafeBookState(s BookState) BookState {
	switch s {
	case BookNormal, BookDegrossing, BookHalted:
		return s
	default:
		return BookHalted
	}
}

// IntentTarget 은 이 프로토콜의 본체 — 이벤트가 아니라 목표상태 전체 스냅샷이다.
type IntentTarget struct {
	AsOfBar   time.Time `json:"as_of_bar"`
	BookState BookState `json:"book_state"`
	Targets   []Target  `json:"targets"`
}

type Want string

const (
	WantOpen Want = "open"
	WantFlat Want = "flat" // ★ flat 이 곧 청산 지시. 별도 SELL 타입은 없다.
)

type Target struct {
	IntentID string  `json:"intent_id"`
	Slot     string  `json:"slot"`
	Symbol   Symbol  `json:"symbol"`
	Side     string  `json:"side"`
	Want     Want    `json:"want"`
	Weight   float64 `json:"weight,omitempty"` // 슬롯 예산 대비 비중. ★ 원화가 아니다 (§7)
	Entry    *Entry  `json:"entry,omitempty"`
	Exit     *Exit   `json:"exit,omitempty"`
}

type Entry struct {
	Mode       string    `json:"mode"` // market | limit
	LimitPrice float64   `json:"limit_price,omitempty"`
	NotAfter   time.Time `json:"not_after"` // ★ 필수. 넘으면 진입만 포기한다.
	MaxSlipBp  int       `json:"max_slip_bp,omitempty"`
}

type Exit struct {
	StopPrice  float64    `json:"stop_price"` // ★ 로직이 아니라 숫자
	TpPrice    float64    `json:"tp_price,omitempty"`
	TpDelegate bool       `json:"tp_delegate,omitempty"`
	TimeExitAt *time.Time `json:"time_exit_at,omitempty"`
}

type DeriskAction string

const (
	DeriskLiquidate  DeriskAction = "liquidate"
	DeriskPause      DeriskAction = "pause"
	DeriskBlockEntry DeriskAction = "block_entry"
	DeriskResume     DeriskAction = "resume"
)

// CmdDerisk — 나가는 방향만. ★ 이 타입에 진입 명령은 존재하지 않는다.
type CmdDerisk struct {
	Action   DeriskAction `json:"action"`
	Scope    string       `json:"scope"` // all | slot | position
	Slot     string       `json:"slot,omitempty"`
	IntentID string       `json:"intent_id,omitempty"`
	Until    *time.Time   `json:"until,omitempty"`
	Reason   string       `json:"reason,omitempty"`
}

// ── 업링크 ──────────────────────────────────────────────────────────────────

type Mode string

const (
	ModePaper Mode = "paper"
	ModeLive  Mode = "live"
)

type DaemonInfo struct {
	Version   string     `json:"version"`
	SHA       string     `json:"sha"`
	StartedAt *time.Time `json:"started_at,omitempty"`
}

type Guards struct {
	Paused          bool       `json:"paused"`
	BlockEntryUntil *time.Time `json:"block_entry_until"`
	CircuitBreaker  bool       `json:"circuit_breaker"`
	// ★ omitempty 금지 — "진입 가능" 과 "안 알려줌" 이 같은 와이어 모양이 되면 안 된다.
	TargetStale bool `json:"target_stale"`
}

type StateSnapshot struct {
	AsOf       time.Time  `json:"as_of"`
	Daemon     DaemonInfo `json:"daemon"`
	Mode       Mode       `json:"mode"` // ★ 생략 불가 — paper/live 합산은 허위 표시
	AppliedSeq uint64     `json:"applied_seq"`
	Guards     Guards     `json:"guards"`
	Positions  []Position `json:"positions"`
	// ★ omitempty 를 붙이지 않는다: "유령 없음(빈 배열)" 과 "안 봤음(필드 부재)" 은 다른 말이다.
	Orphans []Symbol `json:"orphans"`
}

type Position struct {
	IntentID      string     `json:"intent_id"`
	Slot          string     `json:"slot,omitempty"`
	Symbol        Symbol     `json:"symbol"`
	Qty           float64    `json:"qty"`
	AvgEntryPrice float64    `json:"avg_entry_price"`
	EntryAt       *time.Time `json:"entry_at,omitempty"`
	StopArmed     float64    `json:"stop_armed,omitempty"`
	TpOrderID     string     `json:"tp_order_id,omitempty"`
	UnrealizedPct float64    `json:"unrealized_pct,omitempty"`
}

type EventOrder struct {
	IntentID      string     `json:"intent_id"`
	Phase         string     `json:"phase"`
	Symbol        Symbol     `json:"symbol"`
	Side          string     `json:"side"`
	Qty           float64    `json:"qty,omitempty"`
	Price         float64    `json:"price,omitempty"`
	BrokerOrderID string     `json:"broker_order_id,omitempty"`
	SignalTS      *time.Time `json:"signal_ts,omitempty"`
	SubmittedAt   *time.Time `json:"submitted_at,omitempty"`
	FilledAt      *time.Time `json:"filled_at,omitempty"`
	SlippageBp    float64    `json:"slippage_bp,omitempty"`
	FeeKRW        float64    `json:"fee_krw,omitempty"`
	ExitReason    string     `json:"exit_reason,omitempty"`
	RealizedPct   float64    `json:"realized_pct,omitempty"`
	BrokerCode    string     `json:"broker_code,omitempty"`
	Detail        string     `json:"detail,omitempty"`
}

type Heartbeat struct {
	Seq         uint64 `json:"seq"`
	DaemonSHA   string `json:"daemon_sha"`
	UptimeSec   int64  `json:"uptime_sec,omitempty"`
	BrokerWS    string `json:"broker_ws"` // up | down | degraded
	ClockSkewMS int64  `json:"clock_skew_ms,omitempty"`
	Mode        Mode   `json:"mode,omitempty"`
}

type Presence struct {
	Online    bool       `json:"online"`
	Reason    string     `json:"reason"` // connect | shutdown | lwt
	Since     *time.Time `json:"since,omitempty"`
	DaemonSHA string     `json:"daemon_sha,omitempty"`
}

type Ack struct {
	RefID     string       `json:"ref_id"`
	RefSeq    uint64       `json:"ref_seq,omitempty"`
	RefTyp    Type         `json:"ref_typ,omitempty"`
	Status    string       `json:"status"` // applied | partial | rejected | expired | ignored
	Codes     []RejectCode `json:"codes,omitempty"`
	PerIntent []AckIntent  `json:"per_intent,omitempty"`
	Detail    string       `json:"detail,omitempty"`
}

type AckIntent struct {
	IntentID string       `json:"intent_id"`
	Status   string       `json:"status"` // applied | rejected | expired | noop
	Codes    []RejectCode `json:"codes,omitempty"`
}
