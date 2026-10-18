#!/usr/bin/env bash
# macbook.sh — 운영 맥북에 콕핏을 올리고 돌린다. 빌드하는 이 PC 에서 SSH 로 부린다.
#
#   scripts/macbook.sh smoke        맥에서 도는지만 — 키·.env 없이 paper 로 잠깐 띄웠다 지운다 (계좌·reflex·~/cockpit 무접촉)
#   scripts/macbook.sh push         빌드 → ~/cockpit/releases/<sha> → ~/cockpit/bin 이 그걸 가리킨다. 재시작은 안 한다
#   scripts/macbook.sh install paper|live [-- cockpitd 플래그…]
#                                   launchd 설정을 ~/cockpit/<모드> 에 만들어만 둔다 (켜는 건 start) · 플래그에 공백 금지
#   scripts/macbook.sh start|stop|restart paper|live
#   scripts/macbook.sh status [paper|live]
#   scripts/macbook.sh tunnel [paper|live]   이 PC 127.0.0.1:<포트> → 맥 콕핏 (끊기면 다시 붙는다) + API 토큰 사본
#
# 접속 정보는 저장소 루트 .env.macbook (gitignore — ★ 공개 저장소에 주소·계정을 올리지 않는다):
#   MAC_SSH=user@host.local                  # 먼저 시도
#   MAC_SSH_FALLBACK="user@192.168.0.10"     # mDNS 가 안 풀릴 때 (공백으로 여럿)
#
# ★ paper 와 live 는 따로 도는 두 인스턴스다 (2026-10-17) — launchd 라벨·포트·data-dir·.env·로그가 다 다르다:
#     paper   com.yovel.cockpit.paper   7737   ~/cockpit/paper   logs/cockpit-paper.log
#     live    com.yovel.cockpit.live    7738   ~/cockpit/live    logs/cockpit-live.log
#   인스턴스 폴더(= data-dir)가 실행 폴더다 → 콕핏·감시자는 **그 폴더의 .env 만** 읽는다. paper 는 live 의 키움 키를 못 본다.
#
# 맥 배치 (~/cockpit):
#   releases/<sha>/{cockpitd,cockpitsup}   push 가 올린다 (최근 5개 + 도는 것만 남긴다)
#   bin -> releases/<sha>                  push 가 바꾼다. ★ 도는 콕핏은 start 때의 release 에 고정된다 —
#                                          push 만 하고 재시작 전에 콕핏이 죽어도 감시자는 옛 바이너리로 다시 띄운다.
#   <모드>/               data-dir — 원장·books.json·trusted_keys.json·api-token (docs/ops.md «paper 와 live 는 data-dir 을 나눈다»)
#   <모드>/.env           키는 *_FILE 경로만 · 텔레그램 (install 이 .env.example 을 남긴다)
#   <모드>/install.env    install 이 적는다 (MODE·PORT) — start·status·tunnel 이 읽는다
#   <모드>/launchd.plist  install 이 만든다 → start 가 release 를 박아 ~/Library/LaunchAgents/com.yovel.cockpit.<모드>.plist
#                         로 넣고 켠다 (= 재부팅 뒤에도 뜬다) · stop 이 끄고 뺀다 (= 재부팅해도 안 뜬다)
#   logs/cockpit-<모드>.log    (logs/cockpit.log = 옛 단일 인스턴스 로그 → 넘겨받으면 그 모드 로그로 가는 링크)
#
# ★ start 는 그 인스턴스의 키움 키가 reflex 의 키와 같으면 거부한다 — 키당 토큰 1 이라 서로를 끊는다.
#   reflex 는 국내봇(kiwoom)과 미장봇(us)이 **같은 키**를 쓴다 (2026-10-15 실측) → 둘 다 내려야 한다.
#   flat6 설정(~/flat6/.env* — 피드 주 키·추가 키·캐시 키)에 같은 키가 있어도 거부한다 (켜져 있든 아니든).
# ★ live 는 install·start·restart 때 COCKPIT_LIVE_OK=1 을 명시해야 한다 (실주문).
# ★ 옛 단일 인스턴스(라벨 com.yovel.cockpit · ~/cockpit/install.env)는 같은 모드의 restart·stop 이 넘겨받는다.
# ★ 기계를 옮길 땐 옛 기계의 자동 시작부터 끈다 (docs/ops.md).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck disable=SC1091
[ -f "$ROOT/.env.macbook" ] && . "$ROOT/.env.macbook"
: "${MAC_SSH:?저장소 루트 .env.macbook 에 MAC_SSH=user@host 를 적을 것 (스크립트 머리 주석)}"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=8 -o ServerAliveInterval=15 -o ServerAliveCountMax=3)
HOST=""

