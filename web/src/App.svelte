<script>
  import { tick } from 'svelte'
  import { browserDiagnosticIdentity, recordBrowserDiagnostic, startBrowserDiagnostics } from './browser_diagnostics.js'
  import { recordSidebarLayout } from './sidebar_layout_diagnostics.js'
  import {
    app,
    closeSidebar,
    closeThreadExplorer,
    hiddenThreadRefs,
    recoverThreadReference,
    startPolling,
    startThreadReferenceNavigation,
    toggleDensity,
    toggleSidebar,
    toggleThreadExplorer,
  } from './app.svelte.js'
  import { viewport } from './lib/viewport.svelte.js'
  import {
    addStreamOpenListener,
    bootstrapSidebar,
    explorer,
    requestFullHistory,
    setView,
    startPolling as startCatalogStream,
  } from './explorer.svelte.js'
  import { createBuildReload } from './build_reload.svelte.js'
  import { connection, startConnectionChecks } from './connection.svelte.js'
  import ConnectionStatus from './lib/ConnectionStatus.svelte'
  import NewVersionNote from './lib/NewVersionNote.svelte'
  import { startWarmTranscripts, warmSetChanged } from './lib/warm_transcripts.js'
  import { applyView, collapseWorkerGroups, SORT_MODES, viewIsNarrowed } from './explorer_local.js'
  import {
    addPane,
    clearTabs,
    isActiveCatalogThread,
    isActiveSession,
    panes,
    restoreActiveThreadView,
    saveActiveThreadView,
  } from './panes.svelte.js'
  import {
    filterSidebar,
    orderSidebarRows,
    projectSidebar,
    sectionSidebar,
    sidebarCounts,
    suppressBackgroundRows,
  } from './lib/sidebar_projection.js'
  import {
    allWorkerGroupsExpanded,
    setAllWorkerGroups,
    workerDisclosure,
    workerGroupKeys,
  } from './lib/worker_disclosure.svelte.js'
  import { handleThreadShortcut } from './lib/keyboard.js'
  import {
    actualSidebarRows,
    compareSidebarRows,
    expectedCompactSidebarRows,
    expectedExplorerSidebarRows,
  } from './lib/sidebar_validator.js'
  import CompactThreadRow from './lib/CompactThreadRow.svelte'
  import PanesContainer from './lib/PanesContainer.svelte'
  import NewSessionModal from './lib/NewSessionModal.svelte'
  import SettingsMenu from './lib/SettingsMenu.svelte'
  import NetworkMeter from './lib/NetworkMeter.svelte'
  import { networkMeterEnabled } from './lib/network_meter.js'
  import UsageLimits from './lib/UsageLimits.svelte'
  import UsageDetail from './lib/UsageDetail.svelte'
  import GlobalMail from './lib/GlobalMail.svelte'
  import ThreadExplorer from './lib/ThreadExplorer.svelte'
  import ExplorerStatus from './lib/ExplorerStatus.svelte'
  import ExplorerFilters from './lib/ExplorerFilters.svelte'
  import ThreadQuickDetail from './lib/ThreadQuickDetail.svelte'
  import DescendantCloseDialog from './lib/DescendantCloseDialog.svelte'
  import BulkCloseDialog from './lib/BulkCloseDialog.svelte'
  import {
    MIN_SIDEBAR_WIDTH,
    clampSidebarWidth,
    loadSidebarWidth,
    saveSidebarWidth,
    sidebarWidthLimit,
  } from './lib/sidebar_width.js'

  let newWindowOpen = $state(false)
  let sidebarWidth = $state(loadSidebarWidth())
  let sidebarResizing = $state(false)
  let sidebarResizeStartX = 0
  let sidebarResizeStartWidth = 0
  let sidebarElement = $state(null)
  let explorerValidationRows = $state([])
  let sessionOverlayReady = $state(false)
  let sidebarValidationScheduled = false
  let sidebarValidationSequence = 0
  let lastSidebarValidationSignature = ''
  let lastSidebarModelDiagnostic = ''

  function clearTabsSafely() {
    const dirtyFiles = Object.values(panes.tabs).filter((tab) => tab.kind === 'file' && tab.fileDirty)
    if (dirtyFiles.length && !window.confirm('Clear tabs and discard unsaved file changes?')) return
    clearTabs()
  }

  startBrowserDiagnostics()
  // The header's connection indicator, and the wake-up for sends waiting to
  // be retried when Nano answers again.
  startConnectionChecks()
  // Restore before either server fetch can choose a default thread. The first
  // session refresh then rebinds a stale session id by native thread identity.
  restoreActiveThreadView()
  // Start URL resolution before the normal registry poll. The resolver marks
  // the app pending synchronously, so refreshSessions cannot stage newest.
  startThreadReferenceNavigation()
  const initialSessions = startPolling()
  // The warm set is what this page shows: its hidden thread tabs first, which
  // the first session list names by hosted thread, then the sidebar rows it
  // renders, in order. A change in what it renders is reported below.
  void initialSessions.then(() => startWarmTranscripts(() => [
    ...hiddenThreadRefs(),
    ...sidebarRows.map((row) => row.hostedThreadId).filter(Boolean),
  ]))
  // A deploy can serve a new bundle: this tab reloads onto it by itself only
  // when it shows no thread and has no unsaved edit, and otherwise shows a note.
  const buildReload = createBuildReload({ reload: () => location.reload() })
  buildReload.start()
  addStreamOpenListener(() => void buildReload.opened())
  // The thread catalog is app-wide state (the sidebar projects it, the Explorer
  // view renders it whole), so its window fetch and SSE stream live for the
  // app's lifetime rather than the Explorer view's.
  void bootstrapSidebar(initialSessions).finally(startCatalogStream)

  // Persist navigation as it happens, with V4's visibility/unload hooks below
  // as a final synchronous save during reload and mobile tab suspension.
  $effect(() => {
    saveActiveThreadView()
  })

  // The sidebar is the compact face of the Explorer's dataset — the whole
  // catalog, with `/api/sessions` as a thin pending overlay — sliced by a
  // liveness chip: everything, active work, or what Nano is holding.
  // The chip is view-local UI state; nothing about it is worth persisting.
  const CHIPS = [
    { id: 'all', label: 'All' },
    { id: 'working', label: 'Working' },
    { id: 'nano', label: 'Nano' },
  ]
  const EMPTY_TEXT = {
    all: 'No threads.',
    working: 'Nothing working.',
    nano: 'Nothing loaded in Nano.',
  }
  // Rendering is capped (the projection is not) so a catalog of hundreds can't
  // flood the sidebar's DOM; the cap grows on demand, as in the Explorer list.
  const RENDER_STEP = 60

  let filter = $state('nano')
  let renderCap = $state(RENDER_STEP)
  let backgroundOpen = $state(false)

  // The rail renders the shared view's ordering, so a sort set here or in the
  // expanded table is already in force when the other surface appears.
  const orderedDataset = $derived(applyView(explorer.dataset, explorer.view))

  // The sort button cycles the modes. A column-header sort is not a mode, so
  // the button names the column it is following and the next press returns to
  // the first mode ((-1 + 1) % n === 0).
  const sortModeIndex = $derived(SORT_MODES.findIndex((m) => m.id === explorer.view.sort))
  const sortLabel = $derived(SORT_MODES[sortModeIndex]?.label ?? explorer.view.sort)
  const nextSortMode = $derived(SORT_MODES[(sortModeIndex + 1) % SORT_MODES.length])
  // The session registry is a post-boot gap filler, not a second cold-boot
  // sidebar. Before the active snapshot is ready its rows lack canonical
  // hierarchy, stars, and sort facts, so projecting them would create a
  // contradictory pending-only layout before the real tree arrives.
  // Let the canonical dataset finish its derived hierarchy/order pass before
  // the registry-only gap filler can add rows. Without this settled-render
  // boundary, the ready flag can invalidate first and briefly project every
  // live session as a flat pending row while orderedDataset is still empty.
  $effect(() => {
    if (explorer.sidebarBootState !== 'ready') {
      sessionOverlayReady = false
      return
    }
    let current = true
    void tick().then(() => {
      if (current && explorer.sidebarBootState === 'ready') sessionOverlayReady = true
    })
    return () => { current = false }
  })
  const projectionSessions = $derived(sessionOverlayReady ? app.sessions : [])
  const projectedRows = $derived(projectSidebar(orderedDataset, projectionSessions))
  // Liveness modes keep Nano as the first host band in the compact rail.
  // Explicit Explorer column sorts remain plain and identical in both views.
  const allRows = $derived(
    explorer.view.sort.startsWith('liveness-') ? orderSidebarRows(projectedRows) : projectedRows,
  )
  const counts = $derived((() => {
    const next = sidebarCounts(allRows)
    if (explorer.datasetScope !== 'window') next.all = allRows.length + (explorer.historicalCount ?? 0)
    return next
  })())
  const visibleRows = $derived(filterSidebar(allRows, filter))
  const backgroundView = $derived(suppressBackgroundRows(visibleRows, backgroundOpen))
  // All and Nano are ordinary browsing views, so parent counts collapse their
  // complete child trees. Working and Explorer facets keep every
  // match open because closing a group would hide what the filter found.
  const collapsesAgentGroups = $derived(
    (filter === 'all' || filter === 'nano') && !viewIsNarrowed(explorer.view),
  )
  const sectionedRows = $derived(sectionSidebar(backgroundView.rows, explorer.view.sort.startsWith('liveness-')))
  const compactStarredRows = $derived(
    collapsesAgentGroups
      ? collapseWorkerGroups(sectionedRows.starred, workerDisclosure.expanded, workerDisclosure.historicalExpanded)
      : sectionedRows.starred,
  )
  const compactRestRows = $derived(
    collapsesAgentGroups
      ? collapseWorkerGroups(sectionedRows.rest, workerDisclosure.expanded, workerDisclosure.historicalExpanded)
      : sectionedRows.rest,
  )
  const compactRows = $derived([...compactStarredRows, ...compactRestRows])
  const sidebarRows = $derived(compactRows.slice(0, renderCap))
  $effect(() => {
    void sidebarRows
    warmSetChanged()
  })
  const visibleStarredCount = $derived(Math.min(compactStarredRows.length, sidebarRows.length))
  const starredRows = $derived(sidebarRows.slice(0, visibleStarredCount))
  const restRows = $derived(sidebarRows.slice(visibleStarredCount))
  const hasAgentGroups = $derived(workerGroupKeys(allRows).length > 0)
  const allAgentsExpanded = $derived(allWorkerGroupsExpanded(allRows, workerDisclosure.expanded))
  const globalAgentToggleEnabled = $derived(
    hasAgentGroups && (app.explorerOpen ? !viewIsNarrowed(explorer.view) : collapsesAgentGroups),
  )

  // Content-free cold-boot evidence. Browser validations retain the ordered
  // row identities; this companion event makes the data source and pending
  // population explicit after the refresh that caused it.
  $effect(() => {
    const pendingCount = sidebarRows.filter((row) => row.kind === 'pending').length
    const signature = JSON.stringify([
      explorer.sidebarBootState,
      explorer.datasetSource,
      sidebarRows.length,
      pendingCount,
    ])
    if (signature === lastSidebarModelDiagnostic) return
    lastSidebarModelDiagnostic = signature
    recordBrowserDiagnostic('sidebar.model', {
      status: explorer.sidebarBootState,
      op: explorer.datasetSource || 'none',
      itemCount: sidebarRows.length,
      pendingCount,
    })
  })

  function compactRowActive(row) {
    return (row.click?.sessionId && isActiveSession(row.click.sessionId)) ||
      (row.click?.catalogId && isActiveCatalogThread(row.click.catalogId))
  }

  function explorerRowActive(row) {
    return isActiveCatalogThread(row.id)
  }

  function expectedSidebarValidationRows() {
    return app.explorerOpen
      ? expectedExplorerSidebarRows(explorerValidationRows, explorerRowActive)
      : expectedCompactSidebarRows(sidebarRows, compactRowActive)
  }

  function publishSidebarValidation(force = false) {
    if (!sidebarElement) return
    const expected = expectedSidebarValidationRows()
    const actualRoot = sidebarElement.querySelector(app.explorerOpen ? '#thread-explorer' : '.side-compact')
    const report = compareSidebarRows(expected, actualSidebarRows(actualRoot))
    if (!app.explorerOpen) recordSidebarLayout(actualRoot, sidebarRows)
    const signature = JSON.stringify({ expected: report.expected, actual: report.actual })
    if (!force && signature === lastSidebarValidationSignature) return
    lastSidebarValidationSignature = signature
    const browser = browserDiagnosticIdentity()
    const snapshotId = `${browser.tabId}:sidebar:${++sidebarValidationSequence}`
    void fetch('/api/v1/browser-validations', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({
        snapshotId,
        checkedAt: new Date().toISOString(),
        ...browser,
        paneId: 'left-sidebar',
        surface: 'sidebar',
        viewMode: app.explorerOpen ? 'explorer' : `compact-${filter}`,
        pageVisible: document.visibilityState === 'visible',
        surfaceVisible: !viewport.mobile || app.sidebarOpen,
        status: report.ok ? 'match' : 'mismatch',
        expectedRows: report.expected,
        mountedRows: report.actual,
      }),
    }).catch(() => {})
  }

  async function scheduleSidebarValidation() {
    if (sidebarValidationScheduled) return
    sidebarValidationScheduled = true
    await tick()
    queueMicrotask(() => {
      sidebarValidationScheduled = false
      publishSidebarValidation()
    })
  }

  function observeSidebarValidation(node) {
    sidebarElement = node
    const observer = new MutationObserver(() => void scheduleSidebarValidation())
    observer.observe(node, { childList: true, subtree: true, characterData: true, attributes: true })
    const scroller = node.querySelector('.thread-list')
    const onScroll = () => void scheduleSidebarValidation()
    scroller?.addEventListener('scroll', onScroll, { passive: true })
    const resizeObserver = typeof ResizeObserver === 'function'
      ? new ResizeObserver(() => void scheduleSidebarValidation())
      : null
    if (scroller) resizeObserver?.observe(scroller)
    void scheduleSidebarValidation()
    return {
      destroy() {
        observer.disconnect()
        resizeObserver?.disconnect()
        scroller?.removeEventListener('scroll', onScroll)
        if (sidebarElement === node) sidebarElement = null
      },
    }
  }

  $effect(() => {
    void sidebarRows
    void explorerValidationRows
    void app.explorerOpen
    void app.activeId
    void panes.activePaneId
    void app.sidebarOpen
    void scheduleSidebarValidation()
  })

  $effect(() => {
    const heartbeat = setInterval(() => publishSidebarValidation(true), 15_000)
    const visibility = () => publishSidebarValidation(true)
    document.addEventListener('visibilitychange', visibility)
    return () => {
      clearInterval(heartbeat)
      document.removeEventListener('visibilitychange', visibility)
    }
  })

  // Keyboard thread navigation is an intent to continue typing in the target
  // thread. Wait for Svelte to reveal its retained pane, then focus only that
  // pane's composer; historical/detail views have no composer and stay put.
  function focusActiveComposer() {
    void tick().then(() => {
      const activePane = document.querySelector('.thread-pane.focused')
      const composer = activePane?.querySelector('.thread-pane-view:not(.hidden) .composer textarea')
      composer?.focus()
    })
  }

  function onThreadShortcut(event) {
    const landedKey = handleThreadShortcut(event, sidebarRows)
    if (!landedKey) return
    focusActiveComposer()
    revealSidebarRow(landedKey)
  }

  // The recency walk ignores sidebar position, so a keyboard landing is
  // routinely scrolled out of the rail's viewport. Reveal it with the minimal
  // scroll — instant, not smooth, so rapid ⌥Tab presses never queue
  // animations. Mouse clicks never come through here; only shortcuts scroll.
  function revealSidebarRow(key) {
    if (typeof key !== 'string') return
    const row = document.querySelector(`.side-compact [data-sidebar-id="${CSS.escape(key)}"]`)
    row?.scrollIntoView?.({ block: 'nearest' })
  }

  function startSidebarResize(event) {
    if (viewport.mobile || app.explorerOpen) return
    event.preventDefault()
    sidebarResizeStartX = event.clientX
    sidebarResizeStartWidth = sidebarWidth
    sidebarResizing = true
  }

  function moveSidebarResize(event) {
    if (!sidebarResizing) return
    sidebarWidth = clampSidebarWidth(
      sidebarResizeStartWidth + event.clientX - sidebarResizeStartX,
      window.innerWidth,
    )
  }

  function finishSidebarResize() {
    if (!sidebarResizing) return
    sidebarResizing = false
    saveSidebarWidth(sidebarWidth)
  }

  function constrainSidebarToViewport() {
    if (viewport.mobile || sidebarResizing) return
    const constrained = clampSidebarWidth(sidebarWidth, window.innerWidth)
    if (constrained === sidebarWidth) return
    sidebarWidth = constrained
    saveSidebarWidth(sidebarWidth)
  }

  function resizeSidebarFromKeyboard(event) {
    let next = null
    if (event.key === 'ArrowLeft') next = sidebarWidth - 16
    if (event.key === 'ArrowRight') next = sidebarWidth + 16
    if (event.key === 'Home') next = MIN_SIDEBAR_WIDTH
    if (event.key === 'End') next = sidebarWidthLimit(window.innerWidth)
    if (next == null) return
    event.preventDefault()
    sidebarWidth = clampSidebarWidth(next, window.innerWidth)
    saveSidebarWidth(sidebarWidth)
  }
