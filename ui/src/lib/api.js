// 데몬 로컬 API 클라이언트.
//
// ★ 이 파일은 상태를 만들지 않는다 — 데몬이 아는 것을 그대로 옮길 뿐이다.
// 화면이 자기 계산을 시작하는 순간 "화면에 보이는 것" 과 "데몬이 아는 것" 이 갈리고,
// 이 UI 의 최악 실패가 정확히 그것이다.

/** 데몬이 index.html 에 심어 준 부팅 정보. 개발 서버(vite)에서는 없다. */
export const boot = globalThis.__COCKPIT__ ?? null

const DEV_TOKEN_KEY = 'cockpit_dev_token'

let token = boot?.token ?? localStorage.getItem(DEV_TOKEN_KEY) ?? ''

export function hasToken() {
  return token.length > 0
}

/** 데몬이 서빙하는 페이지인가 (아니면 vite 개발 서버). */
export function isServedByDaemon() {
  return boot !== null
}

/**
 * 개발 서버용 토큰 입력. ★ 데몬이 서빙할 때는 쓰지 않는다 —
 * 주입된 토큰을 사람이 덮어쓸 수 있으면, 붙여넣기 실수가 "전부 401" 로만 보인다.
 */
export function setDevToken(value) {
  if (isServedByDaemon()) return
  token = value.trim()
  localStorage.setItem(DEV_TOKEN_KEY, token)
}

export class ApiError extends Error {
  constructor(message, status) {
    super(message)
    this.status = status
  }
}

async function get(path) {
  let res
  try {
    res = await fetch(path, {
      headers: { Authorization: `Bearer ${token}` },
      // 옛 응답을 현재처럼 보여주는 게 이 화면의 최악 실패다. 캐시를 쓰지 않는다.
      cache: 'no-store',
    })
  } catch (e) {
    // 여기 오는 건 대개 데몬이 죽었거나 포트가 다른 경우다.
    throw new ApiError('데몬에 닿지 않는다', 0)
  }
  if (res.status === 401) {
    throw new ApiError('자격 없음 — 토큰이 데몬의 것과 다르다', 401)
  }
  if (res.status === 503) {
    throw new ApiError('데몬은 살아 있으나 엔진이 아직 없다', 503)
  }
  const body = await res.json().catch(() => null)
  if (!res.ok) {
    throw new ApiError(body?.error ?? `HTTP ${res.status}`, res.status)
  }
  return body
}

export const api = {
  health: () => get('/v1/health'),
  state: () => get('/v1/state'),
  plan: () => get('/v1/plan'),
  /**
   * ★ mode 는 호출자가 반드시 고른다. 데몬이 400 으로 거절하는 것과 같은 규칙을
   * 화면에서도 지킨다 — paper 와 live 를 합산해 보여주는 순간 그건 허위 손익이다.
   */
  ledger: (mode, limit = 100) =>
    get(`/v1/ledger?mode=${encodeURIComponent(mode)}&limit=${limit}`),
}
