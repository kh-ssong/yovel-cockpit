#!/usr/bin/env bash
# e2e — flat6(e2e 발행기) → 콕핏(--broker kiwoom) → 가짜 키움(스펙 기반, 체결까지).
#
#   scripts/e2e-flat6-fakekiwoom.sh            # 약 6분
#   RESTART=1 scripts/e2e-flat6-fakekiwoom.sh  # + 보유 중에 콕핏을 강제 종료(kill -9)하고 다시 띄운다
#
# 검증하는 것: 서명·seq·ack·상태 조회(flat6 ↔ 콕핏) + 주문·분할체결·청산(콕핏 ↔ 키움 API 모양)
#             + 장애 주입(주문은 들어갔는데 응답만 502) + 끝에 콕핏 장부 ↔ 가짜 키움 실상태 대조.
# 안 하는 것: 실제 키움 서버 동작 (→ 모의투자 mockapi / 소액 라이브).
#             TP 위임 — flat6 Desk 목표(TargetBook)는 TP 를 싣지 않는다 (tp 0 · 청산 = 같은 intent 의 want=flat).
#             TP 경로는 Go 테스트(internal/executor)가 맡는다.
#
# ★ 격리: 콕핏은 임시 폴더에서 떠서 저장소 .env(실키·실토큰 경로)를 읽지 않고, 가짜 키·가짜 토큰 파일을 쓴다.
#   flat6 은 임시 data-dir·e2e 전용 acct(acc_flat6e2e — flat6 가 e2e·dummy·test 가 든 계정만 받는다)·텔레그램 끔.
#   키움 WS·피드에 붙지 않고 서명키만 읽는다.
#
# 환경변수: FLAT6_DIR(기본 ../yovel-flat6) FLAT6_KEY(기본 $FLAT6_DIR/data/signing_key.json)
#           KIWOOM_SPEC(기본 ~/Downloads/kiwoom-rest-api-spec.json) PY(기본 python) OUT(기본 임시 폴더)
#           RESTART(1 이면 재시작 시나리오)
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FLAT6_DIR="${FLAT6_DIR:-$ROOT/../yovel-flat6}"
FLAT6_KEY="${FLAT6_KEY:-$FLAT6_DIR/data/signing_key.json}"
KIWOOM_SPEC="${KIWOOM_SPEC:-$HOME/Downloads/kiwoom-rest-api-spec.json}"
PY="${PY:-python}"
OUT="${OUT:-$(mktemp -d)}"
# ★ 가격표는 여기 한 곳 — 가짜 키움(-price)과 flat6 e2e 발행기(FLAT6_DUMMY_PRICES)에 **같은 값**을 넘긴다.
#   둘이 어긋나면 flat6 신호가 콕핏 가드(±15%)에 막힌다. 형식 = "코드=가격,코드=가격" — 틀리거나 종목이 빠지면
#   flat6 가 기본값으로 대신 돌지 않고 기동을 거부한다.
PRICES="${PRICES:-005930=71000,000660=195000,035720=41000}"
CP=7795; FK=7801
# ★ flat6 발행기 = `python -m flat6 desk e2e` (flat6 main 8e5e6b3e · 2026-10-15 — 이 해시로 보통·RESTART=1 PASS).
#   옛 dummy(3276172e)는 071ffcad 에서 지워졌고 이게 그 자리다 — Desk 와 같은 목표 코드(TargetBook)로 D-205 모양
#   목표를 실물 발행 경로로 보낸다. flat6 작업 트리가 다른 브랜치면 스냅샷을 풀어 쓴다 (flat6 저장소는 건드리지 않는다):
#     mkdir -p /tmp/f6 && git -C ../yovel-flat6 archive 8e5e6b3e | tar -x -C /tmp/f6
#     go run ./cmd/devsign keygen --kid flat6-e2e --out /tmp/e2e_key.json
#     FLAT6_DIR=/tmp/f6 FLAT6_KEY=/tmp/e2e_key.json scripts/e2e-flat6-fakekiwoom.sh
#   ★ 서명키는 목업 키(kid flat6-drill)면 flat6 가 거부한다 — devsign 으로 만든 키를 쓴다.
[ -f "$FLAT6_DIR/src/flat6/apps/desk/e2e.py" ] || { echo "★ $FLAT6_DIR 에 flat6 e2e 발행기(apps/desk/e2e.py)가 없다 — flat6 8e5e6b3e 이상 (스크립트 머리 주석)"; exit 1; }
[ -f "$FLAT6_KEY" ] || { echo "★ 서명키가 없다: $FLAT6_KEY — devsign keygen 으로 만들 것"; exit 1; }
EXE=""; case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe";; esac
A="http://127.0.0.1:$FK/_admin"
mkdir -p "$OUT/cockpit" "$OUT/flat6"
echo "출력: $OUT"

