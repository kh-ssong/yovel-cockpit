package protocol

import (
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"
)

const testKid = "pw-test"

func testKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, map[string]ed25519.PublicKey) {
	t.Helper()
	// 고정 시드 — 테스트가 실행마다 달라지면 안 된다.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return pub, priv, map[string]ed25519.PublicKey{testKid: pub}
}

var baseTS = time.Date(2026, 8, 15, 0, 5, 0, 0, time.UTC)

// targetEnv 는 서명 전 intent.target 봉투를 만든다.
func targetEnv(seq uint64, ts, exp, notAfter time.Time) map[string]any {
	return map[string]any{
		"v":     1,
		"typ":   string(TypeIntentTarget),
		"id":    "01J9Z8QK3M7X2ABCDEFGHJKMNP",
		"acct":  "acc_7f3a",
		"ts":    ts,
		"exp":   exp,
		"nonce": "b7d1e2f0",
		"seq":   seq,
		"body": map[string]any{
			"as_of_bar":  ts,
			"book_state": "normal",
			"targets": []any{
				map[string]any{
					"intent_id": "01J9Z8QK3M7X2ABCDEFGHJKMNQ",
					"slot":      "gapdown_a",
					"symbol":    map[string]any{"exchange": "KRX", "code": "005930"},
					"side":      "long",
					"want":      "open",
					"weight":    0.14,
					"entry":     map[string]any{"mode": "market", "not_after": notAfter},
					"exit":      map[string]any{"stop_price": 71200},
				},
			},
		},
	}
}

func deriskEnv(nonce string, ts, exp time.Time, action string) map[string]any {
	return map[string]any{
		"v":     1,
		"typ":   string(TypeCmdDerisk),
		"id":    "01J9Z8QK3M7X2ABCDEFGHJKMNS",
		"acct":  "acc_7f3a",
		"ts":    ts,
		"exp":   exp,
		"nonce": nonce,
		"body":  map[string]any{"action": action, "scope": "all"},
	}
}

