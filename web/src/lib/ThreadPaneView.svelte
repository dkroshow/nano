<script>
  import { onDestroy, untrack } from 'svelte'
  import { observeThreadOpen } from '../thread_open.js'
  import { recordBrowserDiagnostic } from '../browser_diagnostics.js'
  import { app } from '../app.svelte.js'
  import { explorer } from '../explorer.svelte.js'
  import { getSessionStore } from '../state.svelte.js'
  import {
    createDurableHistory,
    registerTabHistory,
    unregisterTabHistory,
  } from '../transcript_store.svelte.js'
  import Conversation from './Conversation.svelte'
  import HistoricalThreadView from './HistoricalThreadView.svelte'
  import { historicalBarFields } from './session_bar.js'
  import { cachedThreadDetail, prepareThreadDetail } from './thread_detail_cache.js'
  import {
    resumeGate,
    resolveThreadIdentity,
    createResumeAction,
    createDetachAction,
    detachLabels,
    prioritizeStartingThread,
  } from './thread_resume.svelte.js'

  let { tab, paneId = null, visible = true, onOpenDetail = null } = $props()
  let paneElement
  let measuredNavigation = null
  const resolvingRestoredTab = $derived(!tab.openTrigger && app.threadReferenceResolving)
  $effect(() => {
    void tab.tailResetNonce
    if (!visible || !paneElement || resolvingRestoredTab) return
    return untrack(() => {
      const navigation = tab.tailResetNonce ?? 0
      const repeated = measuredNavigation === navigation
      measuredNavigation = navigation
      return observeThreadOpen(paneElement, {
        paneId: String(tab.id), provider: tab.provider, threadId: tab.nativeId || tab.catalogId,
        catalogId: tab.catalogId, sessionId: tab.sessionId,
      }, {
        startedAt: repeated ? performance.now() : tab.openStartedAt ?? 0,
        trigger: repeated ? 'tab' : tab.openTrigger ?? 'load',
        preserveHistory: !tab.forceTailResetNonce && manualScrollAwayAt > 0,
      })
    })
  })

  let manualScrollAwayAt = $state(0)
  const rememberManualScrollAwayAt = (value) => {
    manualScrollAwayAt = Number(value || 0)
  }
  const boundInfo = $derived(app.sessions.find((session) => session.id === tab.sessionId) ?? null)

  // Pane liveness rides a reconnect grace, because losing the session id is
  // not the same as losing the thread: a nano deploy restart replaces every
  // session id, and the poll that notices the old id died can land seconds
  // before the one carrying the re-adopted successor (adoptSessionTabs then
  // rebinds the tab in place). Flipping to read-only in that window unmounted
  // the composer mid-thought and ate in-flight prompts — so the pane keeps the
  // live view mounted under a quiet "reconnecting" hint and goes historical
  // only once the grace expires. A deliberate detach still flips instantly.
  const RECONNECT_GRACE_MS = 5000
  let lastInfo = $state(null)
  let reconnecting = $state(false)
  let graceTimer = null
  const info = $derived(boundInfo ?? (reconnecting ? lastInfo : null))

  // Hidden tabs keep status and delivery state for instant return. Each
  // visible Conversation opens its own file stream.
  const liveSession = $derived(
    info ? untrack(() => getSessionStore(info.id, info)) : null,
  )
  // One content-free render checkpoint ties what the operator can see to the
  // exact store cursor driving it. Revision progress is throttled; identity,
  // visibility and connection changes are always recorded.
  let lastPaneDiagnostic = ''
  let lastPaneRevision = -1
  let lastPaneProgressAt = 0
  $effect(() => {
    const session = liveSession
    const state = session?.store
    // A file-only pane's status comes from its own stream, not the message
    // channel; report that stream instead.
    const revision = Number(state?.statusRevision ?? 0)
    const connected = Boolean(state?.streamConnected)
    const signature = [
      visible, tab.sessionId, session?.id, state?.threadId, connected,
      state?.turnActive, state?.statusRevision,
    ].join('|')
    const now = Date.now()
    const cursorProgress = revision !== lastPaneRevision && now - lastPaneProgressAt >= 1000
    if (signature !== lastPaneDiagnostic || cursorProgress) {
      lastPaneDiagnostic = signature
      lastPaneRevision = revision
      lastPaneProgressAt = now
      recordBrowserDiagnostic('pane.render', {
        paneId: tab.id || tab.catalogId,
        provider: state?.provider || info?.provider || tab.provider,
        threadId: state?.threadId || info?.threadId || tab.nativeId,
        sessionId: session?.id || tab.sessionId,
        hostedThreadId: state?.hostedThreadId || info?.hostedThreadId,
        epoch: state?.statusEpoch,
        revision,
        connected,
        turnActive: Boolean(state?.turnActive),
        visible: Boolean(visible),
        status: info?.status,
      })
    }
  })

  // Set by this pane's own Detach/End action: that loss of attachment is
  // deliberate, so the read-only flip must not wait out the grace window.
  let detachRequested = false

  // $effect.pre, same reason as the history channel below: the grace flag must
  // be set before the DOM updates for the frame in which boundInfo went null,
  // or the composer unmounts for exactly the frame this exists to prevent.
  $effect.pre(() => {
    if (boundInfo) {
      clearTimeout(graceTimer)
      graceTimer = null
      lastInfo = boundInfo
      reconnecting = false
      detachRequested = false
      return
    }
    if (tab.detached || detachRequested || !untrack(() => lastInfo)) settleHistorical()
    else if (!untrack(() => reconnecting)) {
      reconnecting = true
      graceTimer = setTimeout(settleHistorical, RECONNECT_GRACE_MS)
    }
  })

  // settleHistorical is the real live→read-only transition. The turns that
  // just ran are in the durable projection by now, so refresh the tail once
  // (ETag-conditional) — otherwise the read-only view would show a pre-resume
  // snapshot. Idempotent: the grace timer and the effect can both land here.
  function settleHistorical() {
    clearTimeout(graceTimer)
    graceTimer = null
    reconnecting = false
    if (!lastInfo) return
    lastInfo = null
    history?.rearm()
    void history?.revalidateTail()
  }

  onDestroy(() => clearTimeout(graceTimer))

  // Which thread this pane is showing. The tab's own native id wins: a claude
  // `--resume` successor announces a new one and the tab adopts it, while
  // tab.catalogId keeps naming the predecessor's row until the successor is
  // indexed. Everything below — the gate, the request, the transcript — reads
  // from this one answer, so they cannot describe two different threads.
  const identity = $derived(
    resolveThreadIdentity({
      nativeId: tab.nativeId,
      catalogId: tab.catalogId,
      provider: tab.provider,
      dataset: explorer.dataset,
    }),
  )

  // The tab owns one durable-history channel for its thread, the one the
  // read-only view renders. It stays registered while the pane is live, so a
  // catalog change to the thread keeps it current and a detach repaints from
  // data already in memory instead of refetching — no blank repaint.
  let history = $state(null)
  let historyThreadId = ''

  function useHistoryFor(threadId) {
    if (!threadId || threadId === historyThreadId) return
    closeHistory()
    historyThreadId = threadId
    history = createDurableHistory(threadId)
    registerTabHistory(threadId, history)
    void history.load()
  }

  function closeHistory() {
    if (!history) return
    unregisterTabHistory(historyThreadId, history)
    history.close()
    history = null
  }

  // $effect.pre, not $effect: it runs before the DOM update, so the very first
  // paint already has the channel — a plain effect would show the not-indexed
  // empty state for a frame, the flash this shared-channel design exists to
  // avoid. It also carries the tab forward: a transient tab gains its row id
  // when the durable row lands, and a resumed one moves to the successor's row
  // once the catalog indexes it, so the read-only view a detach leaves behind
  // is the conversation that just ran.
  $effect.pre(() => {
    useHistoryFor(identity.rowId)
  })

  onDestroy(closeHistory)

  // Resuming is a property of the thread, not of the detail view that used to
  // be the only way to reach it. The read-only footer already had the button;
  // it just never received a handler.
  const nativeId = $derived(identity.nativeId)
  // One record for both the gate and the request: the resolved catalog row with
  // the identity above laid over it.
  const target = $derived({ ...(identity.row ?? {}), provider: identity.provider, nativeId, id: identity.rowId })
  const gate = $derived(resumeGate(target))
  const resumer = createResumeAction()

  // A thread still waiting for its re-attach after a restart goes first once a
  // visible pane opens it, rather than waiting its turn in the boot queue. The
  // detached registry answers, because the active sidebar can leave a starting
  // thread out. One request per wait: the guard re-arms once the thread stops
  // starting, so a later restart asks again from the same mounted pane.
  const startingHostedId = $derived.by(() => {
    if (info) return ''
    const stored = app.detached.find((row) => row.provider === identity.provider && row.nativeThreadId === nativeId)
    return stored?.status === 'starting' || stored?.status === 'resting' ? stored.hostedThreadId ?? '' : ''
  })
  let prioritized = ''
  $effect(() => {
    if (!startingHostedId) {
      prioritized = ''
      return
    }
    if (!visible || startingHostedId === prioritized) return
    const asked = startingHostedId
    prioritized = asked
    // A failed request re-arms: the pane asks again when it is next shown.
    void prioritizeStartingThread(asked).then((ok) => {
      if (!ok && prioritized === asked) prioritized = ''
    })
  })
  const detacher = createDetachAction()
  const detachUi = $derived(detachLabels(info?.provider ?? tab.provider))

  function resume() {
    void resumer.resume(target)
  }

  async function detach() {
    if (await detacher.detach(info?.id, target)) {
      detachRequested = true
      settleHistorical()
    }
  }

  // The prefetch that was already here, with its result kept: the read-only
  // bar's "took" is the one field the catalog row cannot carry (it is a turn
  // aggregate, computed at read time on the detail response), and the cache
  // makes this the same request Details opens with — no extra chatter, and the
  // two surfaces agree by construction.
  let detail = $state(null)
  $effect(() => {
    const id = identity.rowId
    if (!id) {
      detail = null
      return
    }
    detail = cachedThreadDetail(id)
    let live = true
    void prepareThreadDetail(id)
      .then((fetched) => {
        if (live) detail = fetched
      })
      .catch(() => {
        // Opening Details retries and reports any durable error.
      })
    return () => {
      live = false
    }
  })
