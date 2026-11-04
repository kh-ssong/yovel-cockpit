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
	// CodeInactive — 이 콕핏에서 활성화하지 않은 (kid, scope) 의 진입. 사용자가 끈 전략이거나
	// 장부 설정이 없는 전략이다. ★ 청산은 막지 않는다 — 끈다고 보유가 무방비가 되면 안 된다.
	CodeInactive RejectCode = "E_INACTIVE"
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
	// Scope — 발행 범위 (`카테고리/playbook`, 예: "intraday/d205"). pitwall architecture.md §12.
	//
	// ★ 스냅샷의 의미는 "계정 전체의 목표" 가 아니라 **"이 (kid, scope) 의 전체 목표"** 다.
	// 한 발행자가 여러 전략을 내면 전략마다 따로 스냅샷을 보내고, 서로를 지우지 않는다.
	// 비어 있으면 그 kid 의 기본 범위 하나로 읽는다 (scope 이전 발행자와 호환).
	Scope     string    `json:"scope,omitempty"`
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
	IntentID string `json:"intent_id"`
	// Group — 같은 포지션으로 묶을 로트들의 이름 (분할매수, 2026-09-30). 선택.
	// ★ 추가 매수는 새 intent_id(= 새 로트)로 낸다 — 진입가·시점이 달라서 로트가 따로여야 한다.
	// group 은 UI·성과를 "포지션 단위" 로 묶어 보이기 위한 표지일 뿐 집행에 쓰지 않는다.
	Group  string  `json:"group,omitempty"`
	Slot   string  `json:"slot"`
	Symbol Symbol  `json:"symbol"`
	Side   string  `json:"side"`
	Want   Want    `json:"want"`
	Weight float64 `json:"weight,omitempty"` // 슬롯 예산 대비 비중. ★ 원화가 아니다 (§7)
	Entry  *Entry  `json:"entry,omitempty"`
	Exit   *Exit   `json:"exit,omitempty"`

	// MarkPrice / MarkAt — 신호를 낸 쪽이 본 **지금 가격** (선택).
	//
	// ★ ref_price 는 진입 때만 온다. 그래서 청산(want=flat)·TP 평가 때 콕핏은 가격을 몰랐고,
	// paper 는 평단으로 근사 체결했다 — 손익이 0 근처로 뭉개져 전략 검증이 안 됐다.
	// 시세는 flat6 가 이미 들고 있으므로(키움 WS 독점) 콕핏이 따로 조회하지 않고 이걸 쓴다.
	// 청산이면 매수1호가(내가 팔 수 있는 값), 보유면 현재가를 싣는다.
	MarkPrice float64    `json:"mark_price,omitempty"`
	MarkAt    *time.Time `json:"mark_at,omitempty"`

	// ExitExec — 이 로트를 **want=flat 으로** 팔 때의 집행 방식 (선택, 2026-11-04). 없으면 시장가.
	// ★ stop·시간청산(콕핏 로컬 층)은 이 값과 무관하게 즉시 시장가다 — 안전망은 기다리지 않는다.
	ExitExec *ExitExec `json:"exit_exec,omitempty"`
}

// 집행 방식 (entry.mode · exit_exec.mode).
const (
	ExecMarket = "market"
	ExecLimit  = "limit"
	// ExecSweep — 창(sweep_until) 안에서 매도1 ≤ cap_price (매도면 매수1 ≥ floor_price) 일 때마다
	// 그 값으로 IOC 지정가를 내 조금씩 담고, 창이 끝나면 잔량을 시장가로 낸다 (protocol.md §4.1.3).
	// ★ 호가에 남는 지정가(maker)가 아니다 — 미체결분은 바로 취소된다.
	ExecSweep = "sweep"
)

type Entry struct {
	Mode       string    `json:"mode"` // market | limit | sweep
	LimitPrice float64   `json:"limit_price,omitempty"`
	NotAfter   time.Time `json:"not_after"` // ★ 필수. 넘으면 진입만 포기한다.
	// RefPrice — 사이징 기준가. ★ 신호를 낸 쪽이 아는 값을 실어 보내면
	// "신호를 낸 가격" 과 "사이징한 가격" 이 갈리지 않는다 (그리고 시세 왕복이 사라진다).
	RefPrice float64 `json:"ref_price,omitempty"`
	// MaxSlipBp — sweep 이면 창 끝 잔량 시장가의 상한 (cap_price 대비). 매도1 이 이걸 넘으면 잔량을 포기한다.
	MaxSlipBp int `json:"max_slip_bp,omitempty"`
	// CapPrice · SweepUntil — mode=sweep 의 상한가와 창 끝 (둘 다 필수 · sweep_until ≤ not_after).
	// ★ 값은 신호원이 정한다 (stop 과 같은 원칙 — 콕핏은 산식을 모른다).
	CapPrice   float64    `json:"cap_price,omitempty"`
	SweepUntil *time.Time `json:"sweep_until,omitempty"`
}

