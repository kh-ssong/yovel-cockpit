<script>
  import Panel from './Panel.svelte'
  import { num, symbolOf } from '../lib/format.js'

  // 유령 = 목표에 없는데 실제로 들고 있는 것. state 는 종목만, plan 은 포지션까지 준다.
  let { orphans = [], planOrphans = [] } = $props()

  const rows = $derived(
    planOrphans.length
      ? planOrphans.map((p) => ({ code: symbolOf(p.symbol), qty: p.qty, intent: p.intent_id }))
      : orphans.map((s) => ({ code: symbolOf(s), qty: null, intent: null })),
  )
</script>

{#if rows.length}
  <Panel
    title="유령 보유"
    count={rows.length}
    note="★ 자동 청산하지 않는다. 서버가 상태를 한 번 잘못 계산하거나 스냅샷이 잘리면, '목표에 없으면 판다' 는 규칙이 사용자의 수동 보유분까지 털어버린다."
  >
    <table>
      <thead>
        <tr><th>종목</th><th class="num">수량</th><th>intent</th></tr>
      </thead>
      <tbody>
        {#each rows as r (r.code + (r.intent ?? ''))}
          <tr>
            <td class="mono">{r.code}</td>
            <td class="num">{r.qty === null ? '—' : num(r.qty)}</td>
            <td class="faint mono">{r.intent ?? '—'}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </Panel>
{/if}
