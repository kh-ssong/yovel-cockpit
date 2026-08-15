#!/usr/bin/env bash
# cockpitd 빌드.
#
# ★ ldflags 로 sha 를 직접 박는 이유: go 의 자동 VCS 스탬프는 git worktree 에서 동작하지 않는다
#   (`.git` 이 파일이라 감지 실패, -buildvcs=true 를 줘도 조용히 비운다). 이 프로젝트는
#   "모든 작업은 워크트리" 규칙이라 개발 빌드가 사실상 항상 그 조건이다.
#   sha 가 비면 "업데이트했는데 옛 코드" 드리프트 감지가 통째로 죽는다.
#
# ★ UI 를 **매번 새로 빌드한 뒤** 바이너리를 만든다. 산출물(internal/webui/dist)은 커밋하지
#   않으므로 "옛 번들이 박히는" 경로가 아예 없어야 하고, 그러려면 embed 직전에 짓는 수밖에 없다.
#   npm 이 없으면 조용히 넘어가지 않고 **멈춘다** — UI 없는 바이너리를 사용자가 UI 있는 것으로
#   착각하는 게 몇 초 아끼는 것보다 비싸다. 정말 필요하면 COCKPIT_SKIP_UI=1 로 명시해서 끈다.
#
# 사용:
#   scripts/build.sh              현재 플랫폼
#   scripts/build.sh cross        macOS(arm64/amd64) + Windows + Linux 전부
#   COCKPIT_SKIP_UI=1 scripts/build.sh   UI 없이 (헤드리스·긴급용. 데몬이 안내 페이지를 낸다)
set -euo pipefail

cd "$(dirname "$0")/.."

build_ui() {
  if [[ "${COCKPIT_SKIP_UI:-0}" == "1" ]]; then
    echo "UI 빌드 건너뜀 (COCKPIT_SKIP_UI=1) — 이 바이너리에는 대시보드가 없다"
    return
  fi
  if ! command -v npm >/dev/null 2>&1; then
    echo "npm 이 없다. UI 를 빌드할 수 없다." >&2
    echo "  설치하거나, UI 없이 만들려면 COCKPIT_SKIP_UI=1 을 명시할 것." >&2
    exit 1
  fi
  echo "UI 빌드"
  # npm ci 는 package-lock.json 을 그대로 재현한다 (install 과 달리 잠금을 갱신하지 않는다).
  # 재현 가능 빌드가 목표이므로 여기서 잠금이 흔들리면 안 된다.
  (cd ui && npm ci --silent && npm run build --silent)
}

PKG=github.com/kh-ssong/yovel-cockpit/internal/version
SHA="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
# 태그가 없으면 describe 가 sha 를 되돌려줘 "cockpitd 32f19dd (32f19dd)" 같은 꼴이 된다.
# 아직 릴리스 태그가 없는 단계이므로 명시적으로 dev 버전을 쓴다.
VERSION="$(git describe --tags --dirty 2>/dev/null || echo "0.0.0-dev+${SHA}")"
BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# 재현 가능 빌드 — 릴리스 바이너리가 이 소스에서 나왔음을 사용자가 확인할 수 있어야
# §1 의 "공개"가 말이 아니라 검증이 된다.
LDFLAGS="-s -w -X ${PKG}.version=${VERSION} -X ${PKG}.sha=${SHA} -X ${PKG}.builtAt=${BUILT_AT}"
COMMON=(-trimpath -ldflags "${LDFLAGS}")
export CGO_ENABLED=0

build() { # os arch suffix
  local os=$1 arch=$2 out="bin/cockpitd-$1-$2$3"
  echo "  $out"
  GOOS="$os" GOARCH="$arch" go build "${COMMON[@]}" -o "$out" ./cmd/cockpitd
}

build_ui

mkdir -p bin
if [[ "${1:-}" == "cross" ]]; then
  echo "크로스 빌드 ${VERSION} (${SHA})"
  # ★ 이 한 덩어리가 Go 를 고른 유일한 결정타다 — 윈도우 PC 한 대에서 맥 ARM 까지 낸다.
  build darwin arm64 ""
  build darwin amd64 ""
  build windows amd64 ".exe"
  build linux amd64 ""
else
  echo "빌드 ${VERSION} (${SHA})"
  suffix=""; [[ "$(go env GOOS)" == "windows" ]] && suffix=".exe"
  build "$(go env GOOS)" "$(go env GOARCH)" "$suffix"
fi