// ExitExec — want=flat 청산의 집행 방식. 지금은 sweep 하나다 (매수1 ≥ floor_price 면 IOC 매도 · 창 끝 잔량 시장가).
type ExitExec struct {
	Mode       string    `json:"mode"` // sweep
	FloorPrice float64   `json:"floor_price"`
	SweepUntil time.Time `json:"sweep_until"`
}

type Exit struct {
	StopPrice  float64    `json:"stop_price"` // ★ 로직이 아니라 숫자
	TpPrice    float64    `json:"tp_price,omitempty"`
	TpDelegate bool       `json:"tp_delegate,omitempty"`
	TimeExitAt *time.Time `json:"time_exit_at,omitempty"`
	// HoldFrac — 이 로트를 **처음 산 수량의 몇 %만 남길지** (분할매도, 2026-09-30). 0~1, 선택.
	//
	// ★ 줄이기만 한다 — 이미 그 이하로 들고 있으면 아무것도 안 한다(되사지 않는다). 그래서 같은
	// 스냅샷이 다시 와도 두 번 팔지 않는다(목표상태 멱등). 0 은 want=flat 과 같다.
	// 남길 수량 = round(최초 진입 수량 × hold_frac) 을 주문 단위로.
	HoldFrac *float64 `json:"hold_frac,omitempty"`
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
	// AppliedSeqByScope — 발행 범위(scope)별로 적용된 seq (2026-09-30).
	// ★ 발행자는 **자기 scope 의 값**과 비교해야 한다. 전역 applied_seq 는 여러 소스 중 최댓값이라,
	// 전략이 여럿이면 남의 seq 를 자기 것으로 읽고 "콕핏이 앞섰다" 고 오판한다 (flat6 publisher 가 읽는다).
	AppliedSeqByScope map[string]uint64 `json:"applied_seq_by_scope,omitempty"`
	Guards            Guards            `json:"guards"`
	Positions         []Position        `json:"positions"`
	// ★ omitempty 를 붙이지 않는다: "유령 없음(빈 배열)" 과 "안 봤음(필드 부재)" 은 다른 말이다.
	Orphans []Symbol `json:"orphans"`
	// RecentCloses — 최근(30분) 종결된 로트 (2026-09-30).
	// ★ 발행자는 positions 만 보면 **상태 조회 사이에 열리고 닫힌 로트**(빠른 TP·stop)를 영영 못 본다 —
	//   flat6 dummy 통합 테스트에서 진입 1.4초 만에 TP 로 닫혀 발행자가 "진입 대기" 에 갇혔다.
	RecentCloses []ClosedLot `json:"recent_closes"`
	// Account — 계좌가 불어나는지 줄어드는지. ★ 조회 실패 시 nil 이고, **0 으로 채우지 않는다**
	// (잔고 0 과 «못 봤음» 을 합치면 사용자가 파산한 줄 안다).
	Account *Account `json:"account,omitempty"`
}

