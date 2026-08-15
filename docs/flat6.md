# flat6 ↔ cockpit 연결

> flat6 가 **판단(pitwall 역할)**, 콕핏이 **집행**을 맡는다.
> 둘 다 같은 PC 에서 도므로 **릴레이(MQTT)가 필요 없다** — 로컬 HTTP 한 방이면 끝난다.

```
flat6                                              cockpit
조건식 IN → 스코어 → TargetSnapshot
                        │ Ed25519 서명
                        └── POST /v1/downlink ──▶ Admit → reconcile → 키움 주문
                                                        │
                                                  원장 · stop · TP 위임
```

---

## 0. 왜 이 모양인가

flat6 의 `execution/contract.py` 는 처음부터 이 계약의 미러로 쓰였고,
`execution/reconciler.py` 는 *"★ 나중에 콕핏(Go)이 이 자리를 가져간다"* 라고 적혀 있다.
즉 이 연결은 새 설계가 아니라 **예정된 자리 바꾸기**다.

★ 다만 자리를 바꾸면 flat6 의 `Reconciler` / `KiwoomBroker` 는 **비활성**이 되어야 한다.
둘 다 살려두면 그게 산식 이중 구현이고, 그때부터 "어느 쪽이 진실인가" 를 매번 물어야 한다.
참조 구현·테스트용으로는 남겨도 되지만 **런루프에서는 빠진다.**

---

## 1. 먼저 정리해야 하는 것 — 키움 토큰

★ 이게 제일 조용하고 제일 아픈 함정이다.

키움은 **1계정 1토큰**이다. flat6 와 콕핏이 같은 앱키로 각자 발급하면 서로의 토큰을 죽인다.
증상은 "가끔 8005" 가 아니라 **상대 세션이 통째로 유실되는** 형태다
(reflex 실측: 8초 사이 3회 재발급 → 다른 봇이 개장부터 504건 연속 실패).

두 선택지뿐이다:

| | 방법 |
|---|---|
| **A. 파일 공유** (권장) | 콕핏을 flat6 의 토큰 파일로 붙인다. 포맷은 이미 맞춰뒀다 (`expires_dt`) |
| B. 앱키 분리 | 콕핏에 별도 앱키를 발급한다. 계좌가 같으면 여전히 위험하니 확인 필요 |

```bash
cockpitd --broker kiwoom \
         --kiwoom-token-file ../yovel-flat6/data/kiwoom_token.json
```

양쪽 모두 `get(force=True)` 를 **"내 토큰이 죽었다는 신고"** 로 다루지 발급 명령으로 다루지
않는다(파일의 토큰이 내 것과 다르면 그걸 채택하고 끝낸다). 그래서 파일만 공유하면
서로 force 하는 핑퐁도 일어나지 않는다. 회귀 gate = `TestSharedTokenFileMeansOneIssuance`.

★ **WS 는 콕핏이 안 쓴다.** 콕핏은 REST 전용이라 "앱키당 WS 세션 1개" 제약과 충돌하지 않는다.
시세 스트림은 flat6 가 계속 독점한다.

---

## 2. 서명 — 이게 성립해야 나머지가 의미 있다

콕핏은 **서명된 intent 만** 진입으로 받아들인다. flat6 가 서명자(pitwall)다.

```bash
# 1) 콕핏에서 키를 만든다 (개발용). 운영 키는 flat6 가 보관한다.
go run ./cmd/devsign keygen --kid flat6-1 --out flat6-key.json
# 출력된 {"flat6-1": "<공개키>"} 를 콕핏의 {data-dir}/trusted_keys.json 에 저장
```

flat6 쪽 구현은 **새로 짤 필요가 없다** — 이 저장소의
[`schema/fixtures/signing/verify.py`](../schema/fixtures/signing/verify.py) 에 있는
`canonicalize()` / `signing_input()` / `sign_envelope()` 를 그대로 가져다 쓰면 된다.
의존성은 `cryptography` 하나뿐이다.

★ **반드시 픽스처를 flat6 CI 에 걸 것.** Ed25519 는 결정적이라 두 구현이 갈릴 수 있는 곳은
서명이 아니라 **정규화(JCS)** 뿐이고, 그 차이는 조용하다 — 평범한 메시지에서는 우연히
일치하다가 한글이 섞이거나 소수점이 등장하는 순간 "가끔 실패하는 인증" 이 된다.

```bash
python schema/fixtures/signing/verify.py   # Go 가 만든 픽스처와 바이트 단위 대조
```

현재 5 케이스(기본 / 키순서·공백 / 한글·이스케이프 / 숫자 / 모르는 필드) + 공개키 유도가
Go ↔ Python 양쪽에서 통과한다.

