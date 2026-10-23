# 콕핏 운영 — 무인으로 돌리기

## 감시자로 띄운다 (`cockpitsup`)

```
cockpitsup -data-dir C:/cockpit/data -- --broker kiwoom --mode live --data-dir C:/cockpit/data --acct acc_... [플래그]
```

- 콕핏이 죽으면 다시 띄운다 (5초 → 금방 죽기를 반복하면 15초 → 60초).
- **살아 있는데 멈추면** 죽이고 다시 띄운다 — 하트비트(`{data-dir}/heartbeat.json`)의 `last_tick`(집행 루프가 마지막으로 돈 시각)이 3분 멈추면.
- 10분에 5번 재시작이면 **30분 쉰다** + 텔레그램 알림 (같은 원인으로 죽으며 주문을 반복하지 않게).
- 텔레그램 키는 콕핏과 같은 `.env` 에서 읽는다 (감시자 실행 폴더 기준).

윈도 작업 스케줄러 (로그온 시 시작, 감시자가 죽으면 1분 뒤 다시):

```
schtasks /Create /TN "cockpit" /SC ONLOGON /RL LIMITED /TR "C:\cockpit\cockpitsup.exe -data-dir C:\cockpit\data -- --broker kiwoom --mode live --data-dir C:\cockpit\data"
```

★ 기계를 옮길 땐 **옛 기계의 자동 시작부터 끈다** — 두 기계가 같은 계좌에 붙는다 (reflex 2026-07-19, 10분간 이중 접속).

## 맥에서 돌리기 (`scripts/macbook.sh`)

빌드하는 PC 에서 SSH 로 맥을 부린다. 접속 정보는 저장소 루트 `.env.macbook` (gitignore — `MAC_SSH=user@host.local`,
mDNS 가 안 풀릴 때 `MAC_SSH_FALLBACK="user@192.168.x.y"`). 맥에 Go 는 필요 없다 (PC 에서 darwin/amd64 로 빌드).

★ **paper 와 live 는 따로 도는 두 인스턴스다** (2026-10-17). 모드를 늘 명시한다 — 기본값이 있으면 paper 를 만지려다 live 를 건드린다.

| | paper | live |
|---|---|---|
| launchd 라벨 | `com.yovel.cockpit.paper` | `com.yovel.cockpit.live` |
| 포트 | 7737 | 7738 |
| data-dir = 실행 폴더 | `~/cockpit/paper` | `~/cockpit/live` |
| 로그 | `~/cockpit/logs/cockpit-paper.log` | `~/cockpit/logs/cockpit-live.log` |

| 명령 | 하는 일 |
|---|---|
| `smoke` | 맥 임시 폴더에서 키·`.env` 없이 paper 로 띄워 health·집행 루프·SIGTERM 정지를 보고 지운다 |
| `push` | 빌드 → `~/cockpit/releases/<sha>` → `~/cockpit/bin` 이 가리킨다 (재시작 안 함 · 커밋 안 한 변경이 있으면 거부) · 도는 release 는 안 지운다 |
| `install paper\|live [-- 플래그]` | launchd 설정을 `~/cockpit/<모드>` 에 만들어만 둔다 · live 는 `COCKPIT_LIVE_OK=1` 필요 |
| `start` / `stop` / `restart` `paper\|live` | start = 설정에 **지금 release 를 박아** `~/Library/LaunchAgents` 에 넣고 켠다 (재부팅 뒤에도 뜬다) · stop = 끄고 뺀다 · live 의 start·restart 는 `COCKPIT_LIVE_OK=1` 필요 |
| `status [paper\|live]` | 모드마다 launchd·도는 release·health·하트비트·`.env`·장부 수·로그 끝 + reflex supervisor·잠자기·메모리·시계 |
| `tunnel [paper\|live]` | 이 PC `127.0.0.1:<그 모드 포트>` → 맥 콕핏 (끊기면 5초 뒤 다시) + API 토큰 사본 (paper `data/macbook/api-token` · live `data/macbook/live.api-token`) |

- 콕핏·감시자는 실행 폴더의 `.env` 를 읽는다 → 인스턴스마다 `~/cockpit/<모드>/.env` (키 `*_FILE` 경로·텔레그램 · install 이
  `.env.example` 을 남긴다). ★ **paper 는 live 의 키움 키를 못 본다** — 키당 토큰 1 이라 paper 가 키를 쥐면 live 의 토큰을 끊는다.
  계정 핸들은 install 플래그 `--acct` 로 준다 (paper `acc_macpaper` · live `acc_maclive` — 다르면 잘못 보낸 목표가 `E_ACCT` 로 튕긴다).