pick_host() {
  local h
  for h in $MAC_SSH ${MAC_SSH_FALLBACK:-}; do
    if ssh "${SSH_OPTS[@]}" "$h" true 2>/dev/null; then HOST=$h; return 0; fi
  done
  echo "★ 맥에 못 붙었다: $MAC_SSH ${MAC_SSH_FALLBACK:-}" >&2
  return 1
}
rsh() { ssh "${SSH_OPTS[@]}" "$HOST" "$@"; }
put() { scp -q "${SSH_OPTS[@]}" "$1" "$HOST:$2"; }

build_mac() {
  if [ "${SKIP_BUILD:-}" = 1 ] && [ -f "$ROOT/bin/cockpitd-darwin-amd64" ]; then return 0; fi
  bash "$ROOT/scripts/build.sh" mac
}

usage() { sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

# 모드는 늘 명시한다 — 기본값을 두면 paper 를 만지려다 live 를 건드린다.
need_mode() { case "${1:-}" in paper|live) ;; *) usage ;; esac; }
live_ok() {
  if [ "$1" = live ] && [ "${COCKPIT_LIVE_OK:-}" != 1 ]; then
    echo "★ live 는 실주문이다 — 확인했으면 COCKPIT_LIVE_OK=1 을 붙여 다시" >&2; exit 1
  fi
}
default_port() { if [ "$1" = live ]; then echo 7738; else echo 7737; fi; }

# 맥 쪽 스크립트 머리에 붙는 공통 함수.
# shellcheck disable=SC2016
REMOTE_LIB='
C="$HOME/cockpit"; LA="$HOME/Library/LaunchAgents"; uid=$(id -u); OLD=com.yovel.cockpit
label() { echo "com.yovel.cockpit.$1"; }
loaded() { launchctl print "gui/$uid/$1" >/dev/null 2>&1; }
# 그 인스턴스의 감시자·콕핏 — 명령줄의 data-dir 로 고른다 (다른 모드·smoke 는 안 걸린다)
procs() { pgrep -f "data-dir $C/$1 "; }
wait_gone() { for _ in $(seq 1 30); do procs "$1" >/dev/null || return 0; sleep 1; done; return 1; }
old_mode() { [ -f "$C/install.env" ] && (. "$C/install.env"; echo "${MODE:-}"); }
# 옛 단일 인스턴스를 넘겨받는다 (같은 모드일 때만): 끄고 · 자동 시작을 빼고 · 설정을 old/ 로 치우고 · 로그를 잇는다.
take_over_old() {
  [ "$(old_mode)" = "$1" ] || return 0
  if loaded "$OLD"; then launchctl bootout "gui/$uid/$OLD" && echo "옛 단일 인스턴스($OLD · $1) 껐다"; fi
  rm -f "$LA/$OLD.plist"
  wait_gone "$1" || { echo "★ 옛 인스턴스 프로세스가 안 내려간다:"; procs "$1" | xargs ps -o pid=,command= -p; exit 1; }
  mkdir -p "$C/old"
  mv -f "$C/install.env" "$C/old/install.env"
  [ -f "$C/$OLD.plist" ] && mv -f "$C/$OLD.plist" "$C/old/$OLD.plist"
  # 옛 로그는 이 모드의 것이었다 — 이어 쓰고, 옛 경로(다른 세션·도구가 읽는다)는 링크로 남긴다
  if [ -f "$C/logs/cockpit.log" ] && [ ! -L "$C/logs/cockpit.log" ] && [ ! -e "$C/logs/cockpit-$1.log" ]; then
    mv "$C/logs/cockpit.log" "$C/logs/cockpit-$1.log" && ln -s "cockpit-$1.log" "$C/logs/cockpit.log"
  fi
  echo "옛 설정 → $C/old/ · 로그 → logs/cockpit-$1.log"
}
'