---

## 3. 보낼 것 — 목표상태 스냅샷

flat6 의 `TargetSnapshot` 을 봉투에 싣고 서명해 POST 한다.

```
POST http://127.0.0.1:7737/v1/downlink
Authorization: Bearer <{data-dir}/api-token 의 내용>
Content-Type: application/json

{ "v":1, "typ":"intent.target", "id":"<ULID>", "acct":"acc_...",
  "ts":"...Z", "exp":"...Z", "seq":<단조증가>, "sig":{...},
  "body": { "as_of_bar":"...", "book_state":"normal", "targets":[ ... ] } }
```

지켜야 할 것 넷:

1. **완성봉마다 전체 스냅샷을 재발행한다.** 델타를 보내지 않는다 — 재발행이 무해한 게
   이 프로토콜의 핵심이고, 유실돼도 다음 스냅샷이 복구한다.
2. **`seq` 는 단조증가.** 같거나 작은 seq 는 콕핏이 조용히 무시한다(에러가 아니다).
3. **진입에는 `not_after` 필수.** 없으면 스키마 위반이다.
4. ★ **`entry.ref_price` 를 실어라.** flat6 는 틱 스트림으로 가격을 이미 알고 있다.
   안 실으면 콕핏이 종목마다 REST 현재가를 다시 조회하는데, 왕복·유량도 문제지만
   진짜 문제는 **신호를 낸 가격과 사이징한 가격이 갈리는 것**이다.

응답은 `ack` 다. 거절도 HTTP 200 으로 오고 `codes` 에 이유가 담긴다
(MQTT 에는 상태코드가 없어서, 두 경로가 다른 모양이면 그게 곧 배선 버그가 된다).

★ 콕핏은 **다운링크를 받는 즉시 집행한다** — 다음 틱(기본 5초)을 기다리지 않는다.
스캘핑처럼 수명이 분 단위인 신호에서 5초는 신호를 통째로 무의미하게 만든다.

---

## 4. flat6 에서 빠지는 것

| 모듈 | 처분 |
|---|---|
| `execution/contract.py` | **유지** — 목표상태를 만드는 쪽은 계속 flat6 다 |
| `execution/reconciler.py` | 런루프에서 제외 (참조 구현으로 파일은 남겨도 됨) |
| `execution/kiwoom_broker.py` | 런루프에서 제외 — 주문은 콕핏만 낸다 |
| `execution/broker.py` (Paper) | 런루프에서 제외 — 콕핏의 paper 브로커가 대신한다 |
| 키움 WS · 스코어 · 틱 로거 | **유지** — flat6 의 본체다 |

★ 자리를 바꾸는 순간 **주문을 낼 수 있는 프로세스가 하나여야 한다.** 둘 다 낼 수 있는
상태로 잠깐이라도 두면 같은 신호로 두 번 사게 된다.

---

## 5. 붙이는 순서

1. flat6 에 서명 유틸 이식 + `verify.py` 픽스처를 CI 에 건다
2. 콕핏 `trusted_keys.json` 에 flat6 공개키 등록
3. 콕핏을 `--broker paper --kiwoom-token-file <flat6 파일>` 로 띄운다
   (paper 지만 시세·토큰은 진짜를 쓴다)
4. flat6 를 **집행 없는 모드**로 돌려 목표만 POST → 콕핏 `/v1/plan` · `/v1/ledger?mode=paper` 로 확인
5. 하루치 대조: flat6 가 낸 목표 수 ↔ 콕핏 ack 수 ↔ 원장 건수
6. 그 다음에야 `--broker kiwoom --mode live` + 소액

★ 4~5 단계를 건너뛰지 말 것. 여기서 걸리는 것은 대부분 계약이 아니라
**시각·seq·유효시각 같은 배선**이고, 그건 라이브에서 처음 만나면 훨씬 비싸다.

---

## 6. 아직 안 풀린 것

- **`acct` 핸들** — 릴레이가 발급하는 값인데 릴레이가 없다. 지금은 아무 값이나 쓰되
  양쪽이 같은 값을 쓰기만 하면 된다. 릴레이가 붙으면 진짜 핸들로 교체한다.
- **`slot_capital`** — 콕핏이 `--slot-capital` 하나로 전 슬롯에 같은 값을 쓴다.
  flat6 가 슬롯을 여러 개 쓰기 시작하면 슬롯별 자본 파일이 필요하다.
- **업링크** — 콕핏이 원장을 `outbox` 에 쌓지만 아직 아무 데도 안 보낸다.
  flat6 가 그걸 읽어 자기 회고에 쓸지, 릴레이가 붙을 때까지 둘지는 미정.
