<script>
  let { title, count = null, note = null, actions, children } = $props()
</script>

<section class="panel">
  <header>
    <h2>{title}</h2>
    {#if count !== null}<span class="count">{count}</span>{/if}
    <span class="spacer"></span>
    {@render actions?.()}
  </header>
  <div class="body">
    {@render children?.()}
  </div>
  {#if note}
    <!-- ★ 패널마다 "이 표가 무엇이 아닌지" 를 한 줄 달아 둔다. 표만 보면 사람은 늘
         가장 낙관적인 해석을 고른다 (특히 paper 원장을 실적으로 읽는 것). -->
    <div class="note">{note}</div>
  {/if}
</section>