# ── smoke ────────────────────────────────────────────────────────────────────
cmd_smoke() {
  build_mac
  pick_host
  local d="/tmp/cockpit-smoke-$$"
  rsh "mkdir -p $d"
  put "$ROOT/bin/cockpitd-darwin-amd64" "$d/cockpitd"
  put "$ROOT/bin/cockpitsup-darwin-amd64" "$d/cockpitsup"
  rsh "bash -s -- $d ${SMOKE_PORT:-7798}" <<'EOF'
d=$1; p=$2; cd "$d" || exit 1
chmod 755 cockpitd cockpitsup
fail() { echo "FAIL $*"; cd /; rm -rf "$d"; exit 1; }
# 빈 환경 — 키·.env·COCKPIT_* 를 하나도 안 물려받는다 (paper + 신호원 가격 = 계좌·reflex 무접촉)
E="env -i HOME=$HOME PATH=/usr/bin:/bin"
locks="$HOME/Library/Application Support/yovel-cockpit/locks"
before=$(ls "$locks" 2>/dev/null | wc -l | tr -d ' ')
echo "macOS $(sw_vers -productVersion) $(uname -m) · $($E ./cockpitd --version 2>&1 | head -1)"
curl -s -m1 "http://127.0.0.1:$p/v1/health" >/dev/null && fail "포트 $p 를 이미 누가 쓴다 (SMOKE_PORT 로 바꿀 것)"
$E ./cockpitsup -bin ./cockpitd -data-dir "$d/data" -- --mode paper --data-dir "$d/data" --port "$p" > sup.log 2>&1 &
sup=$!
h=""
# health 도 토큰을 요구한다 (data-dir/api-token)
for _ in $(seq 1 30); do
  h=$(curl -s -m1 -H "Authorization: Bearer $(cat data/api-token 2>/dev/null)" "http://127.0.0.1:$p/v1/health")
  echo "$h" | grep -q '"ok":true' && break; sleep 1
done
echo "$h" | grep -q '"ok":true' || { tail -5 sup.log; kill $sup; fail "health 응답 없음: $h"; }
echo "health $h"
# 감시자가 하트비트를 읽는 경로 그대로 — 집행 루프가 돌면 last_tick 에 시각이 찍힌다 (처음엔 0001-01-01)
for _ in $(seq 1 30); do grep -q '"last_tick":"20' data/heartbeat.json 2>/dev/null && break; sleep 1; done
grep -q '"last_tick":"20' data/heartbeat.json 2>/dev/null || { kill $sup; fail "집행 루프가 안 돈다 (last_tick 비어 있음)"; }
echo "heartbeat $(head -c 240 data/heartbeat.json)"
# 감시자에 SIGTERM (launchd bootout 과 같다) → 콕핏도 같이 내려가야 한다
kill -TERM $sup
for _ in $(seq 1 15); do kill -0 $sup 2>/dev/null || break; sleep 1; done
kill -0 $sup 2>/dev/null && fail "감시자가 SIGTERM 에 안 내려간다"
pgrep -f "cockpitd --mode paper --data-dir $d/data" >/dev/null && fail "감시자는 내려갔는데 콕핏이 남았다"
after=$(ls "$locks" 2>/dev/null | wc -l | tr -d ' ')
[ "$before" = "$after" ] || fail "계좌 잠금이 새로 생겼다 ($before → $after) — 키를 물려받았나?"
grep -E "cockpitd 시작|감시자 종료" sup.log | sed 's/^/  /'
cd /; rm -rf "$d"
echo "PASS — 맥에서 콕핏·감시자가 돈다 (임시 폴더 지움)"
EOF
}