</script>

<svelte:window
  onkeydown={onThreadShortcut}
  onpointermove={moveSidebarResize}
  onpointerup={finishSidebarResize}
  onpointercancel={finishSidebarResize}
  onresize={constrainSidebarToViewport}
  onbeforeunload={saveActiveThreadView}
/>
<svelte:document onvisibilitychange={saveActiveThreadView} />

<SettingsMenu />

<!-- Rendered here, not beside its trigger in the rail: the mobile sidebar is
     transformed, and a transformed ancestor becomes the containing block for
     position:fixed descendants — which pinned this full-screen overlay inside
     the drawer and put the drawer's own stacking context above its header. -->
<UsageDetail open={app.usageDetailOpen} onClose={() => (app.usageDetailOpen = false)} />

{#if viewport.mobile}
  <!-- The drawer's one affordance, mirroring the fixed settings gear at the
       opposite corner. It exists only on the phone layout: above 700px the
       rail is a column of the shell and there is nothing to open. -->
  <button
    class="nav-toggle"
    aria-label={app.sidebarOpen ? 'Close the thread sidebar' : 'Open the thread sidebar'}
    aria-expanded={app.sidebarOpen}
    aria-controls="thread-sidebar"
    onclick={toggleSidebar}
  >☰</button>
{/if}

<div
  class="shell density-{app.density}"
  class:drawer-open={app.sidebarOpen}
  class:sidebar-resizing={sidebarResizing}
  style="--rail-w: {sidebarWidth}px"
>
  {#if app.threadReferenceError}
    <div class="thread-reference-error" role="alert">
      <span>{app.threadReferenceError}</span>
      <button onclick={recoverThreadReference}>Open Thread Explorer</button>
      <button onclick={() => (app.threadReferenceError = '')}>Dismiss</button>
    </div>
  {/if}
  {#if viewport.mobile && app.sidebarOpen}
    <div class="sidebar-scrim" role="presentation" onclick={closeSidebar}></div>
  {/if}
  <aside id="thread-sidebar" class="sidebar" class:expanded={app.explorerOpen} use:observeSidebarValidation>
    <!-- The density toggle lives in the brand bar because that is the one row
         present in both the compact rail and the expanded table. -->
    <div class="side-brand">
      <span class="side-brand-name">nano</span>
      {#if app.explorerOpen}<ExplorerStatus />{/if}
      <button
        class="density-toggle"
        data-testid="toggle-density"
        aria-pressed={app.density === 'compact'}
        title={app.density === 'compact' ? 'Show more per row' : 'Fit more rows'}
        aria-label={app.density === 'compact' ? 'Switch to full rows' : 'Switch to compact rows'}
        onclick={toggleDensity}
      >
        {app.density === 'compact' ? 'compact' : 'full'}
      </button>
    </div>

    <!-- Account headroom for both providers, directly under the brand row. It
         is deliberately above the Explorer header: the bands below shrink,
         scroll or swap with the rail's state, and this one must not. -->
    <UsageLimits />

    <!-- Band 2 of three, in both states. The header doubles as the expand
         affordance: the sidebar shows N of the catalog's M threads (N follows
         the active chip), and the count reads '…' until the first fetch lands.
         Expanded, the Explorer's filters fill the width the expansion just
         created — no new band is ever inserted above the threads. -->
    <div class="side-head">
      <button
        class="side-expand"
        class:on={app.explorerOpen}
        data-testid="open-thread-explorer"
        title="Open the full thread catalog"
        aria-label="Thread Explorer — {visibleRows.length} of {counts.all} threads in the window"
        aria-expanded={app.explorerOpen}
        aria-controls="thread-explorer"
        onclick={toggleThreadExplorer}
      >
        <span class="side-expand-label">Thread Explorer</span>
        <span class="side-expand-count">{visibleRows.length} / {counts.all || '…'}</span>
        <span class="side-expand-glyph" aria-hidden="true">{app.explorerOpen ? '«' : '»'}</span>
      </button>
      {#if app.explorerOpen}<ExplorerFilters />{/if}
    </div>

    <!-- Sort, shared by both surfaces. One button cycling the modes: it shows
         what the list is sorted by now and switches on click. It writes the
         same view.sort the column headers write, so the ordering survives
         expanding and collapsing in either direction — and after a column sort
         it names that column until pressed. -->
    <div class="side-sort">
      <button
        class="sort-btn"
        data-testid="sort-toggle"
        title="Sorted by {sortLabel} — click to sort by {nextSortMode.label}"
        aria-label="Sorted by {sortLabel}. Change to {nextSortMode.label}"
        onclick={() => setView({ sort: nextSortMode.id, dir: 'desc' })}
      >
        <span class="sort-btn-glyph" aria-hidden="true">⇅</span>
        {sortLabel}
      </button>
      <button
        class="sort-btn agent-groups-btn"
        class:on={allAgentsExpanded}
        type="button"
        aria-pressed={allAgentsExpanded}
        aria-label={allAgentsExpanded ? 'Collapse all agents' : 'Expand all agents'}
        title={globalAgentToggleEnabled
          ? (allAgentsExpanded ? 'Collapse all agent groups' : 'Expand all agent groups')
          : 'Agent groups cannot be collapsed in this filtered view'}
        disabled={!globalAgentToggleEnabled}
        onclick={() => setAllWorkerGroups(!allAgentsExpanded)}
      >
        <span class="sort-btn-glyph" aria-hidden="true">{allAgentsExpanded ? '▾' : '▸'}</span>
        Agents
      </button>
      <GlobalMail />
    </div>

    <!-- Both bodies stay mounted and are swapped with CSS. Building the
         Explorer's rows on click cost ~370ms of dead time before the panel
         could even start opening; paying it once at startup makes the toggle
         immediate in both directions. Only one is ever visible — the same
         threads twice would be the pop-out all over again. -->
    <div id="thread-explorer" class="side-body" class:hidden={!app.explorerOpen}>
      <ThreadExplorer onClose={closeThreadExplorer} onValidationRows={(rows) => (explorerValidationRows = rows)} active={app.explorerOpen} />
    </div>

    <div class="side-compact" class:hidden={app.explorerOpen}>
      <!-- Liveness slice: "what's loaded in nano" / "what's alive in tmux" are
           the two questions the whole-catalog sidebar has to answer at a glance. -->
      <div class="side-chips" role="group" aria-label="Filter threads by liveness">
        {#each CHIPS as chip (chip.id)}
          <button
            class="side-chip"
            class:on={filter === chip.id}
            aria-pressed={filter === chip.id}
            disabled={counts[chip.id] === 0 && filter !== chip.id}
            onclick={() => {
              filter = chip.id
              if (chip.id === 'all') requestFullHistory()
            }}
          >
            {chip.label} <span class="side-chip-n">{counts[chip.id]}</span>
          </button>
        {/each}
      </div>

      {#if backgroundView.backgroundCount > 0}
        <button
          type="button"
          class="background-disclosure"
          data-testid="background-disclosure"
          aria-expanded={backgroundOpen}
          onclick={() => (backgroundOpen = !backgroundOpen)}
        >
          <span aria-hidden="true">{backgroundOpen ? '▾' : '▸'}</span>
          Background agents
          <span class="background-count">{backgroundView.backgroundCount}</span>
        </button>
      {/if}

      <div class="thread-list">
        <!-- The whole catalog: nano's attached sessions (memory rows included)
             and detached threads first, then the rest by recency — panes and
             history alike, the dot and the chips telling them apart. Starred
             rows occupy their own section above that order. -->
        {#if explorer.sidebarBootState === 'loading'}
          <div class="side-empty" role="status">Loading threads…</div>
        {:else if explorer.sidebarBootState === 'error'}
          <div class="side-empty" role="alert">{explorer.listError || 'Active threads could not be loaded.'}</div>
        {:else if starredRows.length || restRows.length}
          {#if starredRows.length}
            <div class="sidebar-section-label" data-testid="starred-section">Starred</div>
            {#each starredRows as row (row.key)}
              <CompactThreadRow {row} />
            {/each}
            {#if restRows.length}
              <div class="sidebar-section-divider" role="separator" data-testid="starred-divider"></div>
            {/if}
          {/if}
          {#each restRows as row (row.key)}
            <CompactThreadRow {row} />
          {/each}
        {:else}
          <div class="side-empty">
            {backgroundView.hiddenCount > 0 ? 'Background agents are folded.' : EMPTY_TEXT[filter]}
          </div>
        {/if}
        {#if compactRows.length > renderCap}
          <button class="load-older" onclick={() => (renderCap += RENDER_STEP)}>
            Show {compactRows.length - renderCap} more
          </button>
        {/if}
      </div>
    </div>

    <div class="side-spawn">
      <button class="new-win-btn" onclick={() => (newWindowOpen = true)}>+ New Window</button>
    </div>

    {#if !viewport.mobile && !app.explorerOpen}
      <button
        type="button"
        class="sidebar-resize-handle"
        aria-label="Resize thread sidebar ({sidebarWidth} pixels)"
        title="Drag to resize the thread sidebar"
        onpointerdown={startSidebarResize}
        onkeydown={resizeSidebarFromKeyboard}
      ></button>
    {/if}
  </aside>

  {#if newWindowOpen}
    <NewSessionModal onClose={() => (newWindowOpen = false)} />
  {/if}
  <ThreadQuickDetail />
  <DescendantCloseDialog />
  <BulkCloseDialog />
  {#if app.explorerOpen}
    <div class="side-rail-spacer" aria-hidden="true"></div>
  {/if}

  <!-- The page's notices, stacked so none hides another, above the drawer
       and the Explorer. A lost connection is the page's state, not a row's:
       rows keep what they last showed until reads after the reconnect
       replace it. -->
  <div class="page-notices">
    <NewVersionNote {buildReload} />
    {#if !explorer.link.up || connection.status === 'offline'}
      <div class="reconnecting-banner" role="status">Reconnecting to Nano…</div>
    {/if}
  </div>
  <!-- The workspace keeps rendering behind the expanded rail — the Explorer is
       a layer over the active thread, not a replacement for it. -->
  <main class="main">
    <div class="pane-toolbar">
      <button class="pane-add" onclick={addPane} disabled={panes.items.length >= 12}>+ Pane</button>
      <button onclick={clearTabsSafely} disabled={Object.keys(panes.tabs).length === 0}>Clear tabs</button>
      <ConnectionStatus />
      {#if networkMeterEnabled()}<NetworkMeter />{/if}
    </div>
    <PanesContainer />
  </main>
</div>
