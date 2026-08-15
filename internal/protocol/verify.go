package protocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gowebpki/jcs"
)

// SigningInput 은 서명 대상 바이트를 만든다: sig 를 제외한 봉투를 RFC 8785 (JCS) 로 정규화.
//
// ★ 수신한 원본 바이트에서 출발한다. 구조체로 왕복시키면 모르는 필드가 사라져 서명이 깨진다 —
// 그러면 서버가 필드를 하나도 늘릴 수 없게 된다.
//
// 부분 필드만 서명하지 않는 이유: 서명 안 된 필드로 의미를 바꾸는 공격이 열린다.
func SigningInput(raw []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("봉투 파싱 실패: %w", err)
	}
	delete(m, "sig")

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // JCS 는 <>& 를 이스케이프하지 않는다
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return jcs.Transform(bytes.TrimRight(buf.Bytes(), "\n"))
}

var (
	ErrUnknownKid  = errors.New("모르는 kid")
	ErrBadAlg      = errors.New("지원하지 않는 서명 알고리즘")
	ErrBadSigBytes = errors.New("서명 바이트가 Ed25519 크기가 아님")
	ErrSigMismatch = errors.New("서명 불일치")
)

// VerifySignature 는 원본 바이트에 대해 Ed25519 서명을 검증한다.
func VerifySignature(raw []byte, sig *Signature, keys map[string]ed25519.PublicKey) error {
	if sig == nil {
		return ErrSigMismatch
	}
	if sig.Alg != "ed25519" {
		return fmt.Errorf("%w: %s", ErrBadAlg, sig.Alg)
	}
	pub, ok := keys[sig.Kid]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownKid, sig.Kid)
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sig.Val)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadSigBytes, err)
	}
	if len(sigBytes) != ed25519.SignatureSize {
		return ErrBadSigBytes
	}
	input, err := SigningInput(raw)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, input, sigBytes) {
		return ErrSigMismatch
	}
	return nil
}

// Sign 은 봉투에 서명을 붙인다. 데몬 자체는 서명할 일이 없지만 (진입 지시는 pitwall 만),
// 테스트와 로컬 루프백 서버가 정직하려면 같은 코드 경로가 필요하다.
func Sign(raw []byte, kid string, priv ed25519.PrivateKey) ([]byte, error) {
	input, err := SigningInput(raw)
	if err != nil {
		return nil, err
	}
	sig := Signature{
		Alg: "ed25519",
		Kid: kid,
		Val: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, input)),
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	sigRaw, err := json.Marshal(sig)
	if err != nil {
		return nil, err
	}
	m["sig"] = sigRaw
	return json.Marshal(m)
}

// Policy 는 클라가 다운링크를 받아들이는 규칙. 서버가 바꿀 수 없는 로컬 설정이다.
type Policy struct {
	// MaxSkew — |ts - now| 허용 오차. 넘으면 E_SKEW.
	MaxSkew time.Duration

	// AcceptUnsignedDerisk — ★ 명시적으로 켜고 끄는 손잡이 (protocol.md §2.2).
	// on  : 서명키를 잃어도 봇을 세울 탈출구가 있다. 대신 릴레이 침해 시 강제청산이 가능해진다.
	// off : 모든 다운링크가 서명 필수. 탈출구가 사라진다.
	// ★ 어느 쪽이든 진입은 서명 없이 절대 통과하지 않는다.
	AcceptUnsignedDerisk bool

	// TrustedKeys — kid → pitwall 공개키. 바이너리에 pin 된 값에서 온다.
	TrustedKeys map[string]ed25519.PublicKey

	// Acct — 이 콕핏이 자기 것으로 아는 계정 핸들. 다른 acct 의 봉투는 E_ACCT 로 거절한다.
	//
	// ★ 왜 서명만으로 부족한가 (다중 사용자에서만 생기는 공격면):
	// 릴레이는 서명키가 없으니 BUY 를 **지어낼** 수는 없다. 그런데 A 에게 갈 **진짜 서명된**
	// BUY 를 B 의 콕핏으로 **배달**할 수는 있다 — 서명은 통과한다(피트월이 실제로 서명했으니).
	// 토픽 ACL 오설정 하나, 캡처한 메시지 재전송 하나면 남의 목표가 내 계좌에서 집행된다.
	// 서명은 "누가 만들었나" 를 증명하지 초대는 "누구에게 가는 것인가" 를 증명하지 않는다.
	//
	// ★ 빈 값이면 검사하지 않는다 (릴레이 이전의 루프백 개발). 대신 데몬이 기동 시 경고한다 —
	// 조용히 무방비인 상태를 조용히 두지 않는다. 릴레이 등록이 붙으면 이 값이 자동으로 찬다.
	Acct string
}

