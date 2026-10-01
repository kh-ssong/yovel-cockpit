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
