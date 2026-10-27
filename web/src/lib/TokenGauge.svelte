<script>
  let { usage } = $props()

  const total = $derived(usage?.total?.totalTokens ?? 0)
  const window_ = $derived(usage?.modelContextWindow ?? 0)
  const pct = $derived(window_ ? Math.min(100, (total / window_) * 100) : 0)

  function fmt(n) {
    if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M'
    if (n >= 1_000) return (n / 1_000).toFixed(1) + 'k'
    return String(n)
  }
</script>

{#if usage}
  <div class="gauge" title="context window fill">
    <div class="bar"><div class="fill" class:hot={pct > 75} style="transform: scaleX({pct / 100})"></div></div>
    <span>{fmt(total)} / {fmt(window_)}</span>
  </div>
{/if}
