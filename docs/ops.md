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
