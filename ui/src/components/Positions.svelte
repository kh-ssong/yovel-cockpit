<script>
  import Panel from './Panel.svelte'
  import { num, pct, sign, clock, symbolOf } from '../lib/format.js'

  let { positions } = $props()
  const rows = $derived(positions ?? [])
</script>

<Panel
  title="포지션"
  count={rows.length}
  note="미실현% 는 데몬이 마지막으로 본 시세 기준이다 — 체결가가 아니다."
>
  {#if rows.length === 0}
    <p class="empty">보유 없음.</p>
  {:else}
    <table>
      <thead>
        <tr>
          <th>종목</th>
          <th>슬롯</th>
          <th class="num">수량</th>
          <th class="num">평단</th>
          <th class="num">미실현</th>
          <th class="num">stop</th>
          <th>TP</th>
          <th class="num">진입</th>
        </tr>
      </thead>
      <tbody>
        {#each rows as p (p.intent_id)}
          <tr>
            <td class="mono">{symbolOf(p.symbol)}</td>
            <td class="dim">{p.slot || '—'}</td>
            <td class="num">{num(p.qty)}</td>
            <td class="num">{num(p.avg_entry_price)}</td>
            <td class="num {sign(p.unrealized_pct)}">{pct(p.unrealized_pct)}</td>
            <td class="num">
              {#if p.stop_armed}
                {num(p.stop_armed)}
              {:else}
                <!-- ★ stop 이 안 걸린 포지션은 조용히 넘기지 않는다. 로컬 청산층이
                     비어 있다는 뜻이고, 그게 이 시스템에서 제일 비싼 공백이다. -->
                <span class="down" title="로컬 손절이 걸려 있지 않다">없음</span>
              {/if}
            </td>
            <td class="dim">{p.tp_order_id ? '위임됨' : '—'}</td>
            <td class="num dim">{clock(p.entry_at)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</Panel>