func marshal(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func signed(t *testing.T, m map[string]any, priv ed25519.PrivateKey) []byte {
	t.Helper()
	b, err := Sign(marshal(t, m), testKid, priv)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func policyWith(t *testing.T) (Policy, ed25519.PrivateKey) {
	t.Helper()
	_, priv, keys := testKeys(t)
	p := DefaultPolicy()
	p.TrustedKeys = keys
	return p, priv
}

// ── 서명 ────────────────────────────────────────────────────────────────────

// 서명은 키 순서·공백에 흔들리면 안 된다. 흔들리면 Python 서버와 Go 클라가
// 같은 문서에 다른 서명을 내고, 그건 "가끔 실패하는 인증"이라는 최악의 형태로 나타난다.
func TestSigningInputIsCanonical(t *testing.T) {
	a := []byte(`{"v":1,"typ":"heartbeat","body":{"b":2,"a":1}}`)
	b := []byte("{\n  \"body\": { \"a\": 1, \"b\": 2 },\n  \"typ\": \"heartbeat\",\n  \"v\": 1\n}")

	ia, err := SigningInput(a)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := SigningInput(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ia) != string(ib) {
		t.Fatalf("정규화가 안 됐다:\n  %s\n  %s", ia, ib)
	}
}

// ★ forward compat 의 실제 시험: 서버가 나중에 필드를 늘려도 옛 클라의 서명 검증이 살아야 한다.
// 구조체로 왕복시켜 서명 대상을 만들면 모르는 필드가 사라져 여기서 깨진다.
func TestSignatureSurvivesUnknownFields(t *testing.T) {
	p, priv := policyWith(t)

	m := targetEnv(1, baseTS, baseTS.Add(time.Minute), baseTS.Add(time.Minute))
	m["future_field"] = "서버가 나중에 추가한 것"
	body := m["body"].(map[string]any)
	body["future_body_field"] = map[string]any{"nested": true}

	raw := signed(t, m, priv)
	if err := VerifySignature(raw, envSig(t, raw), p.TrustedKeys); err != nil {
		t.Fatalf("모르는 필드가 있다고 서명이 깨졌다: %v", err)
	}
	adm := Admit(raw, baseTS.Add(time.Second), p, NewGuard())
	if !adm.Accept {
		t.Fatalf("모르는 필드 때문에 거절됨: %v", adm.Codes)
	}
}

func TestTamperedFieldBreaksSignature(t *testing.T) {
	p, priv := policyWith(t)
	raw := signed(t, targetEnv(1, baseTS, baseTS.Add(time.Minute), baseTS.Add(time.Minute)), priv)

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// stop 가격만 슬쩍 바꾼다 — 중계자가 할 수 있는 가장 조용한 공격이다.
	var body map[string]any
	if err := json.Unmarshal(m["body"], &body); err != nil {
		t.Fatal(err)
	}
	body["targets"].([]any)[0].(map[string]any)["exit"].(map[string]any)["stop_price"] = 1.0
	nb, _ := json.Marshal(body)
	m["body"] = nb
	tampered, _ := json.Marshal(m)

	adm := Admit(tampered, baseTS.Add(time.Second), p, NewGuard())
	if adm.Accept || !hasCode(adm.Codes, CodeSig) {
		t.Fatalf("변조가 통과했다: accept=%v codes=%v", adm.Accept, adm.Codes)
	}
}

func envSig(t *testing.T, raw []byte) *Signature {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env.Sig
}

// ── 신뢰 모델 ───────────────────────────────────────────────────────────────

// ★ 이 저장소가 공개인 이유가 걸린 성질: 릴레이를 장악해도 진입을 지어낼 수 없다.
func TestUnsignedEntryNeverPasses(t *testing.T) {
	for _, acceptUnsigned := range []bool{true, false} {
		p, _ := policyWith(t)
		p.AcceptUnsignedDerisk = acceptUnsigned

		raw := marshal(t, targetEnv(1, baseTS, baseTS.Add(time.Minute), baseTS.Add(time.Minute)))
		adm := Admit(raw, baseTS.Add(time.Second), p, NewGuard())
		if adm.Accept || !hasCode(adm.Codes, CodeSig) {
			t.Fatalf("accept_unsigned_derisk=%v 에서 미서명 진입이 통과했다: %v", acceptUnsigned, adm.Codes)
		}
	}
}

// 미서명 de-risk 는 손잡이가 정한다 — 그리고 그 손잡이는 진입에 영향을 주지 않는다.
func TestUnsignedDeriskFollowsPolicy(t *testing.T) {
	cases := []struct {
		accept bool
		want   bool
	}{{true, true}, {false, false}}

	for _, c := range cases {
		p, _ := policyWith(t)
		p.AcceptUnsignedDerisk = c.accept

		raw := marshal(t, deriskEnv("n1", baseTS, baseTS.Add(5*time.Minute), "pause"))
		adm := Admit(raw, baseTS.Add(time.Second), p, NewGuard())
		if adm.Accept != c.want {
			t.Fatalf("accept_unsigned_derisk=%v → accept=%v (기대 %v, codes=%v)",
				c.accept, adm.Accept, c.want, adm.Codes)
		}
	}
}

// ── 만료의 비대칭 ───────────────────────────────────────────────────────────

// ★ 이 프로토콜에서 제일 중요한 한 줄: 만료된 목표는 버리는 게 아니라 진입만 죽는다.
// 만료된 stop 은 무방비이므로, 통째로 거절하면 청산이 사라진다.
func TestExpiredTargetKeepsExitsAlive(t *testing.T) {
	p, priv := policyWith(t)
	exp := baseTS.Add(time.Minute)
	raw := signed(t, targetEnv(1, baseTS, exp, exp), priv)

	now := exp.Add(30 * time.Second) // 만료 후
	adm := Admit(raw, now, p, NewGuard())

	if !adm.Accept {
		t.Fatalf("만료됐다고 목표를 통째로 버렸다 — 청산이 사라진다 (codes=%v)", adm.Codes)
	}
	if adm.EntryAllowed {
		t.Fatal("만료됐는데 진입이 살아 있다")
	}
	if !hasCode(adm.Codes, CodeExpired) {
		t.Fatalf("만료를 보고하지 않았다: %v", adm.Codes)
	}
	if adm.Status() != "partial" {
		t.Fatalf("status=%q, 기대 partial", adm.Status())
	}
}

// 반면 de-risk 는 시점 명령이라 만료되면 실행하지 않는다.
func TestExpiredDeriskIsRejected(t *testing.T) {
	p, _ := policyWith(t)
	exp := baseTS.Add(time.Minute)
	raw := marshal(t, deriskEnv("n1", baseTS, exp, "liquidate"))

	adm := Admit(raw, exp.Add(time.Second), p, NewGuard())
	if adm.Accept || !hasCode(adm.Codes, CodeExpired) {
		t.Fatalf("만료된 de-risk 가 통과했다: accept=%v codes=%v", adm.Accept, adm.Codes)
	}
}

// ── 순서·재생 ───────────────────────────────────────────────────────────────

func TestStaleSeqIsIgnoredNotRejected(t *testing.T) {
	p, priv := policyWith(t)
	g := NewGuard()
	exp := baseTS.Add(time.Minute)

	first := signed(t, targetEnv(10, baseTS, exp, exp), priv)
	if adm := Admit(first, baseTS.Add(time.Second), p, g); !adm.Accept {
		t.Fatalf("첫 메시지가 거절됨: %v", adm.Codes)
	}
	// retained 로 같은 메시지가 재접속마다 다시 오는 건 정상이다.
	adm := Admit(first, baseTS.Add(2*time.Second), p, g)
	if !adm.Ignored {
		t.Fatalf("재수신을 무시하지 않았다: accept=%v codes=%v", adm.Accept, adm.Codes)
	}
	if adm.Status() != "ignored" {
		t.Fatalf("status=%q, 기대 ignored", adm.Status())
	}
	if len(adm.Codes) != 0 {
		t.Fatalf("정상 동작인데 에러 코드를 냈다: %v", adm.Codes)
	}
}

// ★ 순서가 곧 보안이다: 서명을 먼저 보지 않으면 위조 한 통으로 seq 를 밀어올려
// 그 뒤의 진짜 명령을 전부 stale 로 만들 수 있다 (조용한 서비스 거부).
func TestForgedMessageCannotBumpSeq(t *testing.T) {
	p, priv := policyWith(t)
	g := NewGuard()
	exp := baseTS.Add(time.Hour)

	forged := marshal(t, targetEnv(9999, baseTS, exp, exp)) // 서명 없음
	if adm := Admit(forged, baseTS.Add(time.Second), p, g); adm.Accept {
		t.Fatal("위조가 통과했다")
	}
	if got := g.LastSeq(TypeIntentTarget); got != 0 {
		t.Fatalf("위조가 seq 를 %d 로 밀어올렸다", got)
	}

	real := signed(t, targetEnv(1, baseTS, exp, exp), priv)
	if adm := Admit(real, baseTS.Add(2*time.Second), p, g); !adm.Accept {
		t.Fatalf("진짜 명령이 막혔다: %v", adm.Codes)
	}
}

func TestDeriskNonceReplayRejected(t *testing.T) {
	p, _ := policyWith(t)
	g := NewGuard()
	exp := baseTS.Add(5 * time.Minute)
	raw := marshal(t, deriskEnv("same-nonce", baseTS, exp, "liquidate"))

	if adm := Admit(raw, baseTS.Add(time.Second), p, g); !adm.Accept {
		t.Fatalf("첫 명령이 거절됨: %v", adm.Codes)
	}
	adm := Admit(raw, baseTS.Add(2*time.Second), p, g)
	if adm.Accept || !hasCode(adm.Codes, CodeReplay) {
		t.Fatalf("재생이 통과했다: accept=%v codes=%v", adm.Accept, adm.Codes)
	}
}

func TestSkewRejected(t *testing.T) {
	p, priv := policyWith(t)
	exp := baseTS.Add(time.Hour)
	raw := signed(t, targetEnv(1, baseTS, exp, exp), priv)

	adm := Admit(raw, baseTS.Add(10*time.Minute), p, NewGuard())
	if adm.Accept || !hasCode(adm.Codes, CodeSkew) {
		t.Fatalf("시계 오차가 통과했다: accept=%v codes=%v", adm.Accept, adm.Codes)
	}
}

func TestUplinkTypesAreNotAccepted(t *testing.T) {
	p, _ := policyWith(t)
	for _, typ := range []Type{TypeStateSnapshot, TypeHeartbeat, TypeAck, TypeIntentPosition} {
		m := map[string]any{
			"v": 1, "typ": string(typ), "id": "01J9Z8QK3M7X2ABCDEFGHJKMNP",
			"acct": "acc_7f3a", "ts": baseTS, "body": map[string]any{},
		}
		adm := Admit(marshal(t, m), baseTS.Add(time.Second), p, NewGuard())
		if adm.Accept || !hasCode(adm.Codes, CodeUnsupportedType) {
			t.Fatalf("%s: accept=%v codes=%v", typ, adm.Accept, adm.Codes)
		}
	}
}

// ── target 단위 판정 ────────────────────────────────────────────────────────

func TestAdmitTarget(t *testing.T) {
	notAfter := baseTS.Add(time.Minute)
	open := Target{
		IntentID: "x", Slot: "s", Symbol: Symbol{Exchange: "KRX", Code: "005930"},
		Side: "long", Want: WantOpen, Weight: 0.1,
		Entry: &Entry{Mode: "market", NotAfter: notAfter},
		Exit:  &Exit{StopPrice: 100},
	}
	flat := Target{IntentID: "y", Slot: "s", Want: WantFlat}

	noStop := open
	noStop.Exit = &Exit{StopPrice: 0}

	cases := []struct {
		name         string
		target       Target
		now          time.Time
		entryAllowed bool
		want         TargetAction
		wantCode     RejectCode
	}{
		{"정상 진입", open, baseTS, true, ActionEnter, ""},
		{"not_after 경과 → 진입 포기", open, notAfter.Add(time.Second), true, ActionNoop, CodeExpired},
		{"봉투 만료 → 진입 포기", open, baseTS, false, ActionNoop, CodeExpired},
		{"stop 없는 진입 거부", noStop, baseTS, true, ActionNoop, CodeSchema},
		// ★ 청산은 만료·노화·연결상태와 무관하게 언제나 유효하다.
		{"만료돼도 청산은 산다", flat, notAfter.Add(time.Hour), false, ActionExit, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			act, codes := AdmitTarget(c.target, c.now, c.entryAllowed)
			if act != c.want {
				t.Fatalf("action=%s, 기대 %s (codes=%v)", act, c.want, codes)
			}
			if c.wantCode != "" && !hasCode(codes, c.wantCode) {
				t.Fatalf("codes=%v, 기대 %s", codes, c.wantCode)
			}
			if c.wantCode == "" && len(codes) != 0 {
				t.Fatalf("코드가 없어야 하는데 %v", codes)
			}
		})
	}
}

func TestSafeBookStateFailsSafe(t *testing.T) {
	// 모르는 값을 normal 로 해석하면, 서버가 새 상태를 도입한 순간 옛 클라가
	// "정상"으로 알아듣고 계속 산다. 반대 방향으로 틀려야 한다.
	if got := SafeBookState(BookState("panic_mode_v2")); got != BookHalted {
		t.Fatalf("모르는 book_state → %s, 기대 halted", got)
	}
	if got := SafeBookState(BookNormal); got != BookNormal {
		t.Fatalf("normal 이 %s 로 바뀜", got)
	}
}

func TestStale(t *testing.T) {
	if !Stale(baseTS, baseTS.Add(200*time.Second), 180*time.Second) {
		t.Fatal("늙은 스냅샷을 stale 로 안 봄")
	}
	if Stale(baseTS, baseTS.Add(10*time.Second), 180*time.Second) {
		t.Fatal("멀쩡한 스냅샷을 stale 로 봄")
	}
}
