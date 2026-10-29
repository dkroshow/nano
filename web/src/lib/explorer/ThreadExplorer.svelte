<script>
  import ThreadRowItem from './ThreadRowItem.svelte'
  import {
    explorer, ensureLoaded, loadDataset, setView, retryIndexError,
  } from '../../explorer.svelte.js'
  import { cwdTail, displayModel } from './explorer_format.js'
  import { applyView, collapseWorkerGroups, computeFacets, viewIsNarrowed } from '../../explorer_local.js'
  import { workerDisclosure } from './worker_disclosure.svelte.js'

  // `active` is false while the rail is collapsed: this view stays mounted,
  // but a hidden list must not answer key presses, and it arranges and renders
  // no rows, since every catalog change would redo that work over the whole
  // catalog for nobody.
  let { onClose, onValidationRows = null, active = true } = $props()

  // This component stays mounted while the compact rail is visible. Upgrade a
  // snapshot-seeded sidebar to the complete catalog window only when the
  // operator actually opens the Explorer.
  $effect(() => {
    if (active) ensureLoaded()
  })

  const index = $derived(explorer.index)
  // The visible list is derived locally from the in-memory window dataset:
  // sort, facet selections, and date ranges apply with zero latency. Rendering
  // is capped (the dataset is not) so an "all threads" window can't flood the
  // DOM; the cap grows on demand.
  const RENDER_STEP = 400
  let renderCap = $state(RENDER_STEP)
  const arranged = $derived(active ? applyView(explorer.dataset, explorer.view) : [])
  // Worker groups close here exactly as they do in the rail, over the same
  // shared disclosure state — expanding the rail widens the same rows, so a
  // group left closed must not spring open on the way. A narrowed view keeps
  // matches plus their supervisor chain, so nothing collapses there.
  const visible = $derived(
    viewIsNarrowed(explorer.view)
      ? arranged
      : collapseWorkerGroups(arranged, workerDisclosure.expanded, workerDisclosure.historicalExpanded),
  )
  const rows = $derived(visible.slice(0, renderCap))
  const listEmpty = $derived(visible.length === 0)
  const fetchError = $derived(explorer.listState === 'error')

  $effect(() => onValidationRows?.(rows))

  function onKeydown(e) {
    if (!active) return
    if (openPicker && e.key === 'Escape' && !e.defaultPrevented) {
      openPicker = null
      return
    }
    if (e.key === 'Escape' && !e.defaultPrevented) {
      onClose?.()
    }
  }

  // Column-header popups. Facet pickers (project/provider/model/effort) show a
  // checkbox per distinct value, fetched fresh so counts reflect the other
  // active filters; null selection in the store = all values. Time popups
  // (started/last msg) hold the sort toggle and a date range.
  const FACET_COLUMNS = [
    { field: 'project', filterKey: 'projects', label: 'project', cellClass: 'xc-project', display: cwdTail },
    { field: 'provider', filterKey: 'providers', label: 'provider', cellClass: 'xc-provider', display: (v) => v },
    { field: 'model', filterKey: 'models', label: 'model', cellClass: 'xc-model', display: displayModel },
    { field: 'effort', filterKey: 'efforts', label: 'effort', cellClass: 'xc-effort', display: (v) => v },
  ]
  const TIME_COLUMNS = [
    { field: 'created', rangeKey: 'createdRange', label: 'started', cellClass: 'xc-when xc-start' },
    { field: 'updated', rangeKey: 'updatedRange', label: 'last msg', cellClass: 'xc-when' },
  ]

  // openPicker: null | {kind:'facet', field, filterKey} | {kind:'time', field, rangeKey}
  // Facet values/counts are derived from the in-memory dataset on the fly —
  // no fetch, no cache, no loading state.
  let openPicker = $state(null)
  const openFacets = $derived(
    openPicker?.kind === 'facet' ? computeFacets(explorer.dataset, explorer.view, openPicker.field) : null
  )

  function toggleFacetPicker(col) {
    openPicker = openPicker?.field === col.field ? null : { kind: 'facet', ...col }
  }

  function toggleTimePicker(col) {
    openPicker = openPicker?.field === col.field ? null : { kind: 'time', ...col }
  }

  function onWindowPointerdown(e) {
    if (openPicker && !e.target.closest('.xhead-cell')) openPicker = null
  }

  function facetChecked(filterKey, value) {
    const sel = explorer.view[filterKey]
    return sel == null || sel.values.includes(value)
  }

  function facetUnknownChecked(filterKey) {
    const sel = explorer.view[filterKey]
    return sel == null || sel.unknown
  }

  // Toggling from the all-selected state materializes the full selection minus
  // the toggled entry; re-completing the selection collapses back to null.
  function materializedSelection(filterKey) {
    return (
      explorer.view[filterKey] ?? {
        values: (openFacets?.values ?? []).map((f) => f.value),
        unknown: (openFacets?.unknownCount ?? 0) > 0,
      }
    )
  }

  function toggleFacetValue(filterKey, value) {
    const sel = materializedSelection(filterKey)
    const values = sel.values.includes(value)
      ? sel.values.filter((v) => v !== value)
      : [...sel.values, value]
    applyFacetSelection(filterKey, { ...sel, values })
  }

  function toggleFacetUnknown(filterKey) {
    const sel = materializedSelection(filterKey)
    applyFacetSelection(filterKey, { ...sel, unknown: !sel.unknown })
  }

  function applyFacetSelection(filterKey, sel) {
    const all = (openFacets?.values ?? []).map((f) => f.value)
    const complete =
      sel.values.length === all.length &&
      all.every((v) => sel.values.includes(v)) &&
      (sel.unknown || (openFacets?.unknownCount ?? 0) === 0)
    setView({ [filterKey]: complete ? null : sel })
  }

  // Clicking a column name is a quick sort: first click orders by that column
  // (text columns A→Z, time columns newest-first), a second click flips it.
  function quickSort(field, isTime) {
    if (explorer.view.sort === field) {
      setView({ dir: explorer.view.dir === 'asc' ? 'desc' : 'asc' })
    } else {
      setView({ sort: field, dir: isTime ? 'desc' : 'asc' })
    }
  }

  function rangeOf(rangeKey) {
    return explorer.view[rangeKey] ?? { from: '', to: '' }
  }

  function setRange(rangeKey, patch) {
    const next = { ...rangeOf(rangeKey), ...patch }
    setView({ [rangeKey]: !next.from && !next.to ? null : next })
  }

  function sortIndicator(field) {
    if (explorer.view.sort !== field) return ''
    return explorer.view.dir === 'asc' ? '▲' : '▼'
  }