// ClosedLot — 종결된 로트 하나의 결말.
type ClosedLot struct {
	IntentID string `json:"intent_id"`
	Kid      string `json:"kid,omitempty"`
	Scope    string `json:"scope,omitempty"`
	Group    string `json:"group,omitempty"`
	Symbol   Symbol `json:"symbol"`
	// Reason — flat · time · stop · tp · reduce · derisk · manual · unknown(매수 결과 미상)
	Reason string `json:"reason"`
	// Price — 마지막 청산 체결가. 0 = 모름 (브로커 사후 감지 등).
	Price       float64   `json:"price,omitempty"`
	RealizedPct float64   `json:"realized_pct,omitempty"`
	At          time.Time `json:"at"`
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
	IntentID string  `json:"intent_id"`
	Group    string  `json:"group,omitempty"`
	Slot     string  `json:"slot,omitempty"`
	Symbol   Symbol  `json:"symbol"`
	Qty      float64 `json:"qty"`
	// EntryQty — 이 로트를 **처음 산 수량**. 분할매도(hold_frac)의 기준이다. 0 = 모름(옛 로트).
	EntryQty      float64    `json:"entry_qty,omitempty"`
	AvgEntryPrice float64    `json:"avg_entry_price"`
	EntryAt       *time.Time `json:"entry_at,omitempty"`
	StopArmed     float64    `json:"stop_armed,omitempty"`
	// TpArmed — **브로커에 실제로 걸어 둔** TP 지정가. ★ StopArmed 의 짝이다. 이게 없어서
	// "이미 건 값과 같은가" 를 비교할 수 없었고, 그래서 TP 는 **매 틱 취소·재발행**됐다
	// (2026-08-17 실측: 몇 분 만에 paper-tp-38 → 334). 라이브에선 주문 유량 문제이자, 더
	// 나쁘게는 취소와 재발행 **사이에 TP 가 브로커에 없는 창**이 생긴다 — 하필 그 층의
	// 존재 이유가 "데몬도 서버도 죽어도 이건 체결된다" 이다.
	TpArmed   float64 `json:"tp_armed,omitempty"`
	TpOrderID string  `json:"tp_order_id,omitempty"`
	// TimeExitAt — 이 시각이 되면 콕핏이 **스스로** 판다 (데드맨). 시세가 없어도 동작한다.
	TimeExitAt *time.Time `json:"time_exit_at,omitempty"`
	// Kid · Scope — 이 포지션을 만든 소스 (서명키 × 발행 범위). ★ 이 소스의 목표만 이 포지션을
	// 건드린다 — 다른 소스의 스냅샷이 이걸 유령으로 신고하거나 팔지 않는다 (pitwall §4).
	Kid           string  `json:"kid,omitempty"`
	Scope         string  `json:"scope,omitempty"`
	UnrealizedPct float64 `json:"unrealized_pct,omitempty"`
	// Pending — 진입 주문이 접수됐지만 아직 체결 확정 전이다 (수량·가격은 추정). 2026-10-01.
	Pending bool `json:"pending,omitempty"`
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
	// Exec — 집행 방식이 sweep 이었던 체결의 내역 (그 외엔 없음). qty·price 는 모든 조각을 합친 값이다.
	Exec *ExecReport `json:"exec,omitempty"`
}

// ExecReport — sweep 한 번의 집행 내역 (원장 orders.exec · event.order.exec).
//
// ★ 이게 없으면 «sweep 이 바탕(즉시 시장가)보다 나았나» 를 실전 체결로 잴 수 없다 — 그게 이 집행의 존재 이유다.
type ExecReport struct {
	Mode       string     `json:"mode"`                  // sweep
	CapPrice   float64    `json:"cap_price,omitempty"`   // 매수 — 받은 값 그대로
	FloorPrice float64    `json:"floor_price,omitempty"` // 매도 — 받은 값 그대로
	SweepUntil *time.Time `json:"sweep_until,omitempty"` // 콕핏이 실제로 쓴 창 끝 (상한으로 당겼으면 그 값)
	// RefAsk1 · RefBid1 — 집행을 시작한 순간 콕핏이 본 매도1 · 매수1 (바탕 «즉시 시장가» 와의 차를 재는 기준).
	RefAsk1 float64    `json:"ref_ask1,omitempty"`
	RefBid1 float64    `json:"ref_bid1,omitempty"`
	RefAt   *time.Time `json:"ref_at,omitempty"`
	// Fills — 체결된 조각 (IOC 와 창 끝 잔량). 0 체결 IOC 는 OrderIDs 에만 있다.
	Fills []SweepFill `json:"fills,omitempty"`
	// OrderIDs — 이 집행이 낸 증권사 주문번호 전부 (0 체결 포함). 콕핏 밖 매도 판별·증권사 원장 대조에 쓴다.
	OrderIDs []string `json:"order_ids,omitempty"`
	// IOCOrders — 낸 IOC 수 (0 체결 포함). RestQty · RestPrice — 창 끝 시장가로 산/판 수량과 평단.
	IOCOrders int     `json:"ioc_orders,omitempty"`
	RestQty   float64 `json:"rest_qty,omitempty"`
	RestPrice float64 `json:"rest_price,omitempty"`
	// Ended — 창을 닫은 사유: all(다 참) · window(창 끝) · stop · time · session · slip · short(돈 부족) ·
	// unknown(조각 결과 미상) · error.
	Ended  string `json:"ended,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// SweepFill — 조각 하나의 체결 (주문번호 단위 합).
type SweepFill struct {
	OrderID string    `json:"order_id"`
	Kind    string    `json:"kind"` // ioc | rest
	At      time.Time `json:"at"`
	Price   float64   `json:"price"`
	Qty     float64   `json:"qty"`
	FeeKRW  float64   `json:"fee_krw,omitempty"`
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
