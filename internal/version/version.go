// Package version 은 이 바이너리가 무엇인지 말한다.
//
// ★ 이게 장식이 아닌 이유: "업데이트했는데 옛 코드가 돌고 있음" 드리프트는 이 프로젝트에서
// 이미 실제로 겪은 사고다. 사용자 PC 로 배포하면 그대로 재발하므로, SHA 를 heartbeat 와
// /v1/health 양쪽에 실어 원격에서 확인 가능하게 만든다.
package version

import (
	"runtime/debug"
	"sync"
)

// 릴리스 빌드는 ldflags 로 덮어쓴다:
//
//	go build -trimpath -ldflags "-X .../internal/version.version=0.1.0 -X .../internal/version.sha=$(git rev-parse --short HEAD)"
var (
	version = "0.0.0-dev"
	sha     = ""
	builtAt = ""
)

type Info struct {
	Version string `json:"version"`
	SHA     string `json:"sha"`
	BuiltAt string `json:"built_at,omitempty"`
	Dirty   bool   `json:"dirty,omitempty"`
}

var (
	once   sync.Once
	cached Info
)

// Get 은 빌드 정보를 준다. ldflags 가 없으면 go 가 심어준 VCS 정보로 되돌아간다.
//
// ★ 그 fallback 을 믿으면 안 되는 실측 조건이 하나 있다: **git worktree 에서 빌드하면
// go 가 VCS 정보를 아예 안 박는다** (`.git` 이 디렉토리가 아니라 파일이라서. `-buildvcs=true`
// 를 줘도 에러 없이 조용히 비운다). 그런데 이 프로젝트의 개발 규칙이 "모든 작업은 워크트리" 라
// 개발 빌드는 사실상 항상 그 조건에 걸린다 ⟹ sha 가 "unknown" 이 된다.
//
// 그래서 scripts/build.sh 가 ldflags 로 직접 박는다. 드리프트 감지가 sha 에 걸려 있으므로
// "빌드는 됐는데 sha 가 비어 있다" 를 방치하면 감지 장치가 조용히 죽는다.
func Get() Info {
	once.Do(func() {
		cached = Info{Version: version, SHA: sha, BuiltAt: builtAt}
		bi, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if cached.SHA == "" && len(s.Value) >= 7 {
					cached.SHA = s.Value[:7]
				}
			case "vcs.time":
				if cached.BuiltAt == "" {
					cached.BuiltAt = s.Value
				}
			case "vcs.modified":
				cached.Dirty = s.Value == "true"
			}
		}
		if cached.SHA == "" {
			cached.SHA = "unknown"
		}
	})
	return cached
}
