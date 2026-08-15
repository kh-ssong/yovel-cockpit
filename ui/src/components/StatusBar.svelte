<script>
  import { duration } from '../lib/format.js'

  let { health, snapshot, online, ageSec, lastError } = $props()

  // ★ 모드는 두 곳에서 온다 (health · state 스냅샷). 어긋나면 그건 배선 사고이므로
  // 조용히 한쪽을 고르지 않고 화면에 띄운다.
  const modeMismatch = $derived(
    health && snapshot && health.mode !== snapshot.mode ? `${health.mode} ≠ ${snapshot.mode}` : null,
  )
  const mode = $derived(health?.mode ?? snapshot?.mode ?? null)
</script>

<header class="bar">
  <div class="brand">
    <strong>yovel cockpit</strong>
    {#if mode}
      <span class="badge {mode === 'live' ? 'live' : 'paper'}">
        {mode === 'live' ? '● LIVE — 실주문' : '○ paper — 실주문 없음'}
      </span>
    {/if}
    {#if modeMismatch}
      <span class="badge warn" title="health 와 state 의 mode 가 다르다">모드 불일치 {modeMismatch}</span>
    {/if}
  </div>

  <div class="right mono">
    <span class="dot" class:on={online} class:off={!online}></span>
    {#if online}
      <span class="dim">{ageSec ?? 0}초 전</span>
    {:else if ageSec === null}
      <span class="err">연결 안 됨</span>
    {:else}
      <!-- ★ 끊긴 시점이 아니라 "마지막으로 진짜였던 시각" 을 쓴다. -->
      <span class="err">끊김 · 마지막 {duration(ageSec)} 전</span>
    {/if}

    {#if health}
      <span class="sep">|</span>
      <span class="dim" title="가동 시간">up {duration(health.uptime_sec)}</span>
      <span class="sep">|</span>
      <span class="dim" title="이 바이너리의 커밋">{health.sha || 'sha 없음'}</span>
      {#if health.dirty}
        <span class="badge warn" title="커밋되지 않은 변경으로 빌드됨">dirty</span>
      {/if}
      {#if !health.sha || health.sha === 'unknown'}
        <span class="badge warn" title="sha 가 비면 '업데이트했는데 옛 코드' 를 감지할 수 없다">
          sha 없음
        </span>
      {/if}
    {/if}
  </div>
</header>

{#if !online && lastError}
  <div class="alert bad">
    <strong>데몬과 끊겼다</strong> — {lastError}
    <div class="why">
      아래 값은 마지막으로 받은 것이다 (지금이 아니다). ★ 데몬이 살아 있다면 화면이 없어도
      집행·손절은 계속 돈다 — 끊긴 건 화면이지 봇이 아닐 수 있다.
    </div>
  </div>
{/if}

<style>
  .bar {
    display: flex;
    align-items: center;
    gap: 1rem;
    flex-wrap: wrap;
    padding: 1.1rem 0 0.9rem;
    border-bottom: 1px solid var(--line);
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 0.6rem;
  }
  .brand strong {
    letter-spacing: -0.02em;
  }
  .right {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-size: 0.8rem;
  }
  .sep {
    color: var(--line);
  }
  .err {
    color: var(--down);
  }
  .dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    display: inline-block;
  }
  .dot.on {
    background: var(--up);
  }
  .dot.off {
    background: var(--down);
  }
</style>
