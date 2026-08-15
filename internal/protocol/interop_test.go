package protocol

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// 교차 언어 서명 적합성.
//
// ★ 이 테스트가 지키는 것: **Python(flat6)이 서명한 것을 Go(cockpit)가 검증할 수 있는가.**
// Ed25519 는 결정적이라 같은 입력이면 같은 서명이 나온다. 그러므로 두 구현이 갈리는 곳은
// 서명 알고리즘이 아니라 **정규화(JCS)** 뿐이다 — 키 정렬·공백·유니코드 이스케이프·숫자 표기.
//
// 그 차이는 조용하다: 대부분의 메시지에서 우연히 일치하다가, 한글이 섞이거나 소수점이
// 등장하는 순간 "가끔 실패하는 인증" 이 된다. 그래서 픽스처를 저장소에 박아두고
// **양쪽 구현이 같은 바이트를 만드는지** 각자 자기 테스트에서 확인한다.
//
// 재생성: go test ./internal/protocol -run TestSigningFixtures -update

var update = flag.Bool("update", false, "픽스처를 다시 만든다")

const fixturePath = "../../schema/fixtures/signing/cases.json"

// fixtureSeed — ★ 키를 파일에 저장하지 않는다. 공개 저장소에 키 바이트를 넣으면
// (테스트용이라도) 그 습관 자체가 사고의 씨앗이고, 비밀 스캐너도 걸린다.
// 대신 양쪽이 같은 문자열에서 결정적으로 유도한다.
const fixtureSeedSource = "yovel-jcs-fixture-v1"

func fixtureKey() (ed25519.PublicKey, ed25519.PrivateKey) {
	sum := sha256.Sum256([]byte(fixtureSeedSource))
	priv := ed25519.NewKeyFromSeed(sum[:])
	return priv.Public().(ed25519.PublicKey), priv
}

type signingCase struct {
	Name string `json:"name"`
	Why  string `json:"why"`
	// Message — sig 를 뺀 봉투. 키 순서와 공백은 일부러 흐트러뜨려 둔다.
	Message json.RawMessage `json:"message"`
	// SigningInput — JCS 정규화 결과 (사람이 읽을 수 있게 그대로).
	SigningInput string `json:"signing_input"`
	// SigningInputSHA256 — 바이트 단위 비교용.
	SigningInputSHA256 string `json:"signing_input_sha256"`
	// Signature — base64. Ed25519 는 결정적이라 구현이 달라도 같아야 한다.
	Signature string `json:"signature"`
}

type fixtureFile struct {
	Note       string        `json:"note"`
	SeedSource string        `json:"seed_source"`
	PublicKey  string        `json:"public_key_b64"`
	Kid        string        `json:"kid"`
	Cases      []signingCase `json:"cases"`
}

// 픽스처의 원재료. ★ 골치 아픈 것만 골라 넣는다 — 쉬운 케이스는 어차피 우연히 맞는다.
var fixtureMessages = []struct {
	name string
	why  string
	raw  string
}{
	{
		"basic-target",
		"평범한 목표 스냅샷. 여기서 갈리면 나머지는 볼 것도 없다.",
		`{"v":1,"typ":"intent.target","id":"01J9Z8QK3M7X2ABCDEFGHJKMNP","acct":"acc_7f3a",
		  "ts":"2026-08-15T00:05:00.000Z","exp":"2026-08-15T00:06:00.000Z","nonce":"b7d1e2f0","seq":10432,
		  "body":{"as_of_bar":"2026-08-15T09:05:00+09:00","book_state":"normal","targets":[
		    {"intent_id":"01J9Z8QK3M7X2ABCDEFGHJKMNQ","slot":"gapdown_a",
		     "symbol":{"exchange":"KRX","code":"005930"},"side":"long","want":"open","weight":0.14,
		     "entry":{"mode":"market","not_after":"2026-08-15T00:06:00.000Z","max_slip_bp":80},
		     "exit":{"stop_price":71200,"tp_price":78400,"tp_delegate":true}}]}}`,
	},
	{
		"key-order-and-whitespace",
		"키 순서를 뒤집고 공백을 넣었다. JCS 가 제 일을 하면 basic-target 과 같은 바이트가 나와야 한다.",
		`{
		   "body" : { "targets" : [ { "want":"open", "weight":0.14, "slot":"gapdown_a",
		       "exit" : { "tp_delegate":true, "tp_price":78400, "stop_price":71200 },
		       "entry": { "max_slip_bp":80, "not_after":"2026-08-15T00:06:00.000Z", "mode":"market" },
		       "symbol": { "code":"005930", "exchange":"KRX" }, "side":"long",
		       "intent_id":"01J9Z8QK3M7X2ABCDEFGHJKMNQ" } ],
		     "book_state":"normal", "as_of_bar":"2026-08-15T09:05:00+09:00" },
		   "seq":10432, "nonce":"b7d1e2f0", "exp":"2026-08-15T00:06:00.000Z",
		   "ts":"2026-08-15T00:05:00.000Z", "acct":"acc_7f3a",
		   "id":"01J9Z8QK3M7X2ABCDEFGHJKMNP", "typ":"intent.target", "v":1
		 }`,
	},
	{
		"korean-and-escapes",
		"한글·이모지·따옴표·역슬래시. ★ 여기가 두 구현이 제일 잘 갈리는 지점이다 — " +
			"JCS 는 \\u 이스케이프를 풀고 UTF-8 로 쓰며, 제어문자만 이스케이프한다.",
		`{"v":1,"typ":"cmd.derisk","id":"01J9Z8QK3M7X2ABCDEFGHJKMNS","acct":"acc_7f3a",
		  "ts":"2026-08-15T02:10:00.000Z","exp":"2026-08-15T02:15:00.000Z","nonce":"9a1c77de",
		  "body":{"action":"pause","scope":"all",
		    "reason":"운영자: 슬리피지 \"이상\" 확인 중 \\ 경로 · 탭\t줄바꿈\n★"}}`,
	},
	{
		"numbers",
		"정수·소수·0·음수·큰 수. ★ 숫자 표기는 ES6 규칙을 따른다 (71200.0 은 71200 으로 쓴다).",
		`{"v":1,"typ":"event.order","id":"01J9Z8QK3M7X2ABCDEFGHJKMNV","acct":"acc_7f3a",
		  "ts":"2026-08-15T02:31:44.900Z",
		  "body":{"intent_id":"01J9Z8QK3M7X2ABCDEFGHJKMNQ","phase":"exit_filled",
		    "symbol":{"exchange":"KRX","code":"005930"},"side":"sell",
		    "qty":14,"price":71180,"slippage_bp":-2.8,"fee_krw":2490,"realized_pct":-0.0128,
		    "zero":0,"big":1234567890123}}`,
	},
	{
		"unknown-fields",
		"서버가 나중에 늘린 필드. 옛 클라가 몰라도 서명은 살아야 한다 — " +
			"구조체로 왕복시켜 서명 입력을 만들면 이 케이스가 깨진다.",
		`{"v":1,"typ":"heartbeat","id":"01J9Z8QK3M7X2ABCDEFGHJKMNW","acct":"acc_7f3a",
		  "ts":"2026-08-15T02:32:00.000Z","future_top":"모르는 값",
		  "body":{"seq":5821,"daemon_sha":"a1b2c3d","broker_ws":"up",
		    "future_nested":{"a":[1,2,{"b":null}],"c":true}}}`,
	},
}

