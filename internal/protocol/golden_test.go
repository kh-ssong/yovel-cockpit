package protocol

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 이 테스트가 지키는 것: Go 타입이 schema/v1 과 같은 말을 하는가.
//
// ★ 스키마와 Go 구조체는 사람이 두 번 쓰는 것이라 반드시 어긋난다. 어긋난 걸 런타임에
// 발견하면 그건 라이브에서 필드 하나가 조용히 빈 값으로 들어오는 형태로 나타난다.
// 그래서 저장소의 진짜 예제 파일을 그대로 언마셜해 본다.

func examplePath(t *testing.T, kind string) string {
	t.Helper()
	p := filepath.Join("..", "..", "schema", "examples", kind)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("예제 디렉토리를 못 찾음 %s: %v", p, err)
	}
	return p
}

func readExamples(t *testing.T, kind string) map[string][]byte {
	t.Helper()
	dir := examplePath(t, kind)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	if len(out) == 0 {
		t.Fatalf("%s 에 예제가 없다", dir)
	}
	return out
}

// bodyFor 는 typ 에 맞는 빈 구조체를 준다.
func bodyFor(typ Type) any {
	switch typ {
	case TypeIntentTarget:
		return &IntentTarget{}
	case TypeCmdDerisk:
		return &CmdDerisk{}
	case TypeStateSnapshot:
		return &StateSnapshot{}
	case TypeEventOrder:
		return &EventOrder{}
	case TypeHeartbeat:
		return &Heartbeat{}
	case TypePresence:
		return &Presence{}
	case TypeAck:
		return &Ack{}
	}
	return nil
}

func TestGoldenExamplesRoundTrip(t *testing.T) {
	for name, raw := range readExamples(t, "valid") {
		t.Run(name, func(t *testing.T) {
			var env Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("봉투 언마셜 실패: %v", err)
			}
			if env.V != Version {
				t.Fatalf("v=%d, 기대 %d", env.V, Version)
			}
			target := bodyFor(env.Typ)
			if target == nil {
				t.Fatalf("모르는 typ %q — types.go 에 타입이 빠졌다", env.Typ)
			}
			if err := json.Unmarshal(env.Body, target); err != nil {
				t.Fatalf("body 언마셜 실패: %v", err)
			}
			round, err := json.Marshal(target)
			if err != nil {
				t.Fatal(err)
			}

			var orig, got any
			if err := json.Unmarshal(env.Body, &orig); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(round, &got); err != nil {
				t.Fatal(err)
			}
			// ★ 원본에 있는 모든 키·값이 왕복 후에도 남아 있어야 한다.
			// json 태그 오타는 정확히 여기서 잡힌다 (필드가 조용히 사라지므로).
			assertSubset(t, "", orig, got)
		})
	}
}

