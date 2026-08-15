<script>
  import Panel from './Panel.svelte'
  import { num, krw, pct, sign, clockDate, symbolOf } from '../lib/format.js'

  let { ledger, mode, daemonMode, error, onmode } = $props()

  const rows = $derived(ledger?.orders ?? [])

  // ★ 합산은 두 가지만 한다: 건수와 수수료. 체결가·실현%는 건별 값이라 더하면 거짓말이 된다
  // (per-trade % 를 합치면 규모를 무시한 수치가 나온다).
  const fees = $derived(rows.reduce((a, o) => a + (o.fee_krw ?? 0), 0))
  const manual = $derived(rows.filter((o) => o.source === 'manual').length)
</script>

<Panel
  title="원장"
  count={ledger?.count ?? 0}
  note={mode === 'paper'
    ? 'paper 원장이다 — 성과의 증거가 아니다. 체결·슬리피지가 가정이고, 그 가정이 손익분기 근처 전략의 판정을 뒤집는다.'
    : '실계좌 원장. ★ paper 와 절대 합산해 보지 말 것 — 합산하면 손실이 수익으로 보인다.'}
>
  {#snippet actions()}
    <!-- ★ 기본값 "전체" 를 두지 않는다. 데몬 API 도 mode 없으면 400 으로 거절한다. -->
    <button aria-pressed={mode === 'paper'} onclick={() => onmode('paper')}>paper</button>
    <button aria-pressed={mode === 'live'} onclick={() => onmode('live')}>live</button>
    {#if mode && daemonMode && mode !== daemonMode}
      <span class="badge warn" title="지금 도는 모드는 {daemonMode} 다">
        지금 도는 건 {daemonMode}
      </span>
    {/if}
  {/snippet}

  {#if error}
    <p class="empty">{error}</p>
  {:else if rows.length === 0}
    <p class="empty">{mode} 기록 없음.</p>
  {:else}
    <div class="sum mono">
      <span>{rows.length}건</span>
      <span class="faint">수수료 {krw(fees)}</span>
      {#if manual}
        <!-- ★ 사용자가 HTS 로 직접 낸 것을 봇 청산으로 오인하면 형제 레그까지 잘못 청산된다. -->
        <span class="badge warn">수동 {manual}건 섞임</span>
      {/if}
    </div>
    <table>
      <thead>
        <tr>
          <th>시각</th>
          <th>종목</th>
          <th>단계</th>
          <th class="num">수량</th>
          <th class="num">가격</th>
          <th class="num">slip</th>
          <th class="num">실현</th>
          <th>사유</th>
        </tr>
      </thead>
      <tbody>
        {#each rows as o (o.id)}
          <tr>
            <td class="mono dim">{clockDate(o.filled_at ?? o.submitted_at ?? o.created_at)}</td>
            <td class="mono">{symbolOf(o.symbol)}</td>
            <td class="dim">
              {o.side === 'buy' ? '매수' : '매도'}
              <span class="faint">{o.phase}</span>
              {#if o.source === 'manual'}<span class="badge warn">수동</span>{/if}
            </td>
            <td class="num">{num(o.qty)}</td>
            <!-- ★ 0 을 그대로 찍지 않는다. 브로커가 사후 감지한 청산(우리가 낸 주문이 아닌 것)은
                 체결가를 모르는데, 0 원으로 보이면 "공짜로 팔렸다" 로 읽힌다. -->
            <td class="num">{o.price ? num(o.price) : '—'}</td>
            <td class="num faint">{o.slippage_bp ? num(o.slippage_bp, 1) + 'bp' : '—'}</td>
            <td class="num {sign(o.realized_pct)}">{o.realized_pct ? pct(o.realized_pct) : '—'}</td>
            <td class="dim">{o.exit_reason || o.broker_code || '—'}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</Panel>

<style>
  .sum {
    display: flex;
    gap: 0.8rem;
    align-items: center;
    padding: 0.35rem 1rem 0.5rem;
    font-size: 0.82rem;
    color: var(--fg-dim);
  }
</style>
