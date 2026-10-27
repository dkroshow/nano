<script>
  // The header's connection indicator (connection.svelte.js measures): how
  // long ago Nano last answered, counted every second, then Nano's latency and
  // throughput as five-minute averages, each with an arrow for its trend, or
  // how long Nano has been out of reach.
  import { connection, connectionMetrics } from '../connection.svelte.js'

  const ARROW = { improving: '↑', worsening: '↓', stable: '→' }

  let now = $state(Date.now())
  // A display clock only: it reads the time of the last answer and asks Nano
  // nothing. It restarts on each answer, so the count steps on whole seconds
  // after it. A hidden page skips the redraw.
  $effect(() => {
    void connection.lastSuccessAt
    now = Date.now()
    const timer = setInterval(() => {
      if (document.visibilityState !== 'hidden') now = Date.now()
    }, 1_000)
    return () => clearInterval(timer)
  })

  const metrics = $derived(connection.status === 'connected' ? connectionMetrics() : [])

  function duration(ms) {
    const seconds = Math.max(0, Math.floor(ms / 1_000))
    if (seconds < 60) return `${seconds}s`
    const minutes = Math.floor(seconds / 60)
    return minutes < 60 ? `${minutes}m` : `${Math.floor(minutes / 60)}h`
  }

  const sinceSuccess = $derived(connection.lastSuccessAt ? duration(Math.max(now, connection.lastSuccessAt) - connection.lastSuccessAt) : '')

  const title = $derived(
    connection.status === 'offline'
      ? 'Nano cannot be reached from this browser. A message you send is kept and sent when it can be.'
      : connection.status === 'checking'
        ? 'Checking the connection to Nano'
        : `Connection to Nano, five-minute averages: ${metrics.map((metric) => `${metric.name.toLowerCase()} ${metric.value}, ${metric.trend}`).join('; ')}`,
  )
</script>

<div
  class="connection-status {connection.status}"
  data-testid="connection-status"
  title={title}
  aria-label={title}
>
  <span class="connection-dot" aria-hidden="true"></span>
  {#if connection.status === 'offline'}
    <span>Offline {duration(now - connection.offlineSinceAt)}</span>
  {:else if connection.status === 'checking'}
    <span>Checking…</span>
  {/if}
  {#if sinceSuccess}
    <span class="connection-last"><span class="connection-last-label">Last success:</span> {sinceSuccess} ago</span>
  {/if}
  {#if connection.status === 'connected'}
    {#each metrics as metric (metric.name)}
      <span class="connection-metric {metric.trend}">
        {metric.value}<span class="connection-trend" aria-hidden="true">{ARROW[metric.trend]}</span>
      </span>
    {/each}
  {/if}
</div>

<style>
  .connection-status {
    margin-left: auto; display: flex; align-items: center; gap: 8px; min-width: 0;
    color: var(--text2); font-family: var(--mono); font-size: 11px; white-space: nowrap;
  }
  .connection-dot {
    width: 7px; height: 7px; border-radius: 50%; flex-shrink: 0;
    background: var(--text3);
  }
  .connected .connection-dot { background: var(--green); }
  .offline { color: var(--red); }
  .offline .connection-dot { background: var(--red); }
  .connection-metric { display: inline-flex; align-items: baseline; gap: 2px; }
  .connection-trend { color: var(--text3); }
  .offline .connection-last { color: var(--text2); }
  @media (max-width: 700px) {
    .connection-last-label { display: none; }
  }
  .improving .connection-trend { color: var(--green); }
  .worsening .connection-trend { color: var(--red); }
</style>
