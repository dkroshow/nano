<script>
  // The always-visible account-headroom band: how much of each provider's
  // subscription limit is already spent. It sits under the brand row because
  // that is the one band present in every rail state — compact, expanded, and
  // the phone drawer — so the answer to "am I about to run out?" never needs
  // a click.
  //
  // The server does all the provider-specific work (usage_limits.go); this
  // reads one endpoint and draws bars.

  import { app } from '../../app.svelte.js'

  const POLL_MS = 60_000

  let providers = $state([])
  let fetchFailed = $state(false)
  let now = $state(Date.now())

  async function load() {
    try {
      const res = await fetch('/api/v1/usage-limits')
      if (!res.ok) throw new Error(String(res.status))
      const body = await res.json()
      providers = Array.isArray(body?.providers) ? body.providers : []
      fetchFailed = false
    } catch {
      // Keep the last drawn bars; a blank strip reads as "no usage" which is
      // the opposite of what a failed poll means.
      fetchFailed = true
    }
  }

  $effect(() => {
    load()
    const timer = window.setInterval(load, POLL_MS)
    return () => window.clearInterval(timer)
  })

  // Drives the reset countdowns without re-fetching.
  $effect(() => {
    const timer = window.setInterval(() => (now = Date.now()), 30_000)
    return () => window.clearInterval(timer)
  })

  const PROVIDER_LABEL = { claude: 'claude', codex: 'codex', grok: 'grok' }

  // The strip answers one question: how much of this week is spent. So it
  // shows weekly caps only — Claude's all-models cap plus each per-model cap
  // it meters separately, Codex's account-wide weekly bucket, and Grok's one
  // primary subscription bucket (which can be weekly or monthly).
  //
  // The server collects and logs more than this (Claude's rolling 5-hour
  // window, Codex's per-model buckets). Those stay in the database for the
  // time-series; they just do not earn a bar here.
  function visibleBuckets(provider) {
    const all = provider.buckets || []
    if (provider.provider === 'codex') return all.filter((b) => b.key === 'codex:primary')
    if (provider.provider === 'grok') return all.filter((b) => b.primary)
    return all.filter((b) => b.group === 'weekly')
  }

  function tone(bucket) {
    if (bucket.usedPercent >= 90) return 'crit'
    if (bucket.usedPercent >= 75) return 'warn'
    if (bucket.severity && bucket.severity !== 'normal') return 'warn'
    return ''
  }

  // Short, glanceable: "4h", "2d". Precision past that is noise on a bar.
  function untilReset(resetsAt) {
    if (!resetsAt) return ''
    const ms = resetsAt * 1000 - now
    if (ms <= 0) return 'due'
    const mins = Math.round(ms / 60_000)
    if (mins < 60) return `${mins}m`
    const hours = Math.round(mins / 60)
    if (hours < 48) return `${hours}h`
    return `${Math.round(hours / 24)}d`
  }

  function ageLabel(observedAt) {
    if (!observedAt) return ''
    const ms = now - new Date(observedAt).getTime()
    if (!Number.isFinite(ms) || ms < 10 * 60_000) return ''
    const mins = Math.round(ms / 60_000)
    if (mins < 120) return `${mins}m old`
    return `${Math.round(mins / 60)}h old`
  }

  // The countdown is the glanceable form; the tooltip carries the wall-clock
  // time, which is what you need when deciding whether to wait it out.
  function resetTitle(bucket) {
    if (!bucket.resetsAt) return 'reset time not reported'
    return `resets ${new Date(bucket.resetsAt * 1000).toLocaleString()}`
  }

  function bucketTitle(providerName, bucket, observedAt) {
    const parts = [`${providerName} · ${bucket.label}: ${Math.round(bucket.usedPercent)}% used`]
    if (bucket.resetsAt) parts.push(`resets in ${untilReset(bucket.resetsAt)}`)
    if (bucket.severity && bucket.severity !== 'normal') parts.push(bucket.severity)
    const age = ageLabel(observedAt)
    if (age) parts.push(age)
    return parts.join(' · ')
  }

  // One row per bar. The rail is 270px wide, so several bars cannot share a
  // line without the labels and percentages colliding; the provider name is
  // printed once, on its first row, and the rows below it hang under it.
  const rows = $derived.by(() => {
    const out = []
    for (const p of providers) {
      const name = PROVIDER_LABEL[p.provider] || p.provider
      const buckets = visibleBuckets(p)
      // A provider whose reads are FAILING keeps its last good bars, but they
      // are dimmed and dated, and an alert row is printed under them. Silently
      // showing week-old numbers as if they were current is the one outcome
      // this strip must never produce.
      //
      // A NOTICE is the opposite case: something briefly not current that
      // resolves itself and needs nothing from the operator. It gets a muted
      // line and leaves the bars alone. Painting one of those red is what sent
      // the operator hunting for an account problem that did not exist.
      const failed = !!p.error
      buckets.forEach((bucket, i) => {
        out.push({
          kind: 'bar',
          id: `${p.provider}:${bucket.key}`,
          provider: name,
          lead: i === 0,
          bucket,
          observedAt: p.observedAt,
          failed,
          // The reset-credit pill belongs to the account, not a window, so it
          // rides the provider's first row.
          credits: i === 0 ? p.resetCredits || 0 : 0,
        })
      })
      if (failed) {
        out.push({
          kind: 'alert',
          id: `${p.provider}:alert`,
          provider: name,
          message: p.error,
          detail: p.errorDetail,
          // Present only when there are stale bars above to date.
          age: buckets.length ? ageLabel(p.observedAt) : '',
        })
      } else if (p.notice) {
        out.push({ kind: 'notice', id: `${p.provider}:notice`, provider: name, message: p.notice })
      }
    }
    return out
  })

  const anyCredits = $derived(rows.some((r) => r.credits > 0))
  const anyAlert = $derived(rows.some((r) => r.kind === 'alert'))