(cd "$ROOT" && go build -o "$OUT/cockpitd$EXE" ./cmd/cockpitd && go build -o "$OUT/fakekiwoom$EXE" ./cmd/fakekiwoom) || exit 1
"$PY" -c "import json,sys;k=json.load(open(sys.argv[1]));json.dump({k['kid']:k['public']},open(sys.argv[2],'w'))" \
  "$FLAT6_KEY" "$OUT/cockpit/trusted_keys.json" || exit 1

cd "$OUT"   # ★ 저장소 밖 — .env 를 읽지 않는다
PRICE_FLAGS=(); IFS=',' read -ra _P <<< "$PRICES"; for kv in "${_P[@]}"; do PRICE_FLAGS+=(-price "$kv"); done
"$OUT/fakekiwoom$EXE" -addr 127.0.0.1:$FK -spec "$KIWOOM_SPEC" -cash 10000000 "${PRICE_FLAGS[@]}" > "$OUT/fake.log" 2>&1 &
FPID=$!
for i in $(seq 1 20); do curl -s -m1 $A/state >/dev/null && break; sleep 0.5; done
curl -s -m1 $A/state >/dev/null || { echo "★ fakekiwoom 이 안 떴다"; cat "$OUT/fake.log"; exit 1; }

start_cockpit() {  # $1 = 로그 파일
  COCKPIT_KIWOOM_APPKEY=fake COCKPIT_KIWOOM_SECRET=fake COCKPIT_KIWOOM_TOKEN_FILE="$OUT/cockpit/kiwoom_token.json" \
  COCKPIT_TELEGRAM_BOT_TOKEN= \
  "$OUT/cockpitd$EXE" --data-dir "$OUT/cockpit" --port $CP --ui=false --acct acc_flat6e2e --engine-budget 6000000 \
    --broker kiwoom --kiwoom-api-url http://127.0.0.1:$FK --mode live --ignore-market-hours > "$1" 2>&1 &
  CPID=$!
  for i in $(seq 1 30); do curl -s -m1 http://127.0.0.1:$CP/v1/health >/dev/null && break; sleep 0.5; done
}
start_cockpit "$OUT/cockpit.log"
curl -s -X POST $A/scenario -d '{"fill":"split","chunks":3,"interval_ms":400}' >/dev/null

# --seconds 로 스스로 끝낸다 (마지막 seq 를 남기고 나간다) · timeout 은 걸렸을 때의 안전망.
(cd "$FLAT6_DIR" && PYTHONPATH=src PYTHONIOENCODING=utf-8 \
  FLAT6_DATA_DIR="$OUT/flat6" FLAT6_SIGNING_KEY_PATH="$FLAT6_KEY" \
  FLAT6_COCKPIT_URL=http://127.0.0.1:$CP FLAT6_COCKPIT_TOKEN_FILE="$OUT/cockpit/api-token" FLAT6_COCKPIT_ACCT=acc_flat6e2e \
  FLAT6_TELEGRAM_BOT_TOKEN= FLAT6_TELEGRAM_CHAT_ID= FLAT6_DUMMY_PRICES="$PRICES" \
  timeout 345 "$PY" -u -m flat6 desk e2e --seconds 320 --data-dir "$OUT/flat6/cockpit_e2e" > "$OUT/flat6.log" 2>&1) &
