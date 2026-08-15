# 서명 적합성 픽스처

두 언어가 **같은 바이트를 만드는지** 확인한다.

```bash
go test ./internal/protocol -run TestSigningFixtures    # Go (cockpit)
python schema/fixtures/signing/verify.py                # Python (flat6)
```

## 왜 필요한가

Ed25519 는 결정적이다 — 같은 입력이면 어떤 구현이든 같은 서명이 나온다.
그러므로 두 구현이 갈릴 수 있는 곳은 서명 알고리즘이 아니라 **정규화(RFC 8785 JCS)** 뿐이다:
키 정렬 · 공백 · 유니코드 이스케이프 · 숫자 표기.

★ 그리고 그 차이는 **조용하다.** 평범한 메시지에서는 우연히 일치하다가, 한글이 섞이거나
소수점이 등장하는 순간 "가끔 실패하는 인증" 이 된다. 그래서 골치 아픈 케이스만 골라 박아뒀다.

| 케이스 | 무엇을 잡나 |
|---|---|
| `basic-target` | 여기서 갈리면 나머지는 볼 것도 없다 |
| `key-order-and-whitespace` | JCS 가 제 일을 하는가 (basic 과 같은 바이트가 나와야 함) |
| `korean-and-escapes` | ★ 제일 잘 갈리는 곳. 한글은 `\u` 이스케이프하지 않고 UTF-8 로 쓴다 |
| `numbers` | ES6 숫자 표기 (`71200.0` → `71200`, `1e-07` → `1e-7`) |
| `unknown-fields` | 서버가 늘린 필드가 있어도 서명이 살아야 한다 |

## 키

★ **키 바이트를 파일에 저장하지 않는다.** 테스트용이라도 공개 저장소에 키를 넣는 습관 자체가
사고의 씨앗이고, 비밀 스캐너에도 걸린다. 대신 양쪽이 같은 문자열에서 결정적으로 유도한다:

```
ed25519_seed = sha256("yovel-jcs-fixture-v1")
```

공개키가 픽스처의 `public_key_b64` 와 다르면 유도 규칙이 깨진 것이다.

## 다시 만들 때

계약이 바뀌어 픽스처를 갱신해야 하면:

```bash
go test ./internal/protocol -run TestSigningFixtures -update
python schema/fixtures/signing/verify.py   # ★ 반드시 파이썬으로도 다시 확인
```

★ Go 만 돌리고 넘어가면 "Go 가 Go 를 검증" 한 것뿐이다. 이 픽스처의 존재 이유가 사라진다.

## flat6 쪽

`verify.py` 의 `canonicalize()` / `signing_input()` / `sign_envelope()` 를 그대로 가져다 쓴다.
의존성은 `cryptography` 하나. 그리고 이 검증을 flat6 CI 에도 건다 — 한쪽만 검사하면
반대쪽이 조용히 흘러간다.