# ── push ─────────────────────────────────────────────────────────────────────
cmd_push() {
  local sha
  sha=$(git -C "$ROOT" rev-parse --short HEAD)
  if ! git -C "$ROOT" diff --quiet HEAD -- && [ "${ALLOW_DIRTY:-}" != 1 ]; then
    echo "★ 커밋 안 한 변경이 있다 — 바이너리가 sha $sha 와 어긋난다 (그래도 올리려면 ALLOW_DIRTY=1)" >&2
    exit 1
  fi
  build_mac
  pick_host
  rsh "mkdir -p cockpit/releases/$sha cockpit/logs"
  put "$ROOT/bin/cockpitd-darwin-amd64" "cockpit/releases/$sha/cockpitd"
  put "$ROOT/bin/cockpitsup-darwin-amd64" "cockpit/releases/$sha/cockpitsup"
  rsh "bash -s -- $sha" <<'EOF'
sha=$1; cd "$HOME/cockpit" || exit 1
chmod 755 "releases/$sha/cockpitd" "releases/$sha/cockpitsup"
"./releases/$sha/cockpitd" --version >/dev/null 2>&1 || { echo "★ 새 바이너리가 이 맥에서 안 돈다 — bin 은 그대로 둔다"; exit 1; }
ln -sfn "releases/$sha" bin
# 최근 5개만 — 지금 가리키는 것과 launchd 가 돌리는 것(모드마다·옛 단일 인스턴스)은 남긴다
running=$(cat "$HOME"/Library/LaunchAgents/com.yovel.cockpit*.plist 2>/dev/null | grep -o 'releases/[0-9a-f]*' | cut -d/ -f2 | sort -u | tr '\n' ' ')
ls -1t releases | tail -n +6 | while read -r r; do
  [ "$r" = "$sha" ] || case " $running " in *" $r "*) ;; *) rm -rf "releases/$r" ;; esac
done
echo "bin -> releases/$sha · $(./bin/cockpitd --version 2>&1 | head -1)"
for r in $running; do [ "$r" != "$sha" ] && echo "★ 도는 콕핏은 아직 releases/$r — 바꾸려면 restart <모드>"; done
true
EOF
}