</script>

{#if rows.length}
  <!-- The whole band is the affordance: there is no room in a 270px rail for a
       separate "details" control, and every row in it is about the same
       subject. -->
  <button
    class="usage-strip"
    class:disconnected={fetchFailed}
    class:has-alert={anyAlert}
    data-testid="usage-limits"
    title="Open usage detail — charts, reset times, call log"
    aria-haspopup="dialog"
    onclick={() => (app.usageDetailOpen = true)}
  >
    {#each rows as row (row.id)}
      {#if row.kind === 'notice'}
        <!-- Muted, no mark, no red edge: nothing here is wrong. -->
        <div class="usage-notice" data-testid="usage-notice">
          {row.provider}: {row.message}
        </div>
      {:else if row.kind === 'alert'}
        <!-- Printed, not hovered. An error that only exists in a tooltip is an
             error nobody sees. -->
        <div class="usage-alert" data-testid="usage-alert" title={row.detail || row.message}>
          <span class="usage-alert-mark" aria-hidden="true">!</span>
          <span class="usage-alert-text">
            {row.provider}: {row.message}{row.age ? ` · figures above are ${row.age}` : ''}
          </span>
        </div>
      {:else}
        <div class="usage-row" class:lead={row.lead} class:stale={row.failed}>
          <span class="usage-provider">{row.lead ? row.provider : ''}</span>
          <span class="usage-label" title={row.bucket.label}>{row.bucket.label}</span>
          <span class="usage-track" title={bucketTitle(row.provider, row.bucket, row.observedAt)}>
            <span
              class="usage-fill {tone(row.bucket)}"
              style="transform: scaleX({Math.min(100, Math.max(0, row.bucket.usedPercent)) / 100})"
            ></span>
          </span>
          <span class="usage-pct {tone(row.bucket)}">{Math.round(row.bucket.usedPercent)}%</span>
          <span class="usage-reset" title={resetTitle(row.bucket)}>{untilReset(row.bucket.resetsAt)}</span>
          {#if anyCredits}
            <!-- Reserved on every row, not just the one with the pill:
                 without it that row's bar is shorter than its neighbours and
                 the bars stop being comparable at a glance, which is their
                 whole job. -->
            <span class="usage-credit-slot">
              {#if row.credits > 0}
                <span
                  class="usage-credit"
                  title="{row.credits} unused limit reset{row.credits === 1 ? '' : 's'} available"
                >+{row.credits}</span>
              {/if}
            </span>
          {/if}
        </div>
      {/if}
    {/each}
    {#if fetchFailed}
      <!-- Distinct from a provider failure: here nano itself is unreachable
           from this tab, so every bar on screen is frozen, not just one
           provider's. -->
      <div class="usage-alert" data-testid="usage-alert">
        <span class="usage-alert-mark" aria-hidden="true">!</span>
        <span class="usage-alert-text">nano unreachable — all figures frozen</span>
      </div>
    {/if}
  </button>
{/if}