- 옛 단일 인스턴스(라벨 `com.yovel.cockpit` · `~/cockpit/install.env`)는 같은 모드의 `restart`·`stop` 이 넘겨받는다 —
  끄고 자동 시작을 빼고 옛 설정을 `~/cockpit/old/` 로 치운다. 옛 로그 `logs/cockpit.log` 는 그 모드 로그로 이름을 바꾸고
  옛 경로는 링크로 남긴다 (다른 세션·도구가 그 경로를 읽는다).
- flat6(PC) 는 `FLAT6_COCKPIT_URL=http://127.0.0.1:7737` · `FLAT6_COCKPIT_TOKEN_FILE=<저장소>/data/macbook/api-token`.
  터널이 끊기면 목표가 낡아 **신규 진입만** 멈춘다 — stop·시간청산은 맥에서 원장으로 계속 돈다.
  flat6 가 맥에서 돌면 터널 없이 맥 `127.0.0.1:<포트>` · 토큰 `~/cockpit/<모드>/api-token` 에 바로 붙는다.
- 터널 자동 시작 (PC 로그온 시):
  `schtasks /Create /TN "cockpit-tunnel" /SC ONLOGON /RL LIMITED /TR "\"C:\Program Files\Git\bin\bash.exe\" -lc '<저장소>/scripts/macbook.sh tunnel paper >> ~/cockpit-tunnel.log 2>&1'"`
- 맥 IP 가 DHCP 로 바뀐 적이 있다 — 공유기에서 맥 IP 를 고정해 두면 폴백이 늘 맞는다.
- ★ **키움 키는 한 곳에서만.** `start` 는 그 인스턴스의 키가 reflex `.env` 의 키움 키와 같고 reflex supervisor 에 `kiwoom`·`us` 가
  있으면 거부한다 — reflex 는 국내봇과 미장봇이 같은 키를 쓴다 (2026-10-15 실측). paper 도 키를 주면 시세를 그 키로 받으니 마찬가지.
  flat6 설정(`~/flat6/.env*` 의 피드 주 키·추가 키·캐시 키)에 같은 키가 있어도 거부한다 — 피드가 지금 꺼져 있어도 다시 뜨면
  서로를 끊으니, 키를 넘길 땐 flat6 설정에서 먼저 빼고 그 키의 옛 토큰 파일도 정리한다. 키는 기계를 가리지 않으니
  **이 PC 의 flat6 설정**(`LOCAL_KIWOOM_ENVS`, 기본 `/c/flat6/app/.env* /c/flat6/feed/*.env` — 수집기·캐시·피드)도
  지문만 맥으로 넘겨 같이 본다.
- 로그는 모드마다 한 파일 (하루 수 MB) — 회전은 아직 안 한다.

## 껐다 켜면 — 무엇이 복구되나

원장(`{data-dir}/cockpit.db`)에서 되살린다. 데몬은 아무 데서나 죽어도 된다 (전원·`kill -9`·감시자의 재기동).

| 복구되는 것 | 어떻게 |
|---|---|
| 열린 로트 (수량·평단·stop·TP 주문번호·시간청산·전략) | 원장의 로트 — 실계좌는 브로커 조회로 대조 |
| 종결된 목표 (재진입 금지) · pause·서킷브레이커 | 원장 |
| 체결 대기 주문 | 진행 중 주문 원장 — 이어서 추적 |
| ★ 보내던 중 죽은 주문 | 보내기 전 기록 — 진입은 결과 미상(재매수 금지), 청산은 1분 쉬고 다시 |
| 꺼진 동안의 TP 체결·HTS 매도 | 주문번호·체결 내역으로 귀속 (체결가로 닫는다) |
| 소스별 seq · de-risk nonce | 원장 — 재시작 직후 옛 서명 메시지를 다시 받지 않는다 |
| 최근 종결 (`recent_closes`) | 원장에서 30분치를 다시 만든다 |
| paper 계좌 (예수금·보유·TP) | paper 원장에서 다시 세운다 (시드 + 현금 흐름) |

재시작 직후 flat6 의 다음 발행(수 초)까지는 목표가 비어 있다 — 그동안 새 진입은 없고, stop·시간청산은 원장으로 계속 돈다.

★ **paper 와 live 는 data-dir 을 나눈다.** 로트는 모드(paper|live)를 기억하고, 다른 모드로 연 로트가 열려 있으면
기동을 거부한다 — paper 로트를 live 데몬이 이어받으면 flat 신호에 **live 계좌의 진짜 주식을 판다** (2026-10-15 실측).

`RESTART=1 scripts/e2e-flat6-fakekiwoom.sh` — 보유 중에 콕핏을 `kill -9` 하고 다시 띄워 장부 ↔ 가짜 키움 실보유를 대조한다.