func DefaultPolicy() Policy {
	return Policy{
		MaxSkew:              120 * time.Second,
		AcceptUnsignedDerisk: true,
		TrustedKeys:          map[string]ed25519.PublicKey{},
	}
}

// Admission 은 다운링크 한 통에 대한 판정이다.
//
// ★ Accept 와 EntryAllowed 가 따로 있는 게 이 타입의 핵심이다.
// 만료된 intent.target 은 버리는 게 아니라 "진입만 죽고 exit 은 계속 유효한" 상태가 된다.
// 만료된 stop 은 무방비이므로, 청산을 만료시키는 설계는 §0 의 비대칭에 정면으로 어긋난다.
type Admission struct {
	Env          *Envelope
	Accept       bool
	EntryAllowed bool
	Ignored      bool // 조용히 무시 (stale seq — 에러가 아니다)
	Codes        []RejectCode
}

func (a Admission) Status() string {
	switch {
	case a.Ignored:
		return "ignored"
	case !a.Accept:
		if hasCode(a.Codes, CodeExpired) {
			return "expired"
		}
		return "rejected"
	case len(a.Codes) > 0:
		return "partial"
	default:
		return "applied"
	}
}

func hasCode(codes []RejectCode, c RejectCode) bool {
	for _, x := range codes {
		if x == c {
			return true
		}
	}
	return false
}

func reject(codes ...RejectCode) Admission {
	return Admission{Codes: codes}
}

// Guard 는 seq / nonce 상태를 들고 재생과 되감기를 막는다.
//
// ★ seq 와 nonce 를 헷갈리면 안 된다: retained 로 같은 intent.target 이 재접속마다 다시 오는 건
// 정상이므로 nonce 로 판정하면 정상 스냅샷을 replay 로 버리게 된다. 그래서 intent 는 seq,
// 일회성 명령은 nonce 로 나눈다.
type Guard struct {
	lastSeq map[Type]uint64
	nonces  map[string]time.Time
}

func NewGuard() *Guard {
	return &Guard{lastSeq: map[Type]uint64{}, nonces: map[string]time.Time{}}
}

func (g *Guard) LastSeq(t Type) uint64 { return g.lastSeq[t] }

func (g *Guard) forgetExpiredNonces(now time.Time) {
	for n, exp := range g.nonces {
		if now.After(exp) {
			delete(g.nonces, n)
		}
	}
}