F6=$!
T0=$SECONDS

if [ "${RESTART:-}" = 1 ]; then
  # 체결이 끝난 로트가 생기면(최대 45초) 콕핏을 강제 종료하고 다시 띄운다 — 잠금 인계·로트 복구·seq 복구.
  # ★ 그 로트는 재시작 **뒤에** flat6 의 청산 목표(want=flat)로 팔려야 한다 — 재시작한 콕핏이 목표를 계속
  #   받아들이고(seq 복구) 되살린 로트를 체결가로 닫는지까지 본다. 죽일 때 든 intent 는 held_at_kill.txt 에.
  T=$(cat "$OUT/cockpit/api-token")
  # ★ print 를 쓰지 않는다 — Windows 에선 빈 목록에도 "\r\n" 을 써서 [ -s ] 가 바로 참이 된다 (로트 없이 t+1 에 죽였다).
  for i in $(seq 1 45); do
    curl -s -m1 -H "Authorization: Bearer $T" http://127.0.0.1:$CP/v1/state | "$PY" -c "import json,sys;sys.stdout.write(''.join(p['intent_id']+'\n' for p in json.load(sys.stdin).get('positions') or [] if p.get('qty',0)>0 and not p.get('pending')))" > "$OUT/held_at_kill.txt" 2>/dev/null
    [ -s "$OUT/held_at_kill.txt" ] && break; sleep 1
  done
  echo "t+$((SECONDS-T0)) 재시작 — 보유 로트 $(grep -c . "$OUT/held_at_kill.txt" 2>/dev/null) 개 든 채 kill -9" | tee -a "$OUT/events.log"
  kill -9 $CPID 2>/dev/null; wait $CPID 2>/dev/null; sleep 3
  start_cockpit "$OUT/cockpit2.log"
  echo "t+$((SECONDS-T0)) 재기동 완료" | tee -a "$OUT/events.log"
fi
left=$(( 180 - (SECONDS - T0) )); [ $left -gt 0 ] && sleep $left
echo "t+180 장애 kt10000 502 (applied)" | tee -a "$OUT/events.log"; curl -s -X POST $A/fault -d '{"api":"kt10000","http":502,"applied":true,"count":1}' >/dev/null
wait $F6

T=$(cat "$OUT/cockpit/api-token")
for p in state holdings "books?mode=live" "ledger?mode=live"; do
  curl -s -H "Authorization: Bearer $T" "http://127.0.0.1:$CP/v1/$p" > "$OUT/api_$(echo "$p" | cut -d'?' -f1).json"
done
curl -s $A/state > "$OUT/fake_state.json"
kill $CPID $FPID 2>/dev/null

# ── 판정 ──
PYTHONIOENCODING=utf-8 "$PY" - "$OUT" "${RESTART:-}" <<'PYEOF'
import json, os, re, sys
out = sys.argv[1]
restart = len(sys.argv) > 2 and sys.argv[2] == "1"
j = lambda n: json.load(open(f"{out}/{n}", encoding="utf-8"))
state, hold, ledger, fake = j("api_state.json"), j("api_holdings.json"), j("api_ledger.json"), j("fake_state.json")
flat6 = open(f"{out}/flat6.log", encoding="utf-8", errors="replace").read()
fails = []
# 0. flat6 가 가짜 키움과 같은 가격표를 썼다 (안 썼으면 신호가 ±15% 가드에 막혀 아래가 엉뚱하게 깨진다)
if "[e2e] 가격표 = FLAT6_DUMMY_PRICES" not in flat6: fails.append("flat6 가 FLAT6_DUMMY_PRICES 를 안 읽었다 (flat6 e2e 발행기 8e5e6b3e 이상 필요)")
# 1. flat6 왕복 — 마지막 요약 줄. 거절 포기·진입 만료로 끝난 사이클은 왕복이 아니다
m = re.findall(r"왕복=(\d+) \(조회 전 종결 (\d+) · 거절 포기 (\d+) · 진입 만료 (\d+)\)", flat6)
cycles, unseen, gave_up, expired = map(int, m[-1]) if m else (0, 0, 0, 0)
rounds = cycles - gave_up - expired
print(f"flat6 사이클 {cycles} = 왕복 {rounds} + 거절 포기 {gave_up} + 진입 만료 {expired} (그중 조회 전 종결 {unseen})")
if rounds < 2: fails.append(f"flat6 왕복 {rounds} < 2")
# 2. 콕핏이 보는 브로커 수량 = 가짜 키움 실보유
seen = {h["symbol"]["code"]: h["broker_qty"] for h in hold["holdings"]}
for code, q in fake["holdings"].items():
    if seen.get(code) != q: fails.append(f"{code}: 가짜 키움 {q} vs 콕핏이 본 브로커 {seen.get(code)}")
