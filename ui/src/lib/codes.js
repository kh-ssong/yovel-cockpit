// 거절 코드 → 사람 말.
//
// ★ 코드를 그대로 보여주지 않는 이유: 이 화면을 보는 시점은 대개 "왜 안 샀지" 를 묻는
// 순간이고, 그때 필요한 건 코드가 아니라 **무엇이 막았는지**다.
// ★ 그렇다고 코드를 감추지도 않는다 — 로그·프로토콜 문서와 대조해야 하니 둘 다 띄운다.
// 원본 목록 = internal/protocol/types.go (RejectCode).

export const REJECT = {
  E_SIG: '서명이 맞지 않음 — 신뢰키에 없는 발행자거나 본문이 변조됨',
  E_EXPIRED: '봉투가 만료됨 (exp 경과)',
  E_SKEW: '시계 오차가 허용치를 넘음',
  E_REPLAY: '이미 처리한 메시지 (재생 방어)',
  E_SCHEMA: '형식이 계약과 다름',
  E_UNSUPPORTED_TYPE: '모르는 메시지 타입 — 실행하지 않음',
  E_MODE: '모드가 맞지 않음 (paper ↔ live)',
  E_PAUSED: '일시정지 중 — 신규 진입 차단',
  E_MARKET_CLOSED: '장 시간이 아님',
  E_SYMBOL: '기준가를 모름 — 시세원이 없거나 ref_price 가 안 실림',
  E_CAPITAL: '자본 대비 수량이 안 나옴 (최소주문·호가단위)',
  E_LOCAL_GUARD: '로컬 가드가 막음 (서킷 브레이커 · book_state=halted)',
  E_BROKER: '브로커가 거절',
  E_ORPHAN: '목표에 없는 보유 — ★ 자동 청산하지 않는다',
  E_RATE: '이번 틱 주문 상한에 걸림 — 다음 틱에 다시 시도',
  E_TERMINAL: '이미 종결된 목표 — 같은 자리 재진입을 막았다',
}

export function explain(code) {
  return REJECT[code] ?? code
}

/** ack 상태 → 라벨. applied 만 "반영" 이고 나머지는 각자 다른 뜻이다. */
export const ACK_STATUS = {
  applied: '반영',
  rejected: '거절',
  expired: '만료',
  noop: '할 일 없음',
  ignored: '무시',
  partial: '일부 반영',
}
