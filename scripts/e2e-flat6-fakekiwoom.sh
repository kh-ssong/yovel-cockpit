#!/usr/bin/env bash
# e2e — flat6(dummy 발행기) → 콕핏(--broker kiwoom) → 가짜 키움(스펙 기반, 체결까지).
#
#   scripts/e2e-flat6-fakekiwoom.sh            # 약 6분
#
# 검증하는 것: 서명·seq·ack·상태 조회(flat6 ↔ 콕핏) + 주문·분할체결·TP 위임·취소·청산(콕핏 ↔ 키움 API 모양)
#             + 장애 주입(주문은 들어갔는데 응답만 502) + 끝에 콕핏 장부 ↔ 가짜 키움 실상태 대조.
# 안 하는 것: 실제 키움 서버 동작 (→ 모의투자 mockapi / 소액 라이브).
#
# ★ 격리: 콕핏은 임시 폴더에서 떠서 저장소 .env(실키·실토큰 경로)를 읽지 않고, 가짜 키·가짜 토큰 파일을 쓴다.
#   flat6 은 임시 data-dir·테스트 포트·가짜 acct(acc_flat6dummy)·텔레그램/Slack 끔. 서명키만 읽는다.
#
# 환경변수: FLAT6_DIR(기본 ../yovel-flat6) FLAT6_KEY(기본 $FLAT6_DIR/data/signing_key.json)
#           KIWOOM_SPEC(기본 ~/Downloads/kiwoom-rest-api-spec.json) PY(기본 python) OUT(기본 임시 폴더)
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FLAT6_DIR="${FLAT6_DIR:-$ROOT/../yovel-flat6}"
FLAT6_KEY="${FLAT6_KEY:-$FLAT6_DIR/data/signing_key.json}"
KIWOOM_SPEC="${KIWOOM_SPEC:-$HOME/Downloads/kiwoom-rest-api-spec.json}"
PY="${PY:-python}"
OUT="${OUT:-$(mktemp -d)}"
# ★ 가격표는 여기 한 곳 — 가짜 키움(-price)과 flat6 dummy(FLAT6_DUMMY_PRICES)에 **같은 값**을 넘긴다.
#   둘이 어긋나면 flat6 신호가 콕핏 가드(±15%)에 막힌다. flat6 가 FLAT6_DUMMY_PRICES 를 모르면 무시되고
#   dummy 기본값(지금은 아래와 같다)을 쓴다. 형식 = "코드=가격,코드=가격".
PRICES="${PRICES:-005930=71000,000660=195000,035720=41000}"
CP=7795; FK=7801
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

COCKPIT_KIWOOM_APPKEY=fake COCKPIT_KIWOOM_SECRET=fake COCKPIT_KIWOOM_TOKEN_FILE="$OUT/cockpit/kiwoom_token.json" \
COCKPIT_TELEGRAM_BOT_TOKEN= \
"$OUT/cockpitd$EXE" --data-dir "$OUT/cockpit" --port $CP --ui=false --acct acc_flat6dummy --engine-budget 6000000 \
  --broker kiwoom --kiwoom-api-url http://127.0.0.1:$FK --mode live --ignore-market-hours > "$OUT/cockpit.log" 2>&1 &
CPID=$!
for i in $(seq 1 30); do curl -s -m1 http://127.0.0.1:$CP/v1/health >/dev/null && break; sleep 0.5; done
curl -s -X POST $A/scenario -d '{"fill":"split","chunks":3,"interval_ms":400}' >/dev/null

(cd "$FLAT6_DIR" && PYTHONPATH=src PYTHONIOENCODING=utf-8 \
  FLAT6_MODE=signal FLAT6_STRATEGY=dummy FLAT6_MOCKPIT=false FLAT6_GATEWAY_PORT=0 \
  FLAT6_DATA_DIR="$OUT/flat6" FLAT6_SIGNING_KEY_PATH="$FLAT6_KEY" \
  FLAT6_COCKPIT_URL=http://127.0.0.1:$CP FLAT6_COCKPIT_TOKEN_FILE="$OUT/cockpit/api-token" FLAT6_COCKPIT_ACCT=acc_flat6dummy \
  FLAT6_TELEGRAM_BOT_TOKEN= FLAT6_TELEGRAM_CHAT_ID= FLAT6_SLACK_WEBHOOK= FLAT6_DUMMY_PRICES="$PRICES" \
  timeout 330 "$PY" -u -m flat6.main --mode signal --strategy dummy > "$OUT/flat6.log" 2>&1) &
F6=$!

# TP 유도가 = 005930 가격표 +2.8% 를 500원 단위로 올림 (71000 → 73000; 500 은 50만원 미만 모든 호가 단위의 배수)
P5930=$(echo "$PRICES" | tr ',' '\n' | sed -n 's/^ *005930 *= *\([0-9]*\) *$/\1/p')
TP_PX=$(( (P5930 * 1028 / 1000 + 499) / 500 * 500 ))
sleep 50;  echo "t+50  TP 유도 005930 → $TP_PX" | tee -a "$OUT/events.log"; curl -s -X POST $A/price -d "{\"code\":\"005930\",\"price\":$TP_PX}" >/dev/null
sleep 130; echo "t+180 장애 kt10000 502 (applied)" | tee -a "$OUT/events.log"; curl -s -X POST $A/fault -d '{"api":"kt10000","http":502,"applied":true,"count":1}' >/dev/null
wait $F6

T=$(cat "$OUT/cockpit/api-token")
for p in state holdings "books?mode=live" "ledger?mode=live"; do
  curl -s -H "Authorization: Bearer $T" "http://127.0.0.1:$CP/v1/$p" > "$OUT/api_$(echo "$p" | cut -d'?' -f1).json"
done
curl -s $A/state > "$OUT/fake_state.json"
kill $CPID $FPID 2>/dev/null

# ── 판정 ──
PYTHONIOENCODING=utf-8 "$PY" - "$OUT" <<'PYEOF'
import json, re, sys
out = sys.argv[1]
j = lambda n: json.load(open(f"{out}/{n}", encoding="utf-8"))
state, hold, ledger, fake = j("api_state.json"), j("api_holdings.json"), j("api_ledger.json"), j("fake_state.json")
flat6 = open(f"{out}/flat6.log", encoding="utf-8", errors="replace").read()
rounds = max([int(x) for x in re.findall(r"왕복=(\d+)", flat6)] or [0])
fails = []
# 0. flat6 가 가짜 키움과 같은 가격표를 썼다 (안 썼으면 신호가 ±15% 가드에 막혀 아래가 엉뚱하게 깨진다)
if "[dummy] 가격표 = FLAT6_DUMMY_PRICES" not in flat6: fails.append("flat6 가 FLAT6_DUMMY_PRICES 를 안 읽었다 (flat6 3276172e 이상 필요)")
# 1. flat6 왕복
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
buys = [o for o in fake["orders"] if o["side"] == "buy"]
print(f"flat6 왕복 {rounds} · 가짜 키움 주문 {len(fake['orders'])}(매수 {len(buys)}) · 콕핏 원장 {len(ledger['orders'])}줄 · 최근 종결 {len(state['recent_closes'])}")
for c in state["recent_closes"]:
    print(f"  종결 {c['symbol']['code']} {c['reason']:8} {c.get('price',0):>9} {c.get('realized_pct',0)*100:+.2f}%")
print("PASS" if not fails else "FAIL\n  " + "\n  ".join(fails))
sys.exit(1 if fails else 0)
PYEOF