# 3. 청산 체결가 미상 금지 (결과 미상 매수 'rejected' 는 예외)
for o in ledger["orders"]:
    if o["phase"] == "exit_filled" and not o.get("price"):
        fails.append(f"체결가 미상 청산: {o['symbol']['code']} {o.get('exit_reason')} {o.get('detail','')}")
# 4. 502(applied) 는 '결과 미상' 으로 막히고 장부 밖(external)으로 보여야 한다
if not any(o["phase"] == "rejected" for o in ledger["orders"]): fails.append("502(applied) 매수가 결과 미상으로 기록되지 않았다")
if not any(h["status"] == "external" for h in hold["holdings"]): fails.append("502 로 들어간 주식이 장부 밖(external)으로 안 보인다")
# 5. 재시작 — 두 번째 콕핏이 잠금을 인계받아 떴고, 들고 있던 로트를 원장에서 되살렸다
if restart:
    c2 = open(f"{out}/cockpit2.log", encoding="utf-8", errors="replace").read() if os.path.exists(f"{out}/cockpit2.log") else ""
    m = re.search(r'"restored_positions":(\d+)', c2)  # 콕핏 로그는 JSON
    if "cockpitd 시작" not in c2: fails.append("재시작한 콕핏이 안 떴다: " + c2[-300:])
    elif not m or int(m.group(1)) < 1: fails.append("재시작한 콕핏이 로트를 복구하지 않았다 (restored_positions=%s)" % (m and m.group(1)))
    else: print(f"재시작: 로트 {m.group(1)} 개 복구")
    # 죽일 때 들고 있던 로트가 재시작 뒤 flat6 청산 목표로 체결가에 닫혔다 (= 재시작 뒤에도 목표를 받았다)
    hk = f"{out}/held_at_kill.txt"
    held = [l.strip() for l in open(hk, encoding="utf-8")] if os.path.exists(hk) else []
    held = [h for h in held if h]
    if not held: fails.append("콕핏을 죽일 때 보유 로트가 없었다 — 재시작 복구를 못 봤다")
    for iid in held:
        ex = [o for o in ledger["orders"] if o["intent_id"] == iid and o["phase"] == "exit_filled" and o.get("price", 0) > 0]
        if not ex: fails.append(f"재시작 전 로트 {iid[:8]} 가 재시작 뒤 체결가로 닫히지 않았다")
        else: print(f"재시작 전 로트 {iid[:8]} → 재시작 뒤 {ex[-1].get('exit_reason')} {ex[-1]['price']:g} 에 닫힘")
buys = [o for o in fake["orders"] if o["side"] == "buy"]
print(f"flat6 왕복 {rounds} · 가짜 키움 주문 {len(fake['orders'])}(매수 {len(buys)}) · 콕핏 원장 {len(ledger['orders'])}줄 · 최근 종결 {len(state['recent_closes'])}")
for c in state["recent_closes"]:
    print(f"  종결 {c['symbol']['code']} {c['reason']:8} {c.get('price',0):>9} {c.get('realized_pct',0)*100:+.2f}%")
print("PASS" if not fails else "FAIL\n  " + "\n  ".join(fails))
sys.exit(1 if fails else 0)
PYEOF
