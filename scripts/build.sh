#!/usr/bin/env bash
# cockpitd 빌드.
#
# ★ ldflags 로 sha 를 직접 박는 이유: go 의 자동 VCS 스탬프는 git worktree 에서 동작하지 않는다
#   (`.git` 이 파일이라 감지 실패, -buildvcs=true 를 줘도 조용히 비운다). 이 프로젝트는
#   "모든 작업은 워크트리" 규칙이라 개발 빌드가 사실상 항상 그 조건이다.
#   sha 가 비면 "업데이트했는데 옛 코드" 드리프트 감지가 통째로 죽는다.
#
# 사용:
#   scripts/build.sh              현재 플랫폼
#   scripts/build.sh cross        macOS(arm64/amd64) + Windows + Linux 전부
set -euo pipefail

cd "$(dirname "$0")/.."

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
