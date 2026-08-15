// 화면이 보는 유일한 상태 덩어리.
//
// ★ 설계 규칙 하나: **성공한 마지막 값과 그 시각을 같이 들고 다닌다.**
// 폴링이 실패했을 때 화면을 비우면 "데이터가 없는 것" 처럼 보이고, 그냥 두면
// 옛 값이 현재처럼 보인다. 둘 다 틀렸다 — 값은 남기되 **몇 초 전 것인지** 항상 붙인다.

import { api, ApiError } from './api.js'

const FAST_MS = 2000 // 화면을 보고 있을 때
const SLOW_MS = 15000 // 탭이 숨겨졌을 때 (데몬은 계속 돈다 — 화면만 쉰다)
const LEDGER_EVERY = 5 // 폴링 N 회마다 원장 한 번 (원장은 그렇게 자주 안 바뀐다)

export function createCockpit() {
  let health = $state(null)
  let snapshot = $state(null)
  let plan = $state(null)
  let ledger = $state(null)

  let lastOkAt = $state(0)
  let lastError = $state('')
  let everLoaded = $state(false)
  let ledgerMode = $state(null)
  let ledgerError = $state('')

  // now — "n초 전" 을 그리기 위한 시계. 상태가 안 변해도 나이는 흘러야 한다.
  let now = $state(Date.now())

  let ticks = 0
  let timer = null
  let stopped = false

  async function pull() {
    try {
      const [h, s, p] = await Promise.all([api.health(), api.state(), api.plan()])
      health = h
      snapshot = s
      plan = p
      lastOkAt = Date.now()
      lastError = ''
      everLoaded = true

      // ★ 원장 mode 는 데몬이 도는 모드를 따라간다. 사용자가 고르면 그 선택이 이긴다.
      if (ledgerMode === null) ledgerMode = h.mode
      if (ticks % LEDGER_EVERY === 0) await pullLedger()
    } catch (e) {
      lastError = e instanceof ApiError ? e.message : String(e)
    } finally {
      ticks += 1
    }
  }

  async function pullLedger() {
    if (!ledgerMode) return
    try {
      ledger = await api.ledger(ledgerMode)
      ledgerError = ''
    } catch (e) {
      ledgerError = e instanceof ApiError ? e.message : String(e)
    }
  }

  function schedule() {
    if (stopped) return
    timer = setTimeout(async () => {
      now = Date.now()
      await pull()
      schedule()
    }, document.hidden ? SLOW_MS : FAST_MS)
  }

  return {
    get health() {
      return health
    },
    get snapshot() {
      return snapshot
    },
    get plan() {
      return plan
    },
    get ledger() {
      return ledger
    },
    get ledgerMode() {
      return ledgerMode
    },
    get ledgerError() {
      return ledgerError
    },
    get lastError() {
      return lastError
    },
    get everLoaded() {
      return everLoaded
    },
    /** 마지막 성공으로부터 흐른 초. 한 번도 못 받았으면 null. */
    get ageSec() {
      return lastOkAt === 0 ? null : Math.max(0, Math.round((now - lastOkAt) / 1000))
    },
    /** 지금 데몬과 붙어 있는가 (마지막 시도가 성공했는가). */
    get online() {
      return lastError === '' && lastOkAt !== 0
    },

    async setLedgerMode(mode) {
      ledgerMode = mode
      ledger = null
      await pullLedger()
    },

    start() {
      stopped = false
      // ★ 1초짜리 시계는 폴링과 분리한다. 폴링이 죽어도 나이는 계속 늘어야 한다 —
      // 멈춘 "3초 전" 만큼 사람을 속이는 표시가 없다.
      const clock = setInterval(() => (now = Date.now()), 1000)
      pull().then(schedule)
      return () => {
        stopped = true
        clearInterval(clock)
        if (timer) clearTimeout(timer)
      }
    },
  }
}
