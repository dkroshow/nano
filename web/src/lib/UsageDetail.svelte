<script>
  // The usage detail modal: three step charts, the reset schedule, and the recent
  // call log. Opened from the rail strip.
  //
  // Chart choices worth knowing before editing:
  //
  //  - STEP lines, not straight ones. The server stores a row only when a
  //    reading changes, so between two stored points the value genuinely held
  //    flat. Sloping between them would invent a gradual climb that never
  //    happened and would misread a flat week as a slow rise.
  //  - One shared y-axis per chart, fixed 0-100. Every series is a percentage
  //    of its own cap, so they are directly comparable; a second y-scale would
  //    make two different things look like one.
  //  - Series colours are validated for colour-vision deficiency against
  //    nano's own surfaces. Every series is ALSO direct-labelled at its last
  //    point, so identity never rests on colour alone.
  //  - Providers get separate charts because their limit windows differ and
  //    their reset clocks are unrelated.

  import {
    USAGE_CHART_PAD,
    USAGE_CHART_PLOT_H,
    usageChartXLabel,
    usageChartXTicks,
    usageChartYLabel,
  } from './usage_chart.js'

  let { open = false, onClose } = $props()

  const POLL_MS = 30_000

  let providers = $state([])
  let series = $state([])
  let attempts = $state([])
  let pollSeconds = $state(0)
  // The window the operator picked ends at now, so the latest reading is the
  // right edge. Reset times stay in the schedule table. Extending the axis
  // out to a future reset parked that reading far to the left.
  let hours = $state(72)
  let loading = $state(true)
  let loadError = $state('')
  let now = $state(Date.now())

  async function load() {
    try {
      const res = await fetch(`/api/v1/usage-limits/detail?hours=${hours}`)
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const body = await res.json()
      providers = body.providers || []
      series = body.series || []
      attempts = body.attempts || []
      pollSeconds = body.pollSeconds || 0
      loadError = ''
    } catch (err) {
      loadError = String(err?.message || err)
    } finally {
      loading = false
    }
  }

  $effect(() => {
    if (!open) return
    hours
    loading = true
    load()
    const timer = window.setInterval(load, POLL_MS)
    const clock = window.setInterval(() => (now = Date.now()), 15_000)
    return () => {
      window.clearInterval(timer)
      window.clearInterval(clock)
    }
  })

  function onKeydown(event) {
    if (event.key === 'Escape' && open) onClose?.()
  }

  // Categorical slots 1-3 of the validated palette, stepped per mode. Checked
  // with the palette validator against nano's own chart surfaces (#1e1f20 dark,
  // #e8e8e8 light): all gates pass, worst colour-blind separation dE 9.4 dark /
  // 9.2 light. Do not swap these for nano's brand green and accent — that pair
  // scores dE 1.5 for deuteranopes, i.e. identical.
  const DARK_SLOTS = ['#3987e5', '#d95926', '#199e70']
  const LIGHT_SLOTS = ['#2a78d6', '#eb6834', '#1baf7a']
  let isLight = $state(false)
  $effect(() => {
    if (!open) return
    const root = document.documentElement
    const sync = () => (isLight = root.classList.contains('light'))
    sync()
    const observer = new MutationObserver(sync)
    observer.observe(root, { attributes: true, attributeFilter: ['class'] })
    return () => observer.disconnect()
  })
  const slots = $derived(isLight ? LIGHT_SLOTS : DARK_SLOTS)

  const PROVIDERS = ['claude', 'codex', 'grok']
  // Spark variants are rail-only; charting them adds a second Codex caption
  // row and undoes the compact one-row band. Hide every bengalfox key, not
  // just the original primary id — a later secondary weekly series has the
  // same label and would otherwise plot beside week.
  function isHiddenChartKey(key) {
    return String(key || '').startsWith('codex_bengalfox:')
  }

  // Subscription caps only, matching the rail strip. The rolling 5-hour window is
  // excluded: it is the fastest-moving line and it dominated the plot without
  // answering the question these charts exist for, which is how the WEEK is
  // going. The server still reads and stores it, so the history is intact and
  // charting it again is a one-line change here.
  function seriesFor(provider) {
    return series
      .filter((s) => s.provider === provider && s.group !== 'session' && !isHiddenChartKey(s.key) && (s.points || []).length)
      .map((s, i) => ({ ...s, color: slots[i % slots.length] }))
  }

  function snapshotFor(provider) {
    return providers.find((p) => p.provider === provider)
  }

  // ---- chart geometry -----------------------------------------------------
  const W = 560
  // The right gutter holds the direct labels. It is sized to LABEL_MAX
  // characters of 9px monospace plus the value — a label longer than that is
  // truncated rather than allowed to run off the panel.
  const PAD = { ...USAGE_CHART_PAD, top: 12 }
  const LABEL_MAX = 11

  function shortLabel(label) {
    return label.length > LABEL_MAX ? label.slice(0, LABEL_MAX - 1) + '…' : label
  }
  const plotW = W - PAD.left - PAD.right
  const plotH = USAGE_CHART_PLOT_H

  function timeDomain() {
    return [now - hours * 3600_000, now]
  }

  function xOf(ms, [t0, t1]) {
    if (t1 === t0) return PAD.left
    const clamped = Math.min(t1, Math.max(t0, ms))
    return PAD.left + ((clamped - t0) / (t1 - t0)) * plotW
  }

  // y is a fixed 0-100 percentage scale, so charts are comparable to each
  // other and a bar at 90% always sits in the same place.
  function yOf(pct, top) {
    return top + (1 - Math.min(100, Math.max(0, pct)) / 100) * plotH
  }

  // Step path: hold the previous value until the moment the next reading was
  // recorded, then jump. The line stops at now, the right edge of the plot.
  function stepPath(points, domain, top) {
    if (!points.length) return ''
    const d = []
    points.forEach((p, i) => {
      const x = xOf(new Date(p.at).getTime(), domain)
      const y = yOf(p.percent, top)
      if (i === 0) d.push(`M ${x.toFixed(1)} ${y.toFixed(1)}`)
      else d.push(`H ${x.toFixed(1)}`, `V ${y.toFixed(1)}`)
    })
    d.push(`H ${xOf(Math.min(now, domain[1]), domain).toFixed(1)}`)
    return d.join(' ')
  }

  function lastPoint(s, domain, top) {
    const p = s.points[s.points.length - 1]
    return { x: xOf(Math.min(now, domain[1]), domain), y: yOf(p.percent, top), percent: p.percent }
  }

  // Direct labels sit at each line's live end, so two series within a few
  // percent of each other would print on top of one another. Nudge the LABELS
  // apart vertically while the dots stay on their true values — the dot is the
  // data, the label is annotation.
  const LABEL_GAP = 11

  function labelLayout(rows, domain, top) {
    const items = rows.map((s) => {
      const p = lastPoint(s, domain, top)
      return { key: s.key, label: s.label, color: s.color, x: p.x, dotY: p.y, y: p.y, percent: p.percent }
    })
    items.sort((a, b) => a.y - b.y)
    for (let i = 1; i < items.length; i++) {
      if (items[i].y - items[i - 1].y < LABEL_GAP) items[i].y = items[i - 1].y + LABEL_GAP
    }
    // Pushing down can run past the plot; if so, shift the whole stack back up.
    const overflow = items.length ? items[items.length - 1].y - (top + plotH) : 0
    if (overflow > 0) for (const it of items) it.y -= overflow
    return items
  }

  // ---- crosshair -----------------------------------------------------------
  // hover maps provider -> the hovered timestamp in ms, or null.
  //
  // The hairline follows the pointer freely rather than snapping to stored
  // readings. Snapping is the right default for discrete series, but these are
  // step lines: the value is defined at every x, and readings are stored only
  // when a number changes, so a flat stretch has almost no points to snap to
  // and the hairline would jump minutes away from the cursor.
  let hover = $state({})

  // Stored change points, plus "now" — used for keyboard stepping, where
  // landing on the moments the value actually moved is what you want.
  function changeTimes(rows, domain) {
    const set = new Set([Math.min(now, domain[1])])
    for (const s of rows) {
      for (const p of s.points) {
        const t = new Date(p.at).getTime()
        if (t >= domain[0] && t <= domain[1]) set.add(t)
      }
    }
    return [...set].sort((a, b) => a - b)
  }

  // The value a step line shows at time t is the last reading AT OR BEFORE t,
  // never the nearest one. Reading the nearest would put the tooltip a step
  // ahead of the line it is annotating whenever the pointer sits just left of
  // a change.
  function valueAt(series, t) {
    let value = null
    for (const p of series.points) {
      if (new Date(p.at).getTime() <= t) value = p.percent
      else break
    }
    return value
  }

  function pointerToTime(event, domain) {
    const rect = event.currentTarget.getBoundingClientRect()
    if (!rect.width) return null
    const xUser = ((event.clientX - rect.left) / rect.width) * W
    const frac = (xUser - PAD.left) / plotW
    const t = domain[0] + frac * (domain[1] - domain[0])
    return Math.min(now, domain[1], Math.max(domain[0], t))
  }

  // Arrow keys walk the moments the value changed, not pixels — that is where
  // there is anything to read.
  function moveHover(provider, times, delta) {
    if (!times.length) return
    const cur = hover[provider]
    if (cur == null) {
      hover[provider] = times[times.length - 1]
      return
    }
    let i = times.findIndex((t) => t >= cur)
    if (i < 0) i = times.length - 1
    if (delta > 0 && times[i] <= cur) i += 1
    else if (delta < 0) i -= 1
    hover[provider] = times[Math.min(times.length - 1, Math.max(0, i))]
  }

  function crosshairTime(ms) {
    return new Date(ms).toLocaleString([], {
      weekday: 'short',
      hour: 'numeric',
      minute: '2-digit',
      hour12: true,
    })
  }

  // Horizontal gridlines at 0/25/50/75/100 — recessive, for reading a value
  // off the line without a label on every point.
  const GRID = [0, 25, 50, 75, 100]



  // ---- resets -------------------------------------------------------------
  function resetRows() {
    const out = []
    for (const p of providers) {
      for (const b of p.buckets || []) {
        if (!b.resetsAt) continue
        out.push({
          id: `${p.provider}:${b.key}`,
          provider: p.provider,
          label: b.label,
          percent: b.usedPercent,
          at: b.resetsAt * 1000,
        })
      }
    }
    return out.sort((a, b) => a.at - b.at)
  }

  // A one-time usage reset has its own expiration. It is NOT the same thing as
  // the usage window's reset time, which appears in the schedule below.
  function resetCreditRows() {
    const out = []
    for (const p of providers) {
      const credits = (p.resetCreditDetails || []).filter((credit) => !credit.status || credit.status === 'available')
      if (credits.length) {
        for (const credit of credits) {
          out.push({
            id: `${p.provider}:${credit.id || credit.title || 'credit'}`,
            provider: p.provider,
            title: credit.title || 'Usage reset',
            expiresAt: Number(credit.expiresAt || 0) * 1000,
          })
        }
      } else if (Number(p.resetCredits || 0) > 0) {
        // A protocol downgrade may provide the count without the credit list.
        // Keep the reset visible, but never make up an expiry time.
        out.push({ id: `${p.provider}:credit`, provider: p.provider, title: `${p.resetCredits} usage reset${p.resetCredits === 1 ? '' : 's'}`, expiresAt: 0 })
      }
    }
    return out.sort((a, b) => a.expiresAt - b.expiresAt)
  }

  function untilLabel(ms) {
    const delta = ms - now
    if (delta <= 0) return 'due now'
    const mins = Math.round(delta / 60_000)
    if (mins < 60) return `in ${mins}m`
    const hrs = Math.round(mins / 60)
    if (hrs < 48) return `in ${hrs}h`
    return `in ${Math.round(hrs / 24)}d`
  }

  function wallClock(ms) {
    const at = new Date(ms)
    const today = new Date(now)
    const dayAt = Date.UTC(at.getFullYear(), at.getMonth(), at.getDate())
    const dayToday = Date.UTC(today.getFullYear(), today.getMonth(), today.getDate())
    const dayOffset = Math.round((dayAt - dayToday) / 86_400_000)
    const day = dayOffset === 0 ? 'Today' : dayOffset === 1 ? 'Tomorrow' : at.toLocaleDateString([], {
      weekday: 'short',
    })
    const time = at.toLocaleTimeString([], {
      hour: 'numeric',
      minute: '2-digit',
      hour12: true,
    })
    return `${day} ${time}`
  }

  function stamp(iso) {
    return new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })
  }

  const okCount = $derived(attempts.filter((a) => a.ok).length)
