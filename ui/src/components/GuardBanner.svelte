<script>
  import { untilSec, duration } from '../lib/format.js'

  let { guards, now } = $props()

  // ★ 가드는 "무엇이 켜졌나" 가 아니라 **"지금 무엇이 안 되는가"** 로 쓴다.
  // paused=true 만 띄우면 사용자는 그게 진입만 막는지 청산까지 막는지 매번 문서를 찾아야 한다.
  const items = $derived.by(() => {
    if (!guards) return []
    const out = []
    if (guards.paused) {
      out.push({
        title: '일시정지 — 신규 진입 없음',
        why: '청산 · 손절 · 시간청산은 그대로 돈다. 재개는 서버의 resume 로만 풀린다.',
      })
    }
    if (guards.circuit_breaker) {
      out.push({
        title: '서킷 브레이커 — 로컬 가드가 진입을 막는 중',
        why: '★ 서버가 끌 수 없는 층이다. 끄려면 데몬 쪽 조건이 풀려야 한다.',
      })
    }
    const left = untilSec(guards.block_entry_until, now)
    if (left !== null && left > 0) {
      out.push({
        title: `진입 차단 ${duration(left)} 남음`,
        why: 'de-risk 의 block_entry 가 걸려 있다. 만료되면 자동으로 풀린다.',
      })
    }
    if (guards.target_stale) {
      out.push({
        title: '목표가 늙었다 — 진입 금지',
        why: '마지막 목표 스냅샷이 target-max-age 를 넘겼다. ★ 청산과 stop 은 계속 유효하다 (마지막으로 받은 stop 가격으로).',
      })
    }
    return out
  })
</script>

{#each items as it}
  <div class="alert">
    <strong>{it.title}</strong>
    <div class="why">{it.why}</div>
  </div>
{/each}
