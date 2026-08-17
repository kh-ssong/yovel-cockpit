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
	// CodeAcct — 이 콕핏의 계정이 아닌 봉투가 왔다.
	// ★ 서명이 유효해도 거절한다. 서명은 "누가 만들었나" 를 증명하지 "누구에게 가는 것인가" 는
	// 증명하지 않는다 — 릴레이는 A 의 진짜 서명된 목표를 B 에게 배달할 수 있다.
	CodeAcct RejectCode = "E_ACCT"
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
	// RefPrice — 사이징 기준가. ★ 신호를 낸 쪽이 아는 값을 실어 보내면
	// "신호를 낸 가격" 과 "사이징한 가격" 이 갈리지 않는다 (그리고 시세 왕복이 사라진다).
	RefPrice  float64 `json:"ref_price,omitempty"`
	MaxSlipBp int     `json:"max_slip_bp,omitempty"`
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
	// Account — 계좌가 불어나는지 줄어드는지. ★ 조회 실패 시 nil 이고, **0 으로 채우지 않는다**
	// (잔고 0 과 «못 봤음» 을 합치면 사용자가 파산한 줄 안다).
	Account *Account `json:"account,omitempty"`
}

// Account — 예수금·평가액. paper 면 가상, live 면 진짜다 (`Mode` 로 구분).
type Account struct {
	Deposit   float64 `json:"deposit"`   // 현금
	Orderable float64 `json:"orderable"` // 주문가능 (live 에서는 예수금과 다르다)
	Holdings  float64 `json:"holdings"`  // 보유 평가금액
	Equity    float64 `json:"equity"`    // 현금 + 평가금액
	Currency  string  `json:"currency"`
	// StaleHoldings — 평가에 쓸 현재가를 못 구한 종목이 있었다. ★ 그 경우 평가액은
	// 평단으로 대신 채워지므로 **Equity 가 실제와 다르다**. 조용히 두면 안 되는 사실이다.
	StaleHoldings int `json:"stale_holdings,omitempty"`
}

type Position struct {
	IntentID      string     `json:"intent_id"`
	Slot          string     `json:"slot,omitempty"`
	Symbol        Symbol     `json:"symbol"`
	Qty           float64    `json:"qty"`
	AvgEntryPrice float64    `json:"avg_entry_price"`
	EntryAt       *time.Time `json:"entry_at,omitempty"`
	StopArmed     float64    `json:"stop_armed,omitempty"`
	// TpArmed — **브로커에 실제로 걸어 둔** TP 지정가. ★ StopArmed 의 짝이다. 이게 없어서
	// "이미 건 값과 같은가" 를 비교할 수 없었고, 그래서 TP 는 **매 틱 취소·재발행**됐다
	// (2026-08-17 실측: 몇 분 만에 paper-tp-38 → 334). 라이브에선 주문 유량 문제이자, 더
	// 나쁘게는 취소와 재발행 **사이에 TP 가 브로커에 없는 창**이 생긴다 — 하필 그 층의
	// 존재 이유가 "데몬도 서버도 죽어도 이건 체결된다" 이다.
	TpArmed       float64    `json:"tp_armed,omitempty"`
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