# ── install ──────────────────────────────────────────────────────────────────
cmd_install() {
  local mode=${1:-} port
  local -a args
  need_mode "$mode"
  live_ok "$mode"
  port=${COCKPIT_PORT:-$(default_port "$mode")}
  shift
  [ "${1:-}" = "--" ] && shift
  case "$mode" in
    paper) args=(--mode paper) ;;
    live) args=(--broker kiwoom --mode live) ;;
  esac
  pick_host
  { printf '%s\n' "$REMOTE_LIB"; cat <<'EOF'
mode=$1; port=$2; shift 2
D="$C/$mode"; L=$(label "$mode")
[ -x "$C/bin/cockpitd" ] || { echo "★ 먼저 push"; exit 1; }
for o in paper live; do
  [ "$o" != "$mode" ] && [ -f "$C/$o/install.env" ] && [ "$(. "$C/$o/install.env"; echo "$PORT")" = "$port" ] &&
    { echo "★ 포트 $port 는 $o 가 쓴다 — COCKPIT_PORT 로 다른 포트"; exit 1; }
done
mkdir -p "$D" "$C/logs"
{
  cat <<X
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$L</string>
  <key>ProgramArguments</key>
  <array>
    <string>$C/bin/cockpitsup</string>
    <string>-bin</string><string>$C/bin/cockpitd</string>
    <string>-data-dir</string><string>$D</string>
    <string>--</string>
X
  for a in "$@" --data-dir "$D" --port "$port"; do printf '    <string>%s</string>\n' "$a"; done
  cat <<X
  </array>
  <key>WorkingDirectory</key><string>$D</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>ExitTimeOut</key><integer>30</integer>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>$C/logs/cockpit-$mode.log</string>
  <key>StandardErrorPath</key><string>$C/logs/cockpit-$mode.log</string>
</dict>
</plist>
X
} > "$D/launchd.plist.new"
plutil -lint -s "$D/launchd.plist.new" || { echo "★ plist 가 깨졌다"; exit 1; }
mv -f "$D/launchd.plist.new" "$D/launchd.plist"
printf 'MODE=%s\nPORT=%s\n' "$mode" "$port" > "$D/install.env"
if [ ! -f "$D/.env" ] && [ ! -f "$D/.env.example" ]; then
  if [ "$mode" = live ]; then
    cat > "$D/.env.example" <<'ENV'
# 콕핏 live 설정 — cp .env.example .env 후 채운다. ★ 키 값은 여기 쓰지 말고 *_FILE 로 파일 경로만 (chmod 600).
# COCKPIT_KIWOOM_APPKEY_FILE=/Users/<나>/cockpit/secrets/kiwoom_appkey
# COCKPIT_KIWOOM_SECRET_FILE=/Users/<나>/cockpit/secrets/kiwoom_secret
# COCKPIT_TELEGRAM_BOT_TOKEN_FILE=/Users/<나>/cockpit/secrets/telegram_bot_token
# COCKPIT_TELEGRAM_CHAT_ID=...
# 계정 핸들은 install 플래그 --acct 로 준다 (flat6 가 목표에 싣는 값과 다르면 E_ACCT).
# 키움 토큰 파일은 비워 둔다 → data-dir 안 kiwoom_token.json (키당 토큰 1 — 다른 프로그램과 같은 파일을 쓰지 않는다)
ENV
  else
    cat > "$D/.env.example" <<'ENV'
# 콕핏 paper 설정 — 필요할 때만 cp .env.example .env.
# ★ paper 는 키 없이 돈다 (업비트 = 공개 시세 · 주식 = flat6 mark_price). live 키움 키를 여기 넣지 말 것 —
#   키당 토큰 1 이라 paper 가 live 의 토큰을 끊는다.
# COCKPIT_TELEGRAM_BOT_TOKEN_FILE=/Users/<나>/cockpit/secrets/telegram_bot_token
# COCKPIT_TELEGRAM_CHAT_ID=...
ENV
  fi
  chmod 600 "$D/.env.example"
fi
echo "설정 = $mode · 라벨 $L · 포트 $port · data-dir $D · 플래그: $* (켜는 건 start $mode)"
[ -f "$D/.env" ] || echo "  ($mode/.env 없음 — 키·텔레그램이 필요하면 .env.example 을 보고 만들 것)"
loaded "$L" && echo "★ 지금 켜져 있다 — 새 설정은 restart $mode 때부터"
[ "$(old_mode)" = "$mode" ] && loaded "$OLD" && echo "★ 옛 단일 인스턴스($OLD)가 이 모드로 돈다 — restart $mode 가 넘겨받는다"
true
EOF
  } | rsh "bash -s -- $mode $port ${args[*]} $*"
}