// assertSubset 은 want 의 모든 키·값이 got 안에 같은 값으로 존재하는지 본다.
func assertSubset(t *testing.T, path string, want, got any) {
	t.Helper()

	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: 객체가 아니다 (%T)", path, got)
			return
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok {
				t.Errorf("%s/%s: 왕복 후 사라짐 — json 태그를 확인할 것", path, k)
				continue
			}
			assertSubset(t, path+"/"+k, wv, gv)
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			t.Errorf("%s: 배열이 아니다 (%T)", path, got)
			return
		}
		if len(w) != len(g) {
			t.Errorf("%s: 길이 %d → %d", path, len(w), len(g))
			return
		}
		for i := range w {
			assertSubset(t, path+"/"+itoa(i), w[i], g[i])
		}
	case string:
		g, ok := got.(string)
		if !ok {
			t.Errorf("%s: 문자열이 아니다 (%T)", path, got)
			return
		}
		// 시각은 표현이 아니라 순간으로 비교한다
		// ("...T00:05:00.000Z" 와 "...T00:05:00Z" 는 같은 시각이다).
		if tw, err1 := time.Parse(time.RFC3339, w); err1 == nil {
			if tg, err2 := time.Parse(time.RFC3339, g); err2 == nil {
				if !tw.Equal(tg) {
					t.Errorf("%s: 시각 %s → %s", path, w, g)
				}
				return
			}
		}
		if w != g {
			t.Errorf("%s: %q → %q", path, w, g)
		}
	default:
		if want != got {
			t.Errorf("%s: %v → %v", path, want, got)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestInvalidExamplesAreRejected — 계약을 어긴 예제는 Go 쪽에서도 통과하면 안 된다.
//
// ★ 우리는 JSON Schema 를 안 거치는 경로로도 메시지를 받는다 (MQTT 페이로드는 그냥 바이트다).
// 스키마만 믿으면 검증기는 CI 에서만 돌고 라이브에서는 아무도 안 지키는 게 된다.
func TestInvalidExamplesAreRejected(t *testing.T) {
	// 예제마다 "이 이유로" 거절돼야 한다. 이유까지 고정하지 않으면 엉뚱한 규칙에 걸려
	// 거절되면서도 테스트는 초록인 상태가 된다 (= 정작 검사하려던 규칙은 죽어 있음).
	want := map[string]RejectCode{
		"intent.target-unsigned.json":                CodeSig,
		"intent.target-entry-without-not-after.json": CodeSchema,
		"intent.target-open-without-exit.json":       CodeSchema,
		"cmd.derisk-buy.json":                        CodeSchema,
		"cmd.derisk-slot-scope-without-slot.json":    CodeSchema,
		"envelope-wrong-major.json":                  CodeSchema,
		"state.snapshot-without-mode.json":           CodeUnsupportedType, // 업링크 타입 = 클라가 받을 것이 아님
		"ack-rejected-without-codes.json":            CodeUnsupportedType,
	}

	_, priv, keys := testKeys(t)
	p := DefaultPolicy()
	p.TrustedKeys = keys

	examples := readExamples(t, "invalid")
	for name := range want {
		if _, ok := examples[name]; !ok {
			t.Errorf("%s: 기대 목록에 있는데 예제 파일이 없다", name)
		}
	}

	for name, raw := range examples {
		t.Run(name, func(t *testing.T) {
			wantCode, ok := want[name]
			if !ok {
				t.Fatal("이 예제의 기대 거절 사유가 위 표에 없다 — 예제를 늘렸으면 표도 늘릴 것")
			}

			// 예제의 서명은 자리채움 더미다. 그대로 두면 전부 E_SIG 로 걸려
			// 정작 검사하려던 규칙(not_after 누락 등)이 한 번도 실행되지 않는다.
			raw = resignIfSigned(t, raw, priv)

			// 스큐가 아니라 의도한 규칙에서 걸리도록, 시계를 그 메시지 시각에 맞춘다.
			now := envTS(t, raw).Add(time.Second)

			adm := Admit(raw, now, p, NewGuard())
			if adm.Accept {
				t.Fatalf("거절돼야 하는데 통과함 (codes=%v)", adm.Codes)
			}
			if !hasCode(adm.Codes, wantCode) {
				t.Fatalf("거절 사유 %v, 기대 %s", adm.Codes, wantCode)
			}
		})
	}
}

// resignIfSigned 는 sig 가 있는 예제의 더미 서명을 테스트 키의 진짜 서명으로 갈아끼운다.
// sig 가 없는 예제(= 미서명 진입 케이스)는 그대로 둔다 — 그게 그 예제의 요지다.
func resignIfSigned(t *testing.T, raw []byte, priv ed25519.PrivateKey) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["sig"]; !ok {
		return raw
	}
	delete(m, "sig")
	stripped, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// kid 는 예제의 것(pw-2026-08)이 아니라 테스트 키의 것으로 바꾼다 —
	// 안 그러면 "모르는 kid" 로 걸려 또 E_SIG 가 되고, 검사하려던 규칙은 여전히 안 돈다.
	signed, err := Sign(stripped, testKid, priv)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func envTS(t *testing.T, raw []byte) time.Time {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env.TS
}