</script>

<div class="thread-pane-view" class:hidden={!visible} bind:this={paneElement}>
  <div class="thread-pane-content">
    {#if reconnecting && !boundInfo}
      <!-- The grace window: the session id went stale (usually a nano deploy
           restart) and the pane is waiting for the re-adopted successor. Quiet
           by design — most of the time it resolves before it is even read. -->
      <div class="pane-reconnecting">reconnecting…</div>
    {/if}
    {#if info}
      <!-- Live thread actions share the transcript control row in Conversation;
           historical Resume remains the only floating pane action. -->
      <div class="split">
        <section class="pane-left">
          <Conversation
            session={liveSession}
            {info}
            {tab}
            {paneId}
            {visible}
            {manualScrollAwayAt}
            onManualScrollAwayAt={rememberManualScrollAwayAt}
            chrome={false}
            onDetach={detach}
            {detachUi}
            detachState={detacher.state}
            onOpenThreadDetails={() => onOpenDetail?.()}
          />
        </section>
      </div>
    {:else if history}
      <HistoricalThreadView
        {history}
        {tab}
        {paneId}
        {manualScrollAwayAt}
        onManualScrollAwayAt={rememberManualScrollAwayAt}
        barFields={historicalBarFields(identity.row, detail?.took)}
        {visible}
        onResume={gate && nativeId ? resume : null}
        resumeBusy={resumer.state.busy}
        resumeError={resumer.state.error ?? ''}
        resumeDisabled={!gate?.enabled}
        resumeNote={gate?.note ?? ''}
        watchingSubagent={identity.row?.kind === 'subagent'}
        subagentWorking={identity.row?.fleet?.label === 'working'}
      />
    {:else}
      <div class="pane-transcript-empty">This thread is not indexed yet.</div>
    {/if}
  </div>
</div>