// Admit 은 수신한 다운링크 원본 바이트를 판정한다.
//
// 순서가 곧 보안이다: 서명을 먼저 검증하고 나서야 seq/nonce 상태를 건드린다.
// 반대로 하면 위조 메시지 한 통으로 seq 를 밀어올려 진짜 명령을 막을 수 있다.
func Admit(raw []byte, now time.Time, p Policy, g *Guard) Admission {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return reject(CodeSchema)
	}
	if env.V != Version {
		return reject(CodeSchema)
	}
	if env.ID == "" || env.Acct == "" || env.TS.IsZero() || len(env.Body) == 0 {
		return reject(CodeSchema)
	}

	// ★ 계정 바인딩 — 서명 검사보다 **앞**이다.
	// 남의 계정으로 온 봉투는 서명이 아무리 멀쩡해도 우리 것이 아니다. 그리고 여기서 끊으면
	// 그 메시지는 seq/nonce 상태를 건드리지 못한다 (남의 seq 로 내 카운터를 밀어올리는 것도 막힌다).
	if p.Acct != "" && env.Acct != p.Acct {
		return Admission{Env: &env, Codes: []RejectCode{CodeAcct}}
	}

	// 클라가 받아 처리하는 것은 다운링크뿐이다.
	switch env.Typ {
	case TypeIntentTarget, TypeCmdDerisk:
	default:
		// intent.position 은 예약만 되어 있고 구현하지 않는다 — 추측 실행 금지.
		return Admission{Env: &env, Codes: []RejectCode{CodeUnsupportedType}}
	}

	if p.MaxSkew > 0 {
		if d := now.Sub(env.TS); d > p.MaxSkew || d < -p.MaxSkew {
			return Admission{Env: &env, Codes: []RejectCode{CodeSkew}}
		}
	}

	// ── 서명 (상태 변경 전) ──
	switch env.Typ {
	case TypeIntentTarget:
		// ★ 진입을 담을 수 있는 타입은 서명이 필수다. 릴레이 장악만으론 BUY 를 못 만든다.
		if err := VerifySignature(raw, env.Sig, p.TrustedKeys); err != nil {
			return Admission{Env: &env, Codes: []RejectCode{CodeSig}}
		}
		if env.Seq == nil {
			return Admission{Env: &env, Codes: []RejectCode{CodeSchema}}
		}
	case TypeCmdDerisk:
		if env.Sig != nil {
			if err := VerifySignature(raw, env.Sig, p.TrustedKeys); err != nil {
				return Admission{Env: &env, Codes: []RejectCode{CodeSig}}
			}
		} else if !p.AcceptUnsignedDerisk {
			return Admission{Env: &env, Codes: []RejectCode{CodeSig}}
		}
		if env.Nonce == "" || env.Exp == nil {
			return Admission{Env: &env, Codes: []RejectCode{CodeSchema}}
		}
	}

	// ── 본문 (스키마가 강제하는 것을 Go 쪽에서도 강제한다) ──
	// ★ 두 언어에서 계약이 같은 것을 의미하는지가 여기서 갈린다. JSON Schema 만 믿으면
	// 스키마를 안 거치는 경로(테스트 픽스처·수동 발행)로 들어온 메시지가 무검사 통과한다.
	if codes := validateBody(&env); len(codes) > 0 {
		return Admission{Env: &env, Codes: codes}
	}

	// ── 순서·재생 ──
	switch env.Typ {
	case TypeIntentTarget:
		if *env.Seq <= g.LastSeq(TypeIntentTarget) {
			// 재접속 시 retained 로 같은 메시지가 다시 오는 건 정상 동작이다.
			return Admission{Env: &env, Ignored: true}
		}
	case TypeCmdDerisk:
		g.forgetExpiredNonces(now)
		if _, seen := g.nonces[env.Nonce]; seen {
			return Admission{Env: &env, Codes: []RejectCode{CodeReplay}}
		}
	}

	// ── 만료 ──
	adm := Admission{Env: &env, Accept: true, EntryAllowed: true}
	expired := env.Exp != nil && now.After(*env.Exp)

	switch env.Typ {
	case TypeIntentTarget:
		if expired {
			// ★ 진입만 죽는다. exit·stop 은 계속 유효하다.
			adm.EntryAllowed = false
			adm.Codes = append(adm.Codes, CodeExpired)
		}
	case TypeCmdDerisk:
		if expired {
			return Admission{Env: &env, Codes: []RejectCode{CodeExpired}}
		}
	}

	// 여기까지 왔을 때만 상태를 갱신한다.
	switch env.Typ {
	case TypeIntentTarget:
		g.lastSeq[TypeIntentTarget] = *env.Seq
	case TypeCmdDerisk:
		g.nonces[env.Nonce] = *env.Exp
	}
	return adm
}

