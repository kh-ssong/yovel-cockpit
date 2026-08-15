<script>
  import Panel from './Panel.svelte'
  import { num, krw, pct, clock, symbolOf } from '../lib/format.js'
  import { explain, ACK_STATUS } from '../lib/codes.js'

  let { plan } = $props()

  const enters = $derived(plan?.enters ?? [])
  const exits = $derived(plan?.exits ?? [])
  const stops = $derived(plan?.stop_updates ?? [])
  const tps = $derived(plan?.tp_updates ?? [])
  const dropped = $derived(plan?.dropped_enters ?? 0)

  // ★ ack 전부를 늘어놓지 않는다. "반영됨" 은 표에서 이미 보이고, 사람이 이 화면을 여는 건
  // 대개 **안 된 것**을 볼 때다. 다만 침묵도 아니다 — 사유를 코드와 함께 남긴다.
  const blocked = $derived((plan?.acks ?? []).filter((a) => a.codes?.length))
  const total = $derived(enters.length + exits.length + stops.length + tps.length)
</script>

<Panel
  title="이번 틱 계획"
  count={total}
  note="계획은 아직 주문이 아니다 — reconcile 이 낸 순수 산출이고, 집행은 다음 틱에 executor 가 한다."
>
  {#if total === 0 && blocked.length === 0}
    <p class="empty">할 일 없음 — 목표와 실상태가 일치한다.</p>
  {/if}

  {#if enters.length}
    <table>
      <thead>
        <tr>
          <th>진입</th>
          <th>슬롯</th>
          <th class="num">수량</th>
          <th class="num">기준가</th>
          <th class="num">금액</th>
          <th class="num">실현비중</th>
          <th class="num">유효기한</th>
        </tr>
      </thead>
      <tbody>
        {#each enters as e (e.target.intent_id)}
          <tr>
            <td class="mono">{symbolOf(e.target.symbol)}</td>
            <td class="dim">{e.target.slot || '—'}</td>
            <td class="num">{num(e.qty)}</td>
            <td class="num">{num(e.price)}</td>
            <td class="num">{krw(e.notional)}</td>
            <td class="num">
              {pct(e.realized_weight * 100)}
              {#if e.target.weight && Math.abs(e.realized_weight - e.target.weight) > 0.005}
                <!-- ★ whole-share 내림으로 의도한 비중과 갈렸다. 소액 계좌에서 이 왜곡이
                     백테와 라이브를 가르는 지점이라 숨기지 않는다. -->
                <span class="faint" title="목표 비중 {pct(e.target.weight * 100)}">
                  ← {pct(e.target.weight * 100)}
                </span>
              {/if}
            </td>
            <td class="num dim">{clock(e.target.entry?.not_after)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}

  {#if exits.length}
    <table>
      <thead>
        <tr><th>청산</th><th>사유</th><th class="num">수량</th><th class="num">평단</th></tr>
      </thead>
      <tbody>
        {#each exits as x (x.position.intent_id)}
          <tr>
            <td class="mono">{symbolOf(x.position.symbol)}</td>
            <td class="dim">{x.reason === 'flat' ? '목표가 flat' : x.reason}</td>
            <td class="num">{num(x.position.qty)}</td>
            <td class="num">{num(x.position.avg_entry_price)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}

  {#if stops.length || tps.length}
    <table>
      <thead>
        <tr><th>가격 갱신</th><th>무엇</th><th class="num">전</th><th class="num">후</th></tr>
      </thead>
      <tbody>
        {#each stops as s (s.position.intent_id)}
          <tr>
            <td class="mono">{symbolOf(s.position.symbol)}</td>
            <td class="dim">stop</td>
            <td class="num faint">{s.from ? num(s.from) : '—'}</td>
            <td class="num">{num(s.to)}</td>
          </tr>
        {/each}
        {#each tps as t (t.position.intent_id)}
          <tr>
            <td class="mono">{symbolOf(t.position.symbol)}</td>
            <td class="dim">TP 위임</td>
            <td class="num faint">{t.from ? num(t.from) : '—'}</td>
            <td class="num">{num(t.to)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}

  {#if dropped > 0}
    <div class="drop">
      ★ 주문 상한에 걸려 진입 {dropped}건이 이번 틱에서 잘렸다 (청산은 자르지 않는다).
      다음 틱에 다시 시도한다.
    </div>
  {/if}

  {#if blocked.length}
    <table>
      <thead>
        <tr><th>안 나간 목표</th><th>상태</th><th>사유</th></tr>
      </thead>
      <tbody>
        {#each blocked as a (a.intent_id)}
          <tr>
            <td class="mono faint">{a.intent_id}</td>
            <td class="dim">{ACK_STATUS[a.status] ?? a.status}</td>
            <td>
              {#each a.codes as c}
                <div><code class="faint">{c}</code> <span class="dim">{explain(c)}</span></div>
              {/each}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</Panel>

<style>
  .drop {
    padding: 0.6rem 1rem;
    color: var(--warn);
    font-size: 0.85rem;
  }
  table + table {
    border-top: 1px solid var(--line);
  }
</style>
