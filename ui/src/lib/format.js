// 표시 전용 헬퍼. ★ 여기서 계산을 하지 않는다 — 수치는 데몬이 준 것을 그대로 쓴다.

const NUM = new Intl.NumberFormat('ko-KR')

export function krw(v) {
  if (v === null || v === undefined) return '—'
  return NUM.format(Math.round(v)) + '원'
}

export function num(v, digits = 0) {
  if (v === null || v === undefined) return '—'
  return v.toLocaleString('ko-KR', {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  })
}

export function pct(v, digits = 2) {
  if (v === null || v === undefined) return '—'
  const s = v.toLocaleString('ko-KR', {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  })
  return (v > 0 ? '+' : '') + s + '%'
}

/** 부호를 색으로 옮긴다 (0 은 중립). */
export function sign(v) {
  if (!v) return 'flat'
  return v > 0 ? 'up' : 'down'
}

// ★ 시각은 로케일 포맷터를 쓰지 않는다. ko-KR 은 "19시 55분 36초" 로 늘어나 표에서 자리를
// 다 먹고, 무엇보다 자릿수가 값마다 달라져 세로로 읽히지 않는다. 시각 열은 눈으로 훑으며
// **차이**를 보는 곳이라 고정폭이 낫다.
const pad = (n) => String(n).padStart(2, '0')

// ★ Go 의 제로시각은 "0001-01-01T00:00:00Z" 로 직렬화된다. 그대로 그리면 "01/01 09:00:00"
// 같은 그럴듯한 시각이 되어, **값이 없는 것**과 **값이 있는 것**이 화면에서 같아진다.
function parse(iso) {
  if (!iso) return null
  const d = new Date(iso)
  if (Number.isNaN(d.getTime()) || d.getUTCFullYear() < 1971) return null
  return d
}

export function clock(iso) {
  const d = parse(iso)
  if (!d) return '—'
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

export function clockDate(iso) {
  const d = parse(iso)
  if (!d) return '—'
  return `${pad(d.getMonth() + 1)}/${pad(d.getDate())} ${clock(iso)}`
}

export function duration(sec) {
  if (sec === null || sec === undefined) return '—'
  if (sec < 60) return `${sec}초`
  const m = Math.floor(sec / 60)
  if (m < 60) return `${m}분`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}시간 ${m % 60}분`
  return `${Math.floor(h / 24)}일 ${h % 24}시간`
}

/** ISO 시각까지 남은 초. 지났으면 음수. */
export function untilSec(iso, now = Date.now()) {
  const d = parse(iso)
  if (!d) return null
  return Math.round((d.getTime() - now) / 1000)
}

export function symbolOf(s) {
  if (!s) return '—'
  return s.code ?? '—'
}