</script>

<svelte:window onkeydown={onKeydown} onpointerdown={onWindowPointerdown} />

{#snippet headButtons(col, isTime, filtered, togglePicker)}
  <button class="xhead-name" onclick={() => quickSort(col.field, isTime)}>
    {col.label}{#if sortIndicator(col.field)}<span class="xsort-mark">{sortIndicator(col.field)}</span>{/if}
  </button>
  <button
    class="xhead-icon"
    class:filtered
    aria-label="Filter by {col.label}"
    aria-expanded={openPicker?.field === col.field}
    onclick={togglePicker}
  >
    <svg viewBox="0 0 12 12" width="10" height="10" aria-hidden="true"><path d="M1 2h10L7.6 6.4V10L4.4 8.6V6.4L1 2z" fill="currentColor"/></svg>
  </button>
{/snippet}

{#snippet facetHead(col)}
  <span class="{col.cellClass} xhead-cell">
    {@render headButtons(col, false, explorer.view[col.filterKey] != null, () => toggleFacetPicker(col))}
    {#if openPicker?.field === col.field && openFacets}
      <div class="xfacet-pop" role="group" aria-label="Filter by {col.label}">
        <div class="xfacet-actions">
          <button onclick={() => setView({ [col.filterKey]: null })}>all</button>
          <button onclick={() => setView({ [col.filterKey]: { values: [], unknown: false } })}>none</button>
        </div>
        {#each openFacets.values as f (f.value)}
          <label class="xfacet-row" title={f.value}>
            <input type="checkbox" checked={facetChecked(col.filterKey, f.value)} onchange={() => toggleFacetValue(col.filterKey, f.value)} />
            <span class="xfacet-name">{col.display(f.value)}</span>
            <span class="xfacet-count">{f.count}</span>
          </label>
        {/each}
        {#if openFacets.unknownCount > 0}
          <label class="xfacet-row">
            <input type="checkbox" checked={facetUnknownChecked(col.filterKey)} onchange={() => toggleFacetUnknown(col.filterKey)} />
            <span class="xfacet-name xfacet-unknown">(no {col.label})</span>
            <span class="xfacet-count">{openFacets.unknownCount}</span>
          </label>
        {/if}
      </div>
    {/if}
  </span>
{/snippet}

{#snippet timeHead(col)}
  <span class="{col.cellClass} xhead-cell">
    {@render headButtons(col, true, explorer.view[col.rangeKey] != null, () => toggleTimePicker(col))}
    {#if openPicker?.field === col.field}
      <div class="xfacet-pop xtime-pop" role="group" aria-label="Date range for {col.label}">
        <div class="xtime-range">
          <label>from
            <input
              type="date"
              value={rangeOf(col.rangeKey).from}
              onchange={(e) => setRange(col.rangeKey, { from: e.currentTarget.value })}
            />
          </label>
          <label>to
            <input
              type="date"
              value={rangeOf(col.rangeKey).to}
              onchange={(e) => setRange(col.rangeKey, { to: e.currentTarget.value })}
            />
          </label>
          {#if explorer.view[col.rangeKey] != null}
            <button class="xtime-clear" onclick={() => setView({ [col.rangeKey]: null })}>clear</button>
          {/if}
        </div>
      </div>
    {/if}
  </span>
{/snippet}

<div class="session">
  {#if explorer.indexError}
    <!-- role=alert: a failed poll/refresh announces assertively; the
         role=status chip above stays a separate polite region. -->
    <div class="xindex-error" role="alert">
      <span class="ifile-error">
        {explorer.indexError.op === 'refresh' ? 'Refresh request failed' : 'Catalog status error'}: {explorer.indexError.message}
      </span>
      <button class="load-older xrefresh" onclick={retryIndexError}>Retry</button>
    </div>
  {/if}

  {#if index?.namesError}
    <!-- The thread-names truth file is malformed/unreadable. Names are served
         from the last good copy; the file is never rewritten. -->
    <div class="xindex-error" role="alert">
      <span class="ifile-error">Thread names file error: {index.namesError}</span>
    </div>
  {/if}

  <div class="xlist">
    {#if fetchError && listEmpty}
      <div class="xstate">
        <div class="ifile-error">{explorer.listError}</div>
        <button class="load-older" onclick={loadDataset}>Retry</button>
      </div>
    {:else if explorer.listState === 'loading' && explorer.dataset.length === 0}
      <div class="xstate muted">Loading catalog…</div>
    {:else}
      <div class="xlist-head xgrid">
        <span class="xc-dot" aria-hidden="true"></span>
        <span>name</span>
        {#each FACET_COLUMNS as col (col.field)}
          {@render facetHead(col)}
        {/each}
        {@render timeHead(TIME_COLUMNS[0])}
        {@render timeHead(TIME_COLUMNS[1])}
        <span class="xc-nativeid">native id</span>
        <span class="xc-branch">branch</span>
        <span class="xc-health">health</span>
      </div>
      {#each rows as item (item.id)}
        <ThreadRowItem {item} />
      {/each}
      {#if listEmpty}
        <!-- The header (and any open column picker) stays mounted on an empty
             result so the filter that emptied the list can be undone. -->
        <div class="xstate muted">
          {explorer.fetchParams.threadId
            ? 'No thread matches that exact ID and these filters.'
            : 'No threads match these filters in this window.'}
        </div>
      {/if}
      {#if fetchError}<div class="ifile-error">{explorer.listError}</div>{/if}
      {#if visible.length > renderCap}
        <button class="load-older" onclick={() => (renderCap += RENDER_STEP)}>
          Show more — {visible.length - renderCap} rows not rendered
        </button>
      {/if}
    {/if}
  </div>
</div>
