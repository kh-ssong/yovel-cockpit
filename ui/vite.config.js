import { writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

const OUT = resolve(import.meta.dirname, '../internal/webui/dist')

// ★ 산출물이 Go 쪽(internal/webui/dist)으로 바로 떨어진다. 중간 복사 단계를 두지 않는 이유는,
// 복사가 하나 끼는 순간 "빌드는 했는데 복사를 안 한 옛 번들이 embed" 라는 상태가 생기기 때문이다.
export default defineConfig({
  plugins: [svelte(), keepGitkeep()],
  // 상대 경로 — 나중에 Tauri 가 file:// 로 같은 번들을 그대로 로드할 수 있어야 한다.
  base: './',
  build: {
    outDir: OUT,
    emptyOutDir: true,
    target: 'es2022',
    // ★ ★ minify 를 끈다. 이 저장소가 공개인 유일한 이유가 "키를 만지는 코드를 사용자가
    //   확인할 수 있게" 인데(README §1), 바이너리에 박히는 UI 가 minify 된 덩어리면 그 확인이
    //   UI 에서 끊긴다. 몇십 KB 를 아끼자고 공개의 근거를 반쪽 내지 않는다.
    minify: false,
    sourcemap: false,
    // 자산 인라인 금지 — 인라인되면 파일 단위로 대조할 수가 없다 (같은 이유).
    assetsInlineLimit: 0,
  },
  server: {
    // 개발 서버에서 API 는 데몬으로 넘긴다. Origin 은 localhost:5173 이라 데몬 가드를 통과한다.
    // ★ 단 토큰은 안 실린다 (주입은 데몬이 서빙할 때만) — 화면이 붙여넣기를 요구할 것이다.
    proxy: { '/v1': 'http://127.0.0.1:7737' },
  },
})

// keepGitkeep — emptyOutDir 이 지운 .gitkeep 을 되돌려 놓는다.
//
// dist 는 커밋하지 않지만 .gitkeep 한 장은 커밋되어 있다 — 그래야 npm 없이도
// `//go:embed all:dist` 가 컴파일된다. 빌드할 때마다 그게 지워지면 git status 가
// 매번 "삭제됨" 으로 더러워지고, 결국 누군가 그 삭제를 커밋한다.
function keepGitkeep() {
  return {
    name: 'cockpit-keep-gitkeep',
    apply: 'build',
    closeBundle() {
      writeFileSync(
        resolve(OUT, '.gitkeep'),
        '이 디렉토리는 `ui/` 의 빌드 산출물이 놓이는 자리다 (vite outDir).\n\n' +
          '산출물은 커밋하지 않는다 — 저장소에 두면 "소스는 고쳤는데 embed 된 건 옛 번들" 이라는\n' +
          '조용한 드리프트가 생긴다. 이 파일만 커밋해서 `//go:embed all:dist` 가 npm 없이도\n' +
          '컴파일되게 한다 (산출물이 없으면 데몬이 안내 페이지를 낸다).\n\n' +
          '빌드: cd ui && npm ci && npm run build\n',
      )
    },
  }
}
