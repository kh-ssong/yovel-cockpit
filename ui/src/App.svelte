<script>
  import { onMount } from 'svelte'
  import { createCockpit } from './lib/cockpit.svelte.js'
  import { isServedByDaemon, hasToken, setDevToken } from './lib/api.js'
  import { clockDate } from './lib/format.js'
  import StatusBar from './components/StatusBar.svelte'
  import GuardBanner from './components/GuardBanner.svelte'
  import Positions from './components/Positions.svelte'
  import PlanPanel from './components/PlanPanel.svelte'
  import Orphans from './components/Orphans.svelte'
  import Ledger from './components/Ledger.svelte'

  const cockpit = createCockpit()
  onMount(() => cockpit.start())

  // 개발 서버(vite)에서는 데몬이 토큰을 주입할 수 없다 — 사람이 붙여넣는다.
  let devToken = $state('')
  const needsDevToken = $derived(!isServedByDaemon() && !hasToken())

  // ★ 오래된 값을 "현재" 로 보이게 두지 않는다. 끊긴 뒤 12초가 넘으면 화면을 흐린다.
  const stale = $derived(!cockpit.online && (cockpit.ageSec ?? 999) > 12)
</script>

<StatusBar
  health={cockpit.health}
  snapshot={cockpit.snapshot}
  online={cockpit.online}
  ageSec={cockpit.ageSec}
  lastError={cockpit.lastError}
/>

{#if needsDevToken}
  <div class="alert">
    <strong>개발 서버 — 토큰이 필요하다</strong>
    <div class="why">
      데몬이 서빙할 때는 자동으로 실린다. vite 개발 서버에서는 직접 넣어야 한다:
      <code>{'{data-dir}'}/api-token</code>
    </div>
    <form
      onsubmit={(e) => {
        e.preventDefault()
        setDevToken(devToken)
        location.reload()
      }}
    >
      <input type="password" bind:value={devToken} placeholder="api-token 파일의 내용" />
      <button type="submit">저장</button>
    </form>
  </div>
{/if}

<main class:stale>
  <GuardBanner guards={cockpit.snapshot?.guards} now={Date.now()} />

  <Positions positions={cockpit.snapshot?.positions} />
  <PlanPanel plan={cockpit.plan} />
  <Orphans orphans={cockpit.snapshot?.orphans ?? []} planOrphans={cockpit.plan?.orphans ?? []} />
  <Ledger
    ledger={cockpit.ledger}
    mode={cockpit.ledgerMode}
    daemonMode={cockpit.health?.mode}
    error={cockpit.ledgerError}
    onmode={(m) => cockpit.setLedgerMode(m)}
  />

  {#if !cockpit.everLoaded && !cockpit.lastError}
    <p class="empty">데몬에 붙는 중…</p>
  {/if}
</main>

<footer>
  <span>
    적용된 seq {cockpit.snapshot?.applied_seq ?? '—'}
    <!-- ★ 스냅샷 시각(as_of)이 아니라 **목표가 근거한 봉**을 쓴다. 신호가 얼마나 묵었는지는
         데몬이 언제 대답했는지가 아니라 그 값이 말해준다. -->
    · 목표 기준봉 {clockDate(cockpit.plan?.as_of_bar)}
  </span>
  <!-- ★ 이 한 줄이 이 화면의 성격을 정한다: 화면은 창구지 봇이 아니다. -->
  <span class="faint">창을 닫아도 데몬은 계속 돈다.</span>
</footer>

<style>
  main {
    display: block;
  }
  form {
    margin-top: 0.5rem;
    display: flex;
    gap: 0.5rem;
  }
  footer {
    display: flex;
    gap: 1rem;
    flex-wrap: wrap;
    margin-top: 1.6rem;
    padding-top: 0.8rem;
    border-top: 1px solid var(--line);
    font-family: var(--mono);
    font-size: 0.75rem;
    color: var(--fg-dim);
  }
  footer span:last-child {
    margin-left: auto;
  }
</style>