## 증권사 원장 — 장 뒤 대조

live 키움 콕핏은 거래일 **15:50 KST** 에 그날 키움 원장을 받아 자기 원장과 주문번호로 맞대 보고
`{data-dir}/broker/broker_ledger_<YYYYMMDD>.json` 에 남긴다. 결과는 텔레그램 한 줄이다
(일치면 🧾, 어긋나면 ⚠️ 와 앞의 몇 건). 15:50 뒤에 켜져도 자정 전이면 그날 것을 따라잡는다.

- 파일 = 키움 응답 **그대로** (`sections[].pages[]`) + `check`. 콕핏이 다시 계산하지 않는다 (`internal/broker/kiwoom/ledger.go`).

| 조회 | 담긴 것 |
|---|---|
| ka10170 당일매매일지 | 종목별 매수·매도 수량·평단·금액 · 수수료+세금 · 손익 (당일 매도분 기준) |
| ka10074 일자별실현손익 | 계좌 하루 합 — 실현손익 · 수수료 · 세금 (**실제 청구**) |
| ka10076 체결 | 주문번호별 체결·주문별 수수료·세금 (★ 오늘 것만 준다) |
| kt00007 계좌별주문체결내역상세 | 주문번호별 체결 (지난 날짜도 됨 · 수수료 없음) |
| kt00015 위탁종합거래내역 | 매매 거래내역 — 결제일 기준이라 거래일 행의 수수료는 0 · ★ 계좌주 이름이 실린다 |

- `check` — 일치 수 · 불일치(같은 주문번호, 다른 수량·가격·수수료) · 콕핏에만 · 장부 밖(HTS 손주문 등) ·
  수수료 증권사 − 콕핏 차 · 증권사 실현손익.
- ★ **콕핏 원장은 고치지 않는다.** 주문별 값(수량·가격·수수료)은 체결 직후 ka10076 으로 이미 키움 것이다
  (10/20~23 119건 전부 일치). 다른 건 수수료뿐이다 — 주문별 수수료는 주문마다 10원 미만을 잘라 오고(작은 주문은 0),
  실제 청구는 하루 거래 합에 매긴다 (10/20~23 콕핏 4,076 vs 키움 4,586원). 계좌 기준 손익은 ka10074 를 쓴다.
- 지난 날짜·손으로 뽑기: `brokerctl -broker kiwoom ledger <YYYYMMDD> -out <파일>` (data-dir 에서). 조회만 하고
  **토큰을 발급하지 않는다** — 도는 데몬의 토큰 파일을 빌려 쓰고, 만료·무효면 멈춘다. 지난 날짜는 ka10076 대신 kt00007 로 맞댄다.

## 잠금 — 주문을 낼 수 있는 프로세스는 하나

| 잠금 | 위치 | 막는 것 |
|---|---|---|
| data-dir | `{data-dir}/cockpitd.lock` | 같은 원장을 둘이 쓰기 |
| 계좌 | `{UserConfigDir}/yovel-cockpit/locks/<브로커>-<키 해시>.lock` | data-dir 이 달라도 **같은 브로커 키**로 둘 뜨기 |
| 감시자 | `{data-dir}/cockpitsup.lock` | 감시자 둘 (= 콕핏 둘) |

잠금이 살아 있다 = 그 pid 가 돌고 있고 **하트비트가 3분 안**. 죽거나 멈춘 주인의 잠금은 넘겨받는다.

## 라이브 전 확인할 플래그

| 플래그 | 기본 | 뜻 |
|---|---|---|
| `--krx-exit-cutoff` | `15:15` | KRX 시간청산 상한 — 15:20 부터 동시호가라 장중 매도는 그 전에 |
| `--entry-fill-timeout` | `60s` | 진입 주문 체결 마감 — 넘으면 잔량 취소 |
| `--daily-loss-limit` | `0`(끔) | 오늘 실현손실 한도(원) — 닿으면 신규 진입만 멈춤, 다음 날 자동 해제 |
| `--block-stock-status` | 거래정지·관리종목·정리매매·투자위험·투자경고·단기과열 | 이 상태 종목엔 진입 안 함 |
| `--ref-price-max-dev` | `0.15` | 신호가가 시세와 ±15% 넘게 어긋나면 진입 거절 |
| `--stop-max-price-age` | `3m` | 로컬 stop 은 마지막 체결이 이보다 늙은 시세로 판정 안 함 |
| `--holidays-file` | 내장 2026 | KRX 휴장일 — 30일 안에 끝나면 기동 때 알림 |
| `--ignore-market-hours` | — | ★ 테스트 전용 (실계좌 거부) |