func TestSigningFixtures(t *testing.T) {
	pub, priv := fixtureKey()

	if *update {
		f := fixtureFile{
			Note: "yovel v1 서명 적합성 픽스처. 양쪽 구현(Go/Python)이 같은 바이트를 만드는지 확인한다. " +
				"키는 저장하지 않고 seed_source 에서 유도한다: ed25519_seed = sha256(seed_source).",
			SeedSource: fixtureSeedSource,
			PublicKey:  base64.StdEncoding.EncodeToString(pub),
			Kid:        "fixture-1",
		}
		for _, m := range fixtureMessages {
			var compact any
			if err := json.Unmarshal([]byte(m.raw), &compact); err != nil {
				t.Fatalf("%s: 픽스처 원문이 JSON 이 아니다: %v", m.name, err)
			}
			normalized, err := json.Marshal(compact)
			if err != nil {
				t.Fatal(err)
			}
			input, err := SigningInput(normalized)
			if err != nil {
				t.Fatalf("%s: %v", m.name, err)
			}
			sum := sha256.Sum256(input)
			f.Cases = append(f.Cases, signingCase{
				Name: m.name, Why: m.why,
				Message:            json.RawMessage(normalized),
				SigningInput:       string(input),
				SigningInputSHA256: base64.StdEncoding.EncodeToString(sum[:]),
				Signature:          base64.StdEncoding.EncodeToString(ed25519.Sign(priv, input)),
			})
		}
		b, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(fixturePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixturePath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("픽스처 %d건 기록: %s", len(f.Cases), fixturePath)
		return
	}

	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("픽스처가 없다 (-update 로 생성): %v", err)
	}
	var f fixtureFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.PublicKey != base64.StdEncoding.EncodeToString(pub) {
		t.Fatalf("픽스처의 공개키가 seed 에서 유도한 것과 다르다 — 유도 규칙이 바뀌었다")
	}
	if len(f.Cases) != len(fixtureMessages) {
		t.Fatalf("픽스처 %d건, 케이스 %d건 — -update 로 다시 만들 것", len(f.Cases), len(fixtureMessages))
	}

	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			input, err := SigningInput(c.Message)
			if err != nil {
				t.Fatal(err)
			}
			if string(input) != c.SigningInput {
				t.Fatalf("정규화 결과가 다르다\n  got:  %s\n  want: %s", input, c.SigningInput)
			}
			sum := sha256.Sum256(input)
			if got := base64.StdEncoding.EncodeToString(sum[:]); got != c.SigningInputSHA256 {
				t.Fatalf("정규화 바이트 해시 불일치: %s vs %s", got, c.SigningInputSHA256)
			}

			sig, err := base64.StdEncoding.DecodeString(c.Signature)
			if err != nil {
				t.Fatal(err)
			}
			if !ed25519.Verify(pub, input, sig) {
				t.Fatal("픽스처 서명 검증 실패")
			}

			// 실제 수신 경로로도 통과해야 한다: sig 를 붙여 VerifySignature 를 태운다.
			signed, err := Sign(c.Message, f.Kid, priv)
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifySignature(signed, envSigOf(t, signed),
				map[string]ed25519.PublicKey{f.Kid: pub}); err != nil {
				t.Fatalf("수신 경로 검증 실패: %v", err)
			}
		})
	}
}

func envSigOf(t *testing.T, raw []byte) *Signature {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env.Sig
}