</script>

<svelte:window onkeydown={onKeydown} />

{#if open}
  <!-- Click-outside closes; the panel stops propagation. -->
  <div class="ud-overlay" role="presentation" onclick={() => onClose?.()}>
    <div
      class="ud-modal"
      role="dialog"
      aria-modal="true"
      aria-label="Account usage detail"
      data-testid="usage-detail"
      onclick={(e) => e.stopPropagation()}
    >
      <div class="ud-head">
        <span class="ud-title">Account usage</span>
        <button class="ud-close" onclick={() => onClose?.()} aria-label="Close">×</button>
      </div>

      <div class="ud-body">
        {#if loadError}
          <p class="ud-load-error">Couldn't load detail: {loadError}</p>
        {:else if loading}
          <p class="ud-loading">Loading…</p>
        {/if}

        <div class="ud-layout">
          <div class="ud-charts" data-testid="usage-charts">
          <div class="ud-chart-toolbar">
            <span class="ud-chart-toolbar-label">History shown</span>
            <div class="ud-range" role="group" aria-label="History shown">
              {#each [6, 24, 72, 168] as h (h)}
                <button class="ud-range-btn" class:on={hours === h} onclick={() => (hours = h)}>
                  {h < 24 ? `${h}h` : `${h / 24}d`}
                </button>
              {/each}
            </div>
          </div>
          <!-- One chart per provider: their windows and reset clocks are
               unrelated, so a shared plot would imply a link that isn't there. -->
          {#each PROVIDERS as provider (provider)}
          {@const rows = seriesFor(provider)}
          {@const snap = snapshotFor(provider)}
          {@const domain = timeDomain()}
          <section class="ud-chart-block">
            <div class="ud-chart-head">
              <h3 class="ud-chart-title">
                {provider}
                {#if snap?.planType}<span class="ud-plan">{snap.planType}</span>{/if}
              </h3>
              {#if snap?.error}
                <span class="ud-chart-err" title={snap.errorDetail}>{snap.error}</span>
              {:else if snap?.notice}
                <span class="ud-chart-note">{snap.notice}</span>
              {/if}
            </div>

            {#if rows.length}
              {@const times = changeTimes(rows, domain)}
              {@const hoverT = hover[provider] ?? null}
              {@const top = PAD.top}
              {@const height = top + plotH + PAD.bottom}
              <!-- The wrapper is the positioning context for the HTML readout;
                   the SVG itself is scaled by its viewBox, so the readout is
                   placed by percentage of the plot rather than in user units. -->
              <div class="ud-chart-wrap">
              <svg
                class="ud-chart"
                width={W}
                height={height}
                viewBox="0 0 {W} {height}"
                role="img"
                aria-label="{provider} limit usage over the last {hours} hours"
                tabindex="0"
                onpointermove={(e) => (hover[provider] = pointerToTime(e, domain))}
                onpointerdown={(e) => (hover[provider] = pointerToTime(e, domain))}
                onpointerleave={(e) => {
                  // Touch fires pointerleave the instant the finger lifts, so
                  // clearing unconditionally made a tap flash the readout and
                  // erase it. A finger has no hover state to leave: the value
                  // stays until the next tap, a drag, or the panel closes.
                  if (e.pointerType === 'mouse') hover[provider] = null
                }}
                onblur={() => (hover[provider] = null)}
                onkeydown={(e) => {
                  if (e.key === 'ArrowLeft') { e.preventDefault(); moveHover(provider, times, -1) }
                  else if (e.key === 'ArrowRight') { e.preventDefault(); moveHover(provider, times, 1) }
                  else if (e.key === 'Escape') hover[provider] = null
                }}
              >
                {#each GRID as g (g)}
                  <line class="ud-grid" x1={PAD.left} x2={W - PAD.right} y1={yOf(g, top)} y2={yOf(g, top)} />
                  <text class="ud-axis-label" x={PAD.left - 6} y={yOf(g, top) + 3} text-anchor="end">{usageChartYLabel(g)}</text>
                {/each}
                {#each usageChartXTicks(domain) as t (t)}
                  <text class="ud-axis-label" x={xOf(t, domain)} y={height - 6} text-anchor="middle">{usageChartXLabel(t, domain)}</text>
                {/each}

                {#each rows as s (s.key)}
                  <path class="ud-line" d={stepPath(s.points, domain, top)} stroke={s.color} />
                {/each}

                <!-- Direct labels at the live end of each line. This is what
                     keeps identity off colour alone, and it discharges the
                     light-mode contrast warning from the palette validator. -->
                {#each labelLayout(rows, domain, top) as p (p.key)}
                  <circle class="ud-dot" cx={p.x} cy={p.dotY} r="3.5" fill={p.color} />
                  {#if Math.abs(p.y - p.dotY) > 1}
                    <!-- Leader line back to the true value when the label was
                         nudged off it, so the annotation still points at data. -->
                    <line class="ud-leader" x1={p.x} y1={p.dotY} x2={p.x + 5} y2={p.y} stroke={p.color} />
                  {/if}
                  <text class="ud-series-label" x={p.x + 7} y={p.y + 3.5} fill={p.color}>
                    {shortLabel(p.label)} {Math.round(p.percent)}%
                    <title>{p.label} — {Math.round(p.percent)}% used</title>
                  </text>
                {/each}

                {#if hoverT != null}
                  <line class="ud-hairline"
                        x1={xOf(hoverT, domain)} x2={xOf(hoverT, domain)}
                        y1={top} y2={top + plotH} />
                  {#each rows as s (s.key)}
                    {@const v = valueAt(s, hoverT)}
                    {#if v != null}
                      <circle class="ud-hover-dot" cx={xOf(hoverT, domain)} cy={yOf(v, top)} r="3.5" fill={s.color} />
                    {/if}
                  {/each}
                {/if}

                <!-- Transparent capture layer, last so it sits above the marks:
                     the pointer only has to be somewhere in the plot, never on
                     a 2px line. -->
                <rect class="ud-hit" x={PAD.left} y={top}
                      width={xOf(Math.min(now, domain[1]), domain) - PAD.left} height={plotH} />
              </svg>

              {#if hoverT != null}
                <!-- Values lead, series names follow: the reader already knows
                     which lines are on screen and wants the numbers. -->
                <div
                  class="ud-readout"
                  class:flip={xOf(hoverT, domain) / W > 0.62}
                  style="left: {(xOf(hoverT, domain) / W) * 100}%"
                >
                  <div class="ud-readout-time">{crosshairTime(hoverT)}</div>
                  {#each rows as s (s.key)}
                    {@const v = valueAt(s, hoverT)}
                    {#if v != null}
                      <div class="ud-readout-row">
                        <span class="ud-readout-key" style="background: {s.color}"></span>
                        <span class="ud-readout-val">{Math.round(v)}%</span>
                        <span class="ud-readout-name">{s.label}</span>
                      </div>
                    {/if}
                  {/each}
                </div>
              {/if}
              </div>
            {:else if !loading}
              <p class="ud-empty">No stored readings yet for {provider} in this range.</p>
            {/if}
          </section>
          {/each}
          </div>

          <div class="ud-information" data-testid="usage-information">
          <section class="ud-section" data-testid="usage-reset-credits">
          <h3 class="ud-section-title">One-time usage resets</h3>
          <p class="ud-section-note ud-credit-note">
            These expire independently of the weekly or rolling usage windows.
          </p>
          <table class="ud-table">
            <thead>
              <tr><th>Reset</th><th>Expires</th></tr>
            </thead>
            <tbody>
              {#each resetCreditRows() as r (r.id)}
                <tr>
                  <td><span class="ud-prov">{r.provider}</span> {r.title}</td>
                  <td class="ud-when">
                    {#if r.expiresAt}
                      {untilLabel(r.expiresAt)} · {wallClock(r.expiresAt)}
                    {:else}
                      Expiry time not reported
                    {/if}
                  </td>
                </tr>
              {:else}
                <tr><td colspan="2" class="ud-empty-cell">No one-time usage resets available.</td></tr>
              {/each}
            </tbody>
          </table>
          </section>

          <section class="ud-section">
          <h3 class="ud-section-title">Resets</h3>
          <table class="ud-table">
            <thead>
              <tr><th>Limit</th><th>Used</th><th>Resets</th><th>When</th></tr>
            </thead>
            <tbody>
              {#each resetRows() as r (r.id)}
                <tr>
                  <td><span class="ud-prov">{r.provider}</span> {r.label}</td>
                  <td class="ud-num">{Math.round(r.percent)}%</td>
                  <td class="ud-num">{untilLabel(r.at)}</td>
                  <td class="ud-when">{wallClock(r.at)}</td>
                </tr>
              {:else}
                <tr><td colspan="4" class="ud-empty-cell">No reset times reported.</td></tr>
              {/each}
            </tbody>
          </table>
          </section>

          <section class="ud-section">
          <h3 class="ud-section-title">
            Call log
            <span class="ud-section-note">
              {okCount}/{attempts.length} ok{pollSeconds ? ` · polling every ${Math.round(pollSeconds / 60)}m` : ''} · resets when nano restarts
            </span>
          </h3>
          <div class="ud-log">
            {#each attempts as a, i (a.at + a.provider + i)}
              <div class="ud-log-row" class:bad={!a.ok} class:skipped={a.ok && a.note}>
                <span class="ud-log-time">{stamp(a.at)}</span>
                <span class="ud-log-prov">{a.provider}</span>
                <span class="ud-log-result" title={a.detail || ''}>
                  {#if !a.ok}{a.error}
                  {:else if a.note}skipped · {a.note}
                  {:else}ok · {a.buckets} limits{/if}
                </span>
                <span class="ud-log-ms">{a.durationMs}ms</span>
              </div>
            {:else}
              <p class="ud-empty">No calls recorded yet.</p>
            {/each}
          </div>
          </section>
          </div>
        </div>
      </div>
    </div>
  </div>
{/if}