// validateBody 는 schema/v1 의 필수·enum 제약 중 안전에 직결되는 것을 Go 에서 다시 건다.
func validateBody(env *Envelope) []RejectCode {
	switch env.Typ {
	case TypeIntentTarget:
		var it IntentTarget
		if err := json.Unmarshal(env.Body, &it); err != nil {
			return []RejectCode{CodeSchema}
		}
		if it.AsOfBar.IsZero() {
			return []RejectCode{CodeSchema}
		}
		for _, t := range it.Targets {
			if t.IntentID == "" || t.Symbol.Code == "" || t.Slot == "" {
				return []RejectCode{CodeSchema}
			}
			switch t.Want {
			case WantFlat:
			case WantOpen:
				// ★ stop 없는 진입, 만료 없는 진입은 프로토콜 레벨에서 막는다.
				if t.Entry == nil || t.Exit == nil {
					return []RejectCode{CodeSchema}
				}
				if t.Entry.NotAfter.IsZero() || t.Exit.StopPrice <= 0 {
					return []RejectCode{CodeSchema}
				}
				if t.Weight <= 0 || t.Weight > 1 {
					return []RejectCode{CodeSchema}
				}
				if t.Entry.Mode == "limit" && t.Entry.LimitPrice <= 0 {
					return []RejectCode{CodeSchema}
				}
			default:
				return []RejectCode{CodeSchema}
			}
		}
		return nil

	case TypeCmdDerisk:
		var c CmdDerisk
		if err := json.Unmarshal(env.Body, &c); err != nil {
			return []RejectCode{CodeSchema}
		}
		switch c.Action {
		case DeriskLiquidate, DeriskPause, DeriskBlockEntry, DeriskResume:
		default:
			// ★ 여기가 "명령 채널에 buy 는 없다" 가 코드로 서 있는 지점이다.
			return []RejectCode{CodeSchema}
		}
		switch c.Scope {
		case "all":
		case "slot":
			if c.Slot == "" {
				return []RejectCode{CodeSchema}
			}
		case "position":
			if c.IntentID == "" {
				return []RejectCode{CodeSchema}
			}
		default:
			return []RejectCode{CodeSchema}
		}
		if c.Action == DeriskBlockEntry && c.Until == nil {
			return []RejectCode{CodeSchema}
		}
		return nil
	}
	return nil
}

// TargetAction 은 reconcile 이 target 하나에 대해 할 일.
type TargetAction string

const (
	ActionEnter TargetAction = "enter"
	ActionExit  TargetAction = "exit"
	ActionNoop  TargetAction = "noop"
)

// AdmitTarget 은 target 하나의 진입 자격을 본다.
//
// entryAllowed 는 두 곳에서 꺼진다: 봉투 만료(Admission.EntryAllowed) 와
// 스냅샷 노화(target_max_age). 어느 쪽이든 청산은 막지 않는다.
func AdmitTarget(t Target, now time.Time, entryAllowed bool) (TargetAction, []RejectCode) {
	if t.Want == WantFlat {
		return ActionExit, nil // ★ 청산은 만료·노화와 무관하게 언제나 유효
	}
	if t.Want != WantOpen {
		return ActionNoop, []RejectCode{CodeSchema}
	}
	if t.Entry == nil || t.Exit == nil || t.Exit.StopPrice <= 0 {
		// stop 없이 진입시키는 목표는 받지 않는다.
		return ActionNoop, []RejectCode{CodeSchema}
	}
	if !entryAllowed {
		return ActionNoop, []RejectCode{CodeExpired}
	}
	if !t.Entry.NotAfter.IsZero() && now.After(t.Entry.NotAfter) {
		return ActionNoop, []RejectCode{CodeExpired}
	}
	return ActionEnter, nil
}

// Stale 은 목표 스냅샷이 늙어서 진입을 막아야 하는지 본다.
// ★ 이게 "연결이 끊긴 순간 무엇이 남는가" 의 직접적 구현이다 — 진입은 죽고 exit 은 산다.
func Stale(asOf time.Time, now time.Time, maxAge time.Duration) bool {
	return maxAge > 0 && now.Sub(asOf) > maxAge
}