# ── start / stop / restart ───────────────────────────────────────────────────
# $1 = start | restart · $2 = 모드
remote_start() {
  { printf '%s\n' "$REMOTE_LIB"; cat <<'EOF'
op=$1; m=$2; D="$C/$m"; L=$(label "$m"); P="$LA/$L.plist"
[ -f "$D/launchd.plist" ] || { echo "★ 먼저 install $m"; exit 1; }
. "$D/install.env"
if loaded "$L" || { [ "$(old_mode)" = "$m" ] && loaded "$OLD"; }; then
  [ "$op" = restart ] || { echo "이미 켜져 있다 — restart $m"; exit 1; }
fi
loaded "$L" && launchctl bootout "gui/$uid/$L"
take_over_old "$m"
wait_gone "$m" || { echo "★ $m 프로세스가 안 내려간다:"; procs "$m" | xargs ps -o pid=,command= -p; exit 1; }

# ★ reflex 와 같은 키움 키면 거부 — 키당 토큰 1 (국내봇·미장봇이 같은 키를 쓴다). 값은 안 찍고 지문만 비교한다.
fp() { tr -d " \t\r\n\"'" | shasum -a 256 | cut -c1-16; }
envval() { grep -E "^$1=" "$2" 2>/dev/null | tail -1 | cut -d= -f2- | tr -d "\"'"; }
ck=""
f=$(envval COCKPIT_KIWOOM_APPKEY_FILE "$D/.env"); [ -n "$f" ] && [ -f "$f" ] && ck=$(fp < "$f")
v=$(envval COCKPIT_KIWOOM_APPKEY "$D/.env"); [ -n "$v" ] && ck=$(printf %s "$v" | fp)
rk=""
v=$(envval REFLEX_KIWOOM_APP_KEY "$HOME/works/reflex-agent/.env"); [ -n "$v" ] && rk=$(printf %s "$v" | fp)
ex=$(/usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:REFLEX_SUPERVISOR_EXCHANGES' \
  "$LA/com.reflex.supervisor.plist" 2>/dev/null)
if [ -n "$ck" ] && [ "$ck" = "$rk" ] && loaded com.reflex.supervisor && echo ",$ex," | grep -q -E ',(kiwoom|us),'; then
  echo "★ 거부 — reflex 가 같은 키움 키를 쓴다 (supervisor 켜짐 · 거래소 $ex)."
  echo "  국내봇(kiwoom)·미장봇(us)을 supervisor 에서 빼고 다시. 키당 토큰 1 이라 서로를 끊는다."
  exit 1
fi
# ★ flat6 설정(~/flat6/.env*)에 같은 키가 있어도 거부 — 피드가 그 키의 WS·토큰을 쥔다. 지금 꺼져 있어도
#   다시 뜨면 서로를 끊으니 설정에서 빠져야 한다. 추가 키(EXTRA_KEYS = 앱키:시크릿,…)는 앱키만 본다.
for f in "$HOME"/flat6/.env*; do
  [ -n "$ck" ] && [ -f "$f" ] || continue
  for k in FLAT6_KIWOOM_APP_KEY FLAT6_CACHE_KIWOOM_APP_KEY FLAT6_KIWOOM_EXTRA_KEYS; do
    envval "$k" "$f" | tr , '\n' | cut -d: -f1 | tr -d " \t\r" | while IFS= read -r v; do
      [ -n "$v" ] && [ "$(printf %s "$v" | fp)" = "$ck" ] && echo x
    done | grep -q x || continue
    echo "★ 거부 — flat6 이 같은 키움 키를 쓴다 ($(basename "$f") $k)."
    echo "  flat6 을 다른 키로 옮기고(그 키의 옛 토큰 파일도 정리) 다시. 키당 토큰 1 이라 서로를 끊는다."
    exit 1
  done
done

sha=$(basename "$(readlink "$C/bin")")
[ -x "$C/releases/$sha/cockpitd" ] || { echo "★ bin 이 가리키는 release 가 없다 — push"; exit 1; }
mkdir -p "$LA"
# 도는 콕핏을 지금 release 에 고정한다 — push 가 bin 을 바꿔도 재시작 전엔 안 바뀐다
sed "s#$C/bin/#$C/releases/$sha/#g" "$D/launchd.plist" > "$P"
launchctl bootstrap "gui/$uid" "$P" || { echo "★ launchctl bootstrap 실패"; exit 1; }
h=""
for _ in $(seq 1 40); do
  h=$(curl -s -m1 -H "Authorization: Bearer $(cat "$D/api-token" 2>/dev/null)" "http://127.0.0.1:$PORT/v1/health")
  echo "$h" | grep -q '"ok":true' && break; sleep 1
done
if ! echo "$h" | grep -q '"ok":true'; then echo "★ 40초 안에 안 떴다 — 로그:"; tail -5 "$C/logs/cockpit-$m.log"; exit 1; fi
echo "켜짐 ($m · $L · 포트 $PORT · releases/$sha) — $h"
EOF
  } | rsh "bash -s -- $1 $2"
}

cmd_stop() {
  local m=$1
  pick_host
  { printf '%s\n' "$REMOTE_LIB"; cat <<'EOF'
m=$1; L=$(label "$m")
if loaded "$L"; then launchctl bootout "gui/$uid/$L" && echo "$m 껐다"; else echo "$m 는 켜져 있지 않았다"; fi
rm -f "$LA/$L.plist"   # 재부팅해도 안 뜨게
take_over_old "$m"
if ! wait_gone "$m"; then echo "★ 아직 남은 $m 프로세스:"; procs "$m" | xargs ps -o pid=,command= -p; exit 1; fi
echo "$m 남은 프로세스 없음 · 자동 시작 해제"
EOF
  } | rsh "bash -s -- $m"
}

# ── status ───────────────────────────────────────────────────────────────────
cmd_status() {
  local modes=${1:-"paper live"}
  echo "이 PC HEAD $(git -C "$ROOT" rev-parse --short HEAD)"
  pick_host
  echo "맥 = $HOST"
  { printf '%s\n' "$REMOTE_LIB"; cat <<'EOF'
echo "bin -> $(readlink "$C/bin" 2>/dev/null || echo 없음)"
for m in "$@"; do
  D="$C/$m"; L=$(label "$m")
  if [ ! -f "$D/install.env" ]; then echo "[$m] 설정 없음 (install $m 전)"; continue; fi
  MODE=; PORT=; . "$D/install.env"
  if launchctl print "gui/$uid/$L" > /tmp/.cockpit_lc 2>&1; then
    echo "[$m] 포트 $PORT · launchd 켜짐 · $(grep -m1 -E '^\s*state = ' /tmp/.cockpit_lc | xargs) · $(grep -m1 -E '^\s*pid = ' /tmp/.cockpit_lc | xargs) · $(grep -o 'releases/[0-9a-f]*' "$LA/$L.plist" | head -1)"
  else
    echo "[$m] 포트 $PORT · launchd 꺼짐 (자동 시작 $([ -f "$LA/$L.plist" ] && echo 있음 || echo 없음))"
  fi
  rm -f /tmp/.cockpit_lc
  echo "  health $(curl -s -m2 -H "Authorization: Bearer $(cat "$D/api-token" 2>/dev/null)" "http://127.0.0.1:$PORT/v1/health" || echo '응답 없음')"
  [ -f "$D/heartbeat.json" ] && /usr/bin/python3 - "$D/heartbeat.json" <<'PY'
import json, re, sys, datetime as dt
b = json.load(open(sys.argv[1]))
def age(k):  # Go RFC3339(Nano) → 몇 초 전. 0 시각(아직 안 돎)은 "-"
    m = re.match(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(\.\d+)?(Z|[+-]\d\d:\d\d)?", b.get(k) or "")
    if not m or m.group(1).startswith("0001"):
        return "-"
    t = dt.datetime.strptime(m.group(1), "%Y-%m-%dT%H:%M:%S") + dt.timedelta(seconds=float(m.group(2) or 0))
    z = m.group(3) or "Z"
    if z != "Z":
        t -= (1 if z[0] == "+" else -1) * dt.timedelta(hours=int(z[1:3]), minutes=int(z[4:6]))
    return "%ds 전" % (dt.datetime.utcnow() - t).total_seconds()
print("  heartbeat ts %s · last_tick %s · 로트 %s · 진행 주문 %s" % (age("ts"), age("last_tick"), b.get("open_lots"), b.get("working_orders")))
PY
  nb=없음; [ -f "$D/books.json" ] && nb="장부 $(grep -c '"name"' "$D/books.json")개"
  echo "  .env $([ -f "$D/.env" ] && echo 있음 || echo 없음) · books.json $nb · trusted_keys $([ -f "$D/trusted_keys.json" ] && echo 있음 || echo 없음)"
  lg="$C/logs/cockpit-$m.log"
  [ -f "$lg" ] && { echo "  로그 ($(du -h "$lg" | cut -f1)) 마지막 2줄:"; tail -2 "$lg" | cut -c1-200 | sed 's/^/    /'; }
done
if loaded "$OLD"; then
  echo "★ 옛 단일 인스턴스($OLD) 켜짐 · 모드 $(old_mode) — restart $(old_mode) 로 넘겨받을 것"
fi
ex=$(/usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:REFLEX_SUPERVISOR_EXCHANGES' \
  "$LA/com.reflex.supervisor.plist" 2>/dev/null)
loaded com.reflex.supervisor && rs=켜짐 || rs=꺼짐
echo "reflex supervisor $rs · 거래소 ${ex:-?}"
echo "맥: 잠자기 $(pmset -g | awk '$1=="sleep"{print $2}')(0=안 잠) · $(memory_pressure 2>/dev/null | tail -1 | sed 's/System-wide memory free percentage/메모리 여유/') · 시계 $(sntp -t 3 time.apple.com 2>/dev/null | tail -1 | awk '{print $1"s"}')"
true
EOF
  } | rsh "bash -s -- $modes"
}

# ── tunnel (이 PC) ───────────────────────────────────────────────────────────
# flat6(이 PC) 는 FLAT6_COCKPIT_URL=http://127.0.0.1:<포트> · FLAT6_COCKPIT_TOKEN_FILE=<토큰 사본> 로 붙는다.
# 터널이 끊기면 flat6 발행이 실패하고 목표가 낡는다 → 콕핏 데드맨이 신규 진입만 막는다 (청산·stop 은 맥에서 계속).
cmd_tunnel() {
  local m=${1:-paper} lp tok rport
  need_mode "$m"
  lp=${TUNNEL_PORT:-$(default_port "$m")}
  if [ "$m" = paper ]; then tok=${TOKEN_OUT:-$ROOT/data/macbook/api-token}; else tok=${TOKEN_OUT:-$ROOT/data/macbook/$m.api-token}; fi
  mkdir -p "$(dirname "$tok")"
  if curl -s -m1 "http://127.0.0.1:$lp/v1/health" >/dev/null 2>&1; then
    echo "★ 이 PC 127.0.0.1:$lp 에 이미 뭔가 떠 있다 (로컬 콕핏?) — TUNNEL_PORT 로 다른 포트" >&2; exit 1
  fi
  while :; do
    if pick_host; then
      rport=$(rsh ". \"\$HOME/cockpit/$m/install.env\" 2>/dev/null && echo \"\$PORT\"") || rport=""
      if [ -n "$rport" ] && scp -q "${SSH_OPTS[@]}" "$HOST:cockpit/$m/api-token" "$tok.new" 2>/dev/null; then
        mv -f "$tok.new" "$tok"
      fi
      rport=${rport:-$(default_port "$m")}
      echo "$(date '+%F %T') 터널 127.0.0.1:$lp → $HOST:$rport ($m) · 토큰 $tok"
      ssh "${SSH_OPTS[@]}" -o ExitOnForwardFailure=yes -N -L "127.0.0.1:$lp:127.0.0.1:$rport" "$HOST" || true
      echo "$(date '+%F %T') 끊김 — 5초 뒤 다시"
    fi
    sleep 5
  done
}

case "${1:-}" in
  smoke) cmd_smoke ;;
  push) cmd_push ;;
  install) shift; cmd_install "$@" ;;
  start|restart) need_mode "${2:-}"; live_ok "$2"; pick_host; remote_start "$1" "$2" ;;
  stop) need_mode "${2:-}"; cmd_stop "$2" ;;
  status) [ -z "${2:-}" ] || need_mode "$2"; cmd_status "${2:-}" ;;
  tunnel) cmd_tunnel "${2:-paper}" ;;
  *) usage ;;
esac
