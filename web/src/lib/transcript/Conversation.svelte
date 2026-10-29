<script>
  import { offlineTurnLabel, promptQueues } from '../shared/lifecycle_state.js'
  import { connection } from '../../connection.svelte.js'
  import { paneLink } from '../status/freshness.js'
  import { onMount, tick, untrack } from 'svelte'
  import { messageBodyHtml } from './message_body.js'
  import { composerLocked, composerPlaceholder, enterSubmits, startingPhase } from '../composer/composer_gate.js'
  import { viewport } from '../panes/viewport.svelte.js'
  import { renameSession } from '../../app.svelte.js'
  import { liveActivityForItem } from './live_turn.js'
  import { fmtDuration } from '../shared/duration_format.js'
  import { nextTickDelayMs } from '../explorer/explorer_format.js'
  import { effortsForModel, loadCodexModels } from '../composer/codex_models.js'
  import { claudeModelChoices } from '../composer/claude_models.js'
  import { draftKey, loadDraft, saveDraft } from '../composer/drafts.js'
  import { noteComposerText } from '../../build_reload.svelte.js'
  import { sessionBarFields } from '../shared/session_bar.js'
  import InfoBarFacts from './InfoBarFacts.svelte'
  import InteractionCard from '../composer/InteractionCard.svelte'
  import FeedbackButton from './FeedbackButton.svelte'
  import { visibleInteractions } from '../composer/interactions.js'
  import { newClientActionId } from '../composer/pending_sends.js'
  import {
	formatSubmissionTimestamp,
    formatTranscriptTimestamp,
    senderSubmissionTimestampFromFooter,
    itemText as durableItemText,
    liveItemCategory,
    shouldAutoLoadEarlier,
    transcriptItemKind,
    transcriptItemLabel,
    transcriptItemCategory,
    turnDurationsByFinalItem,
  } from '../../transcript_view.js'
  import { createTailNavigation } from './tail_navigation.js'
  import { browserDiagnosticIdentity, recordBrowserDiagnostic } from '../../browser_diagnostics.js'
  import { openFileTab } from '../../panes.svelte.js'
  import { fileReferenceLinks } from './file_references.js'
  import StreamingAgentMessage from './StreamingAgentMessage.svelte'
  import { liveItems as fileLiveItems, openFileTranscript } from './file_transcript.svelte.js'
  import { warmTranscript } from './warm_transcripts.js'
  import {
    actualTranscriptRows,
    compactTranscriptValidationID,
    compareTranscriptRows,
    expectedTranscriptRows,
    transcriptValidationKey,
  } from './transcript_validator.js'
  import {
    captureTranscriptAuditRows,
    compareTranscriptAuditRows,
  } from './transcript_audit.js'
  import { addTranscriptAuditListener } from '../../transcript_audit_events.js'

  // chrome=false is the pane embedding: the view bar renders either way (the
  // toggle is the only control there, and a pane needs it), but compacts to
  // clear the pane's floating detach button.
  //
  // Detach is the pane's action, not the conversation's — ThreadPaneView owns
  // the request and the float. It is handed down because the thread menu at the
  // bottom of this component is where a phone can reach it: the float is hidden
  // there, and a live thread with no way to let go of it is a trap.
  let {
    session,
    info,
    tab = null,
    paneId = null,
    visible = true,
    manualScrollAwayAt = 0,
    onManualScrollAwayAt = null,
    chrome = true,
    onDetach = null,
    detachUi = null,
    detachState = null,
    onOpenThreadDetails = null,
  } = $props()
  const store = $derived(session.store)
  const providerName = $derived({ codex: 'Codex', claude: 'Claude', grok: 'Grok' }[store.provider] ?? 'The provider')

  // A live row without a saved id yet may still be waiting for its saved copy.
  function transcriptAuditAwaitingDurable(item) {
    return !(item?.durableItemID || item?.id?.startsWith('durable:'))
  }

  function submissionTimestamp(item) {
    return formatSubmissionTimestamp(item?.senderSubmittedAt)
  }

  function openTranscriptFile(reference) {
    openFileTab(reference, {
      cwd: info?.cwd || tab?.cwd || '',
      catalogThreadId: tab?.catalogId || '',
      sessionId: session.id,
    }, paneId)
  }
  // Runtime status: the store's copy (fed by /state and the list poll) with
  // the summary prop as the instant fallback before the first /state lands.
  const sessionStatus = $derived(store.sessionStatus ?? info?.status ?? null)
  // Which kind of "starting" this is. Immutable per session, so the summary
  // prop is the only source needed — creating a session and attaching to a
  // thread look identical in the status alone (lib/composer/composer_gate.js).
  const spawnMode = $derived(info?.spawnMode ?? '')
  const startingAs = $derived(startingPhase(sessionStatus, spawnMode))

  // A file-only thread's rows and live text come from this pane's own stream
  // (lib/transcript/file_transcript.svelte.js), which reloads to a fresh tail on every
  // connect. A stream opened for a new profile or a return to view shows once
  // its first tail lands; until then the pane keeps what it showed, with the
  // new stream's error beside it if it fails first. An Enhanced stream seeded
  // with the thread's warm rows (lib/transcript/warm_transcripts.js) shows at once
  // instead: the warm stream kept them current while this pane's was closed.
  let fileFeed = $state.raw(null)
  // Whether this pane may show its turn state as live (lib/status/freshness.js).
  let link = $state('awaiting')
  let fileFeedShown = $state.raw(null)
  // Repair transcript reopens the stream: a fresh tail of Nano's saved
  // transcript, and nothing else.
  let fileReloads = $state(0)
  let repairNote = $state('')
  $effect(() => {
    void fileReloads
    if (!visible) return
    const target = untrack(() => session)
    const feedStream = {}
    const ref = store.hostedThreadId || session.id
    const warm = warmTranscript(ref)
    const seed = view === 'all' ? null : warm
    link = paneLink(untrack(() => link), 'opened')
    const feed = openFileTranscript(ref, view, {
      onStatus: (status, fresh) => {
        target.applyStreamStatus?.(feedStream, status, fresh)
        link = paneLink(link, 'status')
      },
      onDisconnect: () => target.streamDisconnected?.(feedStream),
      onDrop: () => { link = paneLink(link, 'dropped') },
      onSaved: (actionIDs) => target.settleEchoes?.(actionIDs),
      onTail: (listedIDs) => target.echoTail?.(listedIDs),
      seed,
    })
    fileFeed = feed
    if (seed) fileFeedShown = feed
    return () => feed.close()
  })
  $effect(() => {
    if (fileFeed?.state.tailed) fileFeedShown = fileFeed
  })
  const fileFeedError = $derived(fileFeed && fileFeed !== fileFeedShown && fileFeedShown ? fileFeed.state.history.error : null)
  const fileView = $derived(fileFeedShown ?? fileFeed)

  function feedbackContext() {
    return {
      hostedThreadId: store.hostedThreadId || info?.hostedThreadId || '',
      threadId: store.threadId || info?.threadId || tab?.nativeId || '',
      provider: store.provider || info?.provider || tab?.provider || '',
      view: view === 'all' ? 'dev' : 'enhanced',
      paneId: String(tab?.id || tab?.catalogId || paneId || session?.id || ''),
      pane: {
        working: shownTurn === 'working',
        pendingPrompts: pendingPromptCount,
        lastSavedRowId: durableItems.at(-1)?.id || '',
      },
    }
  }

  function repairFileTranscript() {
    if (liveTurnActive) {
      repairNote = "Repair transcript isn't available while an agent is working."
      return
    }
    repairNote = ''
    fileReloads += 1
  }
  const history = $derived(fileView?.state.history ?? null)
  const historyHasEarlier = $derived(
    Boolean(history?.page?.hasMoreBefore && (history?.page?.beforeCursor ?? history?.page?.nextBeforeCursor))
  )
  const activeRenderSource = 'file'
  // The durable block's turn durations, same rule as the read-only view: the
  // resumed pane shows the same history and must state the same facts about it.
  const durableTurnDurations = $derived(turnDurationsByFinalItem(history?.turns))

  let draft = $state('')
  let textareaEl = $state(null)
  let shiftDown = false
  let enterHandled = false
  let submitClaimed = false
  let scroller = $state(null)
  const tailNavigation = createTailNavigation(() => scroller)
  let initialPositioned = $state(false)
  let submittedActionID = $state('')
  let submittedShown = false

  // Sends Nano has not acknowledged (store.unsent): one line counts them and
  // opens or closes their rows, closed by default once there are two or more.
  let unsentToggled = $state(null)
  const unsentRows = $derived(store.unsent ?? [])
  const unsentOpen = $derived(unsentToggled ?? unsentRows.length < 2)
  // A display clock for the rows' countdowns: it runs only while one is
  // counting and the page is visible, and asks Nano nothing.
  let unsentNow = $state(Date.now())
  $effect(() => {
    if (!unsentRows.some((unsent) => unsent.retryAt)) return
    unsentNow = Date.now()
    const timer = setInterval(() => {
      if (document.visibilityState !== 'hidden') unsentNow = Date.now()
    }, 1_000)
    return () => clearInterval(timer)
  })

  function retryIn(unsent) {
    return Math.max(0, Math.ceil((unsent.retryAt - unsentNow) / 1_000))
  }

  function unsentLabel(unsent) {
    if (unsent.stopped) return 'Not acknowledged by Nano'
    const attempt = unsent.attempts ? ` (attempt ${unsent.attempts})` : ''
    if (unsent.inFlight) return `Not delivered yet: sending${attempt}`
    if (unsent.behind) return 'Not delivered yet: waiting for the earlier message'
    if (!unsent.posted) return connection.status === 'offline' ? 'Not delivered yet: waiting for the connection' : 'Not delivered yet: sending'
    if (unsent.retryAt) return `Not delivered yet: retrying in ${retryIn(unsent)}s${attempt}`
    return `Not delivered yet: retrying automatically${attempt}`
  }

  const unsentSummary = $derived.by(() => {
    const counting = unsentRows.filter((unsent) => unsent.retryAt)
    if (counting.length) return ` · retry in ${Math.min(...counting.map(retryIn))}s`
    if (connection.status === 'offline' && unsentRows.some((unsent) => !unsent.posted)) return ' · waiting for the connection'
    return ''
  })

  // 'clean' = enhanced transcript; 'all' = every item, nothing suppressed,
  // raw JSON one click away. The storage key and its two values predate the
  // category filter below and stay as they are.
  let view = $state(localStorage.getItem('nano-view') ?? 'clean')
  $effect(() => localStorage.setItem('nano-view', view))
  let transcriptValidatorEnabled = $state(localStorage.getItem('nano:transcript-validator') === 'on')
  let transcriptValidationReport = $state(null)
  let transcriptValidationScheduled = false
  let lastTranscriptValidationSignature = ''
  let transcriptValidationSequence = 0
  let transcriptAuditState = $state(null)
  const transcriptAuditsInFlight = new Set()

  const serverLiveItems = $derived(fileLiveItems(fileView?.state.live ?? []))
  // The person's own sends show at once, as delivering, until the stream's row
  // for the same action takes their place (store.echoes).
  const liveItems = $derived.by(() => {
    const listed = new Set(serverLiveItems.map((item) => item.clientActionId).filter(Boolean))
    const echoes = (store.echoes ?? []).filter((echo) => !listed.has(echo.clientActionId)).map((echo) => ({
      id: `echo:${echo.clientActionId}`, type: 'userMessage', text: echo.text, clientActionId: echo.clientActionId,
      status: 'delivering', allowedActions: [], label: echo.slow ? 'Still sending' : '', done: true, item: { type: 'userMessage' },
    }))
    return echoes.length ? [...serverLiveItems, ...echoes] : serverLiveItems
  })
  const durableItems = $derived(history?.items ?? [])
  // A file-only reply's live text names its message by native id, which its
  // saved row carries as nativeItemId. Live text whose message is already
  // saved is that message shown twice, so it takes the saved row's identity
  // and the duplicate checks see the pair.
  const savedIDByNative = $derived(new Map(
    durableItems.filter((item) => item?.nativeItemId).map((item) => [item.nativeItemId, item.id]),
  ))
  function liveSemanticID(item) {
    if (item.durableItemID) return item.durableItemID
    if (item.id.startsWith('durable:')) return item.id.slice('durable:'.length)
    if (item.id.startsWith('reply:')) return savedIDByNative.get(item.id.slice('reply:'.length))
    return undefined
  }
  const visibleItems = $derived(liveItems)
  const newestVisibleItemID = $derived(visibleItems.at(-1)?.id ?? '')
  const hasTranscript = $derived(liveItems.length > 0 || durableItems.length > 0)
  const expectedValidationRows = $derived(expectedTranscriptRows({
    durableItems: durableItems.map((item) => ({ ...item, text: durableItemText(item) })),
    liveItems: visibleItems.map((item) => ({ ...item, text: liveValidationText(item) })),
  }))
  // Nano's prompts for this thread are its live entries: each carries its
  // stored state, the actions the person may take and a label. A prompt needs
  // the person while Nano hands it over, holds it for the next turn, or holds
  // it for Resend or Discard; once the provider has it, its row is enough.
  // The count adds the sends Nano never acknowledged, the one prompt state
  // this browser keeps.
  const PROMPT_STATES_NEEDING_PERSON = new Set(['delivering', 'queued', 'uncertain', 'parked'])
  const needsPerson = (item) => item.type === 'userMessage' && PROMPT_STATES_NEEDING_PERSON.has(item.status)
  // A prompt on its way to the provider, or with it but not yet in its saved
  // transcript, has a filled dot; one Nano holds behind the running turn has a
  // hollow dot; a saved one, and one held for the person, has none. A prompt
  // the provider has with agent output below it is being answered: a dot there
  // would say it still waits while its answer shows.
  const PROVIDER_HAS_IT = new Set(['accepted', 'confirmed'])
  const answeredPrompts = $derived.by(() => {
    const answered = new Set()
    let agentBelow = false
    for (let index = visibleItems.length - 1; index >= 0; index--) {
      const item = visibleItems[index]
      if (item.type === 'agentMessage') agentBelow = true
      else if (item.type === 'userMessage' && agentBelow) answered.add(item.id)
    }
    return answered
  })
  function promptDot(item) {
    if (item.status === 'queued') return 'queued'
    if (item.status === 'delivering') return 'on-the-way'
    return PROVIDER_HAS_IT.has(item.status) && !answeredPrompts.has(item.id) ? 'on-the-way' : ''
  }
  const pendingPromptCount = $derived(liveItems.filter(needsPerson).length + store.unsent.length)
  // Nano has every prompt its stream lists: a send this browser still holds
  // as unacknowledged is Nano's now.
  $effect(() => {
    const listed = serverLiveItems.map((item) => item.clientActionId).filter(Boolean)
    // Only the stream's rows drive this; what the store reads while it steps
    // must not make the effect run again.
    untrack(() => {
      for (const actionID of listed) session.acknowledgeSend?.(actionID)
    })
  })

  async function chooseView(next) {
    if (next === view) return
    view = next
    await tick()
    atBottom = true
    if (scroller) scroller.scrollTop = scroller.scrollHeight
  }

  async function showEarlierHistory() {
    // Prepending saved rows preserves the reader’s position.
    const prevHeight = scroller?.scrollHeight ?? 0
    await fileView?.loadEarlier()
    await tick()
    if (scroller) scroller.scrollTop += scroller.scrollHeight - prevHeight
  }

  // Stick-vs-preserve must be decided from the position BEFORE the DOM grows —
  // this effect runs after, so a fresh measurement is already stale (a seeded
  // 415-item history would read as "far from bottom" and strand the reader at
  // the top). Same model as HistoricalTranscript: atBottom is tracked on every
  // scroll event; a fresh pane counts as at-bottom so the first growth (resume
  // seed or first live item) lands on the newest message.
  let atBottom = true

  $effect(() => {
    // Follow the true live edge, not the last transcript row. Working, a
    // pending prompt, and an interaction card all sit below the item list, so
    // a turnActive flip with no new row still has to re-pin.
    void liveItems.length
    void liveItems.at(-1)?.text
    void durableItems.at(-1)?.id
    void Boolean(thinking)
    void pendingInteractions.length
    if (!visible || !scroller || !atBottom) return
    // settleToBottom pins synchronously before it starts the bounded repair
    // frames. Reveal after that first pin: a busy stream can keep replacing
    // the repair loop before its callback runs, and waiting for the callback
    // leaves every already-mounted row under visibility:hidden indefinitely.
    tailNavigation.settleToBottom()
    if (!initialPositioned && hasTranscript) initialPositioned = true
  })

  $effect(() => {
    void thinkingWords.length
    void thinkingActivity?.label
    void thinkingActivity?.detail
    if (visible && atBottom && thinking) tailNavigation.followGrowth()
  })

  function onScroll() {
    atBottom = tailNavigation.observeScroll()
    if (hasTranscript) initialPositioned = true
    onManualScrollAwayAt?.(tailNavigation.manualScrollHoldAt())
    autoLoadEarlier()
  }

  // Reading upward loads saved pages while preserving the reader's position.
  function autoLoadEarlier() {
    if (!scroller) return
    if (shouldAutoLoadEarlier({
      scrollTop: scroller.scrollTop,
      hasOlder: historyHasEarlier,
      loadingEarlier: history?.loadingEarlier,
      earlierError: history?.earlierError,
    })) {
      void showEarlierHistory()
    }
  }

  function followStreamPresentation() {
    if (visible && atBottom) tailNavigation.followGrowth()
  }

  function transcriptMismatchCode(report) {
    const row = report.missing[0] || report.extra[0] || report.duplicates[0] || report.semanticDuplicates[0] ||
      report.blank[0] || report.typeMismatch[0]
    return row?.key || (report.orderMismatchCount ? 'row-order' : '')
  }

  function liveValidationText(item) {
    if (item.type === 'userMessage' || item.type === 'agentMessage') return item.text
    if (item.type === 'toolUse') {
      return `${item.item?.inputJson ?? JSON.stringify(item.item?.input ?? {})}${item.item?.output ?? ''}`
    }
    if (item.type === 'commandExecution') {
      return `${item.item?.command ?? ''}${item.item?.aggregatedOutput ?? ''}`
    }
    return auxBody(item)
  }

  function runTranscriptValidation(forcePublish = false) {
    if (!visible || !scroller) return
    const report = compareTranscriptRows(expectedValidationRows, actualTranscriptRows(scroller))
    transcriptValidationReport = report
    const signature = JSON.stringify({
      expected: report.expected.map((row) => [row.key, row.type, row.expectsContent]),
      actual: report.actual.map((row) => [row.key, row.type, row.hasContent]),
    })
    if (!forcePublish && signature === lastTranscriptValidationSignature) return
    lastTranscriptValidationSignature = signature
    const browserIdentity = browserDiagnosticIdentity()
    const paneIdentity = String(tab?.id || tab?.catalogId || paneId || session?.id || 'pane')
    const snapshotID = `${browserIdentity.tabId}:${paneIdentity}:${++transcriptValidationSequence}`
      void fetch('/api/v1/browser-validations', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({
        snapshotId: snapshotID,
        checkedAt: new Date().toISOString(),
        ...browserIdentity,
        paneId: paneIdentity,
        surface: 'transcript',
        viewMode: view,
        provider: store.provider || info?.provider || tab?.provider,
        threadId: store.threadId || info?.threadId || tab?.nativeId,
        sessionId: session?.id || info?.id || tab?.sessionId,
        hostedThreadId: store.hostedThreadId || info?.hostedThreadId,
        epoch: store.statusEpoch,
        revision: store.statusRevision,
        activeRenderSource,
        pageVisible: document.visibilityState === 'visible',
        surfaceVisible: visible,
        status: report.ok ? 'match' : 'mismatch',
        duplicateRenderKeyCount: report.duplicates.length,
        duplicateSemanticIdCount: report.semanticDuplicates.length,
        expectedRows: report.expected,
        mountedRows: report.actual,
      }),
    }).catch(() => {})
    recordBrowserDiagnostic('transcript.validation', {
      paneId: tab?.id || tab?.catalogId,
      provider: store.provider || info?.provider || tab?.provider,
      threadId: store.threadId || info?.threadId || tab?.nativeId,
      sessionId: session?.id || info?.id || tab?.sessionId,
      hostedThreadId: store.hostedThreadId || info?.hostedThreadId,
      epoch: store.statusEpoch,
      revision: store.statusRevision,
      activeRenderSource,
      expectedRowCount: report.expected.length,
      mountedRowCount: report.actual.length,
      missingRowCount: report.missing.length,
      extraRowCount: report.extra.length,
      duplicateRowCount: report.duplicates.length,
      semanticDuplicateRowCount: report.semanticDuplicates.length,
      blankRowCount: report.blank.length,
      typeMismatchCount: report.typeMismatch.length,
      orderMismatchCount: report.orderMismatchCount,
      status: report.ok ? 'match' : 'mismatch',
      code: transcriptMismatchCode(report),
    })
  }

  async function scheduleTranscriptValidation() {
    if (!visible || transcriptValidationScheduled) return
    transcriptValidationScheduled = true
    await tick()
    queueMicrotask(() => {
      transcriptValidationScheduled = false
      runTranscriptValidation()
    })
  }

  function transcriptAuditPaneIdentity() {
    return String(tab?.id || tab?.catalogId || paneId || session?.id || 'pane')
  }

  function transcriptAuditExclusionLabel(reason) {
    return String(reason || 'unknown').replaceAll('_', ' ')
  }

  function transcriptAuditStartBoundary() {
    const historyUnsettled = Boolean(history) && !['ready', 'empty', 'retained'].includes(history.state)
    if (historyUnsettled ||
      history?.loadingEarlier || history?.earlierError
    ) return 'unknown'
    if (historyHasEarlier) return 'earlier_unmounted'
    return 'start'
  }

  function transcriptAuditPaneThreadRef() {
    return String(tab?.catalogId || store.threadId || info?.threadId || tab?.nativeId || '')
  }

  function transcriptAuditTargetsThisThread(request) {
    if (tab?.catalogId) return request.catalogThreadId === String(tab.catalogId)
    const localReferences = [
      store.threadId, info?.threadId, tab?.nativeId,
    ].filter(Boolean).map(String)
    return localReferences.includes(String(request.threadId || '')) ||
      localReferences.includes(String(request.catalogThreadId || ''))
  }

  async function transcriptAuditJSON(response) {
    const body = await response.json().catch(() => null)
    if (!response.ok) throw new Error(body?.error?.message || `transcript audit failed (${response.status})`)
    return body
  }

  async function fulfillTranscriptAudit(request) {
    if (!request?.auditId || transcriptAuditsInFlight.has(request.auditId) || !visible || !scroller) return
    const browser = browserDiagnosticIdentity()
    if (request.tabId !== browser.tabId || request.pageId !== browser.pageId || request.paneId !== transcriptAuditPaneIdentity() ||
        !transcriptAuditTargetsThisThread(request)) return
    transcriptAuditsInFlight.add(request.auditId)
    transcriptAuditState = { status: 'checking', auditId: request.auditId, scopes: request.scopes }
    try {
      await tick()
      const capture = captureTranscriptAuditRows(scroller, request.scopes, {
        startBoundary: transcriptAuditStartBoundary(),
        // The pane mounts every live entry and the newest saved row.
        endBoundary: 'tail',
      })
      const query = new URLSearchParams({
        minProjectedSeq: String(capture.startBoundary === 'start' ? 0 : capture.minProjectedSeq || 0),
        maxProjectedSeq: String(capture.endBoundary === 'tail' ? 0 : capture.maxProjectedSeq || 0),
      })
      const referenceResponse = await fetch(`/api/v1/transcript-audits/${encodeURIComponent(request.auditId)}/reference?${query}`)
      if (!referenceResponse.ok) {
        const completed = await transcriptAuditJSON(await fetch(`/api/v1/transcript-audits/${encodeURIComponent(request.auditId)}`))
        transcriptAuditState = completed.result ?? { status: completed.status, reasons: ['reference_unavailable'] }
        return
      }
      const reference = await referenceResponse.json()
      const result = await compareTranscriptAuditRows(reference, capture, {
        scopes: request.scopes,
        viewMode: view,
      })
      const completed = await transcriptAuditJSON(await fetch(`/api/v1/transcript-audits/${encodeURIComponent(request.auditId)}/result`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({
          capturedAt: new Date().toISOString(),
          ...browser,
          paneId: transcriptAuditPaneIdentity(),
          paneThreadRef: transcriptAuditPaneThreadRef(),
          viewMode: view,
          pageVisible: document.visibilityState === 'visible',
          surfaceVisible: visible,
          projectionKey: reference.projectionKey,
          snapshotHeadSeq: reference.snapshotHeadSeq,
          result,
        }),
      }))
      transcriptAuditState = completed.result ?? { status: completed.status }
    } catch (error) {
      transcriptAuditState = { status: 'inconclusive', reasons: [error?.message ?? String(error)] }
    } finally {
      transcriptAuditsInFlight.delete(request.auditId)
    }
  }

  $effect(() => {
    if (!visible) return
    void expectedValidationRows
    void scheduleTranscriptValidation()
  })

  $effect(() => {
    if (!visible || !scroller) return
    const observer = new MutationObserver(() => {
      void scheduleTranscriptValidation()
      if (atBottom) tailNavigation.followGrowth()
    })
    observer.observe(scroller, { childList: true, subtree: true, characterData: true })
    void scheduleTranscriptValidation()
    return () => observer.disconnect()
  })

  $effect(() => {
    if (!visible) return
    const heartbeat = setInterval(() => runTranscriptValidation(true), 15_000)
    return () => clearInterval(heartbeat)
  })

  $effect(() => {
    if (!visible) return
    const publishVisibility = () => runTranscriptValidation(true)
    document.addEventListener('visibilitychange', publishVisibility)
    return () => document.removeEventListener('visibilitychange', publishVisibility)
  })

  function submittedRow(actionID) {
    if (!scroller || !actionID) return null
    return [...scroller.querySelectorAll('[data-client-action-id]')]
      .find((row) => row.dataset.clientActionId === actionID) ?? null
  }

  function recordSubmittedVisibility(actionID, status, outcome) {
    const matchingDOMActionRowCount = scroller
      ? [...scroller.querySelectorAll('[data-client-action-id]')]
        .filter((row) => row.dataset.clientActionId === actionID).length
      : 0
    recordBrowserDiagnostic('send.visibility', {
      paneId: tab?.id || tab?.catalogId,
      provider: store.provider || info?.provider || tab?.provider,
      threadId: store.threadId || info?.threadId || tab?.nativeId,
      sessionId: session?.id || info?.id || tab?.sessionId,
      hostedThreadId: store.hostedThreadId || info?.hostedThreadId,
      actionId: actionID,
      epoch: store.statusEpoch,
      revision: store.statusRevision,
      itemCount: liveItems.length,
      activeRenderSource,
      visibleItemCount: visibleItems.length,
      matchingDOMActionRowCount,
      pendingCount: pendingPromptCount,
      connected: Boolean(store.streamConnected),
      turnActive: Boolean(store.turnActive),
      status,
      outcome,
    })
  }

  function settleSubmittedPrompt(actionID, status) {
    const row = submittedRow(actionID)
    if (!row) {
      recordSubmittedVisibility(actionID, status, 'missing')
      return
    }
    recordSubmittedVisibility(actionID, status, 'anchoring')
    // Pin the scroller bottom so Working stays on screen.
    tailNavigation.settleToBottom(() => {
      const latest = submittedRow(actionID)
      if (!latest) {
        recordSubmittedVisibility(actionID, status, 'missing')
        return
      }
      const scrollerRect = scroller?.getBoundingClientRect()
      const rowRect = latest.getBoundingClientRect()
      const inView = Boolean(scrollerRect) &&
        rowRect.bottom > scrollerRect.top - 2 &&
        rowRect.top < scrollerRect.bottom + 2
      recordSubmittedVisibility(actionID, status, inView ? 'visible' : 'hidden')
    })
  }

  // The browser's guarantee is visual, not merely stateful: keep the live
  // edge in view when the file stream publishes the prompt. A real
  // scroll gesture still wins.
  $effect(() => {
    const actionID = submittedActionID
    if (!actionID || !visible) return
    // The prompt's live text remains until its saved row takes over.
    const shown = visibleItems.some((item) => item?.clientActionId === actionID)
    if (shown) submittedShown = true
    else if (submittedShown) {
      submittedActionID = ''
      return
    }
    let cancelled = false
    void tick().then(() => {
      if (cancelled || tailNavigation.hasManualScrollHold()) return
      settleSubmittedPrompt(actionID, 'pending')
    })
    return () => { cancelled = true }
  })

  let lastTailResetNonce = 0
  $effect(() => {
    const nonce = Number(tab?.tailResetNonce || 0)
    if (!visible || !nonce || nonce === lastTailResetNonce) return
    lastTailResetNonce = nonce
    tailNavigation.restoreManualScrollHold(manualScrollAwayAt)
    const forceTail = Number(tab?.forceTailResetNonce || 0) === nonce
    if (!forceTail && tailNavigation.hasManualScrollHold()) return
    tailNavigation.clearManualScrollHold()
    onManualScrollAwayAt?.(0)
    atBottom = true
    tailNavigation.settleToBottom(() => {
      if (hasTranscript) initialPositioned = true
    })
  })

  // Connection determines admission before any stale work flag is considered.
  const composerDisabled = $derived(composerLocked(sessionStatus))
  // Admission only: Stop, Queue and running-turn input. turnActive includes
  // this tab's own send before Nano answers, so it never labels the turn.
  const liveTurnActive = $derived(sessionStatus === 'ready' && store.turnActive)
  // The turn as the server published it (threadStatus, the sidebar's value
  // too): Working, waiting and the clock render this.
  const shownTurn = $derived(sessionStatus === 'ready' ? store.threadStatus : 'idle')
  // The pane hears its turn state only on its own stream. Until that stream
  // has spoken, it offers no Stop; while it is down, or Nano does not answer
  // this browser, it shows the last state it heard as offline.
  const reachable = $derived(connection.status !== 'offline')
  const statusLive = $derived(reachable && link === 'live')
  const offlineStatus = $derived(reachable && link !== 'dropped' ? '' : offlineTurnLabel(store.threadStatus))
  const queuePrompt = $derived(promptQueues(sessionStatus, store.turnActive))
  // Send joins the working turn when the provider takes running-turn input;
  // Queue then holds a message for the next turn. A turn waiting on a dialog
  // queues: a message never answers it.
  const joinsTurn = $derived(liveTurnActive && store.runningTurnInput && pendingInteractions.length === 0)
  // The live turn's elapsed. The anchor is the SERVER's — the same immutable
  // instant the rail reads (D4) — so the pane and the rail cannot disagree, and
  // a turn that began before this browser opened is still timed correctly. It
  // arrives on the session summary, which the runtime SSE nudge refreshes, with
  // /state as the fallback for a pane that opened mid-turn.
  //
  // This replaces a codex-only clock anchored at the browser's own Date.now()
  // (a D2 violation: the browser invented the start), and it makes the claude
  // pane show an elapsed for the first time.
  // A file-only pane's turn start comes from its own stream, with its status.
  const turnAnchorMs = $derived(Number(store.turnStartedAtMs) || 0)
  // Waiting on a dialog is not working (D15). The server publishes it as
  // needs_you, the same value the sidebar row shows.
  const waitingOnOperator = $derived(shownTurn === 'needs_you')
  // While the provider compacts the turn's context, its own start signal is
  // the anchor: the head reads Compacting… and times the compaction.
  const compactingSinceMs = $derived(shownTurn === 'working' ? Number(store.compactingSinceMs) || 0 : 0)
  const elapsedAnchorMs = $derived(compactingSinceMs || turnAnchorMs)
  let liveNow = $state(Date.now())
  const liveElapsed = $derived(
    shownTurn === 'working' && statusLive && elapsedAnchorMs ? fmtDuration(liveNow - elapsedAnchorMs) : ''
  )

  // D6: tick to the next visible change, at the tenth-of-a-second the first ten
  // seconds need and the second boundary after that. Nothing is scheduled while
  // there is no clock to move.
  $effect(() => {
    if (shownTurn !== 'working' || !elapsedAnchorMs || !statusLive) return
    const anchor = elapsedAnchorMs
    let timer
    const tick = () => {
      const at = Date.now()
      liveNow = at
      timer = setTimeout(tick, nextTickDelayMs(at - anchor))
    }
    tick()
    return () => clearTimeout(timer)
  })

  // --- The live thinking surface ---------------------------------------------
  // The chat-agent idiom: reasoning is process, not transcript. It streams
  // while the model works and is gone the moment the answer starts, so nothing
  // it says survives into the record. This surface also carries the liveness
  // the old "working" bar carried — the enhanced view hides reasoning and tool
  // items, so without it a working session reads as a frozen one.
  //
  // Turn state decides whether it is visible. Agent messages can be emitted in
  // the middle of a turn, and the enhanced view intentionally keeps only
  // messages warm, so the newest visible message cannot tell us that work has
  // ended. The server's published turn state (threadStatus) is the authority.
  const THINKING_TAIL_WORDS = 48
  // No wire item yet is a real working state, not an idle one: a codex
  // re-attach mid-turn replays nothing, so during a long silent tool run the
  // pane would otherwise read as dead while the rail blinks (observed on the
  // infra sessions, 2026-07-31). A bare synthetic item keeps the enhanced
  // surface generic — "Working" plus the server-anchored clock — until the
  // authoritative turn state becomes terminal.
  const thinking = $derived(
    view === 'clean' && (shownTurn === 'working' || shownTurn === 'needs_you')
      ? { type: 'bareWorking', item: {}, text: '' }
      : null
  )
  // liveActivityForItem is the same label vocabulary the codex live turn uses,
  // so the surface names activity identically to the rest of the app. A turn
  // whose newest item is still the user's own message reads as plain "Working".
  const thinkingActivity = $derived(
    thinking ? liveActivityForItem({ ...thinking.item, type: thinking.type }) : null
  )
  // The newest reasoning since the last answer — not necessarily the tail.
  // Reasoning arrives as a short burst of deltas and is immediately overtaken
  // by the tool call it was planning, so binding the text to "reasoning IS the
  // tail" made it flash for a few hundred milliseconds and vanish (observed on
  // a live turn: the label reached Thinking, but a 350ms DOM sample never once
  // caught text under it). Carrying the thought while the work it planned runs
  // is both steadier to read and closer to the chat idiom.
  //
  // The search stops at the user's message — the start of this turn — and NOT
  // at an agent message, because codex narrates mid-turn: the real order is
  // reasoning → agentMessage("I'll check the file") → commandExecution
  // (probe-verified). Treating any agent message as the boundary let that
  // narration wipe the thought before the command it was planning even ran.
  const thinkingSource = $derived.by(() => {
    if (!thinking) return null
    for (let i = liveItems.length - 1; i >= 0; i -= 1) {
      const it = liveItems[i]
      if (it.type === 'userMessage') return null
      if (it.type === 'reasoning' && it.text) return it
    }
    return null
  })

  // Keys are source-scoped absolute word indices: stable as the tail window
  // slides (a word already on screen must not re-run its entrance), but
  // distinct across reasoning items, so a genuinely new thought animates in.
  //
  // Codex titles its reasoning summaries in markdown bold
  // ("**Planning handler function search command**", probe-verified), and this
  // surface renders plain dim text — the same styleRawText rule reasoning
  // follows everywhere else — so the emphasis markers would show as literal
  // asterisks. Strip them rather than pulling markdown into a status line.
  const thinkingWords = $derived(
    (thinkingSource?.text ?? '')
      .replaceAll('**', '')
      .split(/\s+/)
      .filter(Boolean)
      .map((word, index) => ({ word, key: `${thinkingSource.id}:${index}` }))
      .slice(-THINKING_TAIL_WORDS)
  )

  // --- Pending interactions --------------------------------------------------
  // Dialogs the agent is waiting on, rendered at the live edge below the
  // transcript. State is server-owned (interactions.go) and arrives on /state
  // plus the nano-origin feed events; this view only decides what is still
  // answerable. The clock ticks so an autoResolutionMs deadline collapses the
  // card on time instead of at the next unrelated re-render.
  let interactionNow = $state(Date.now())
  $effect(() => {
    if (!store.pendingInteractions.some((i) => i.expiresAt)) return
    const timer = setInterval(() => (interactionNow = Date.now()), 1000)
    return () => clearInterval(timer)
  })
  const pendingInteractions = $derived(sessionStatus === 'ready' ? visibleInteractions(store.pendingInteractions, interactionNow) : [])

  // --- Model / effort, and the stop affordance -------------------------------
  // One control surface, two provider truths. Codex takes model and effort as
  // turn/start overrides ("for this turn and subsequent turns"), so its
  // controls change the NEXT turn rather than reconfiguring the thread; claude's
  // set_model changes the live session on the spot. Either way the server owns
  // the values and this reads them back from store.turnOptions, so a reload —
  // or a second tab — shows the truth.
  const isCodex = $derived(store.provider === 'codex')
  const isClaude = $derived(store.provider === 'claude')
	// The store is the one authority: it accepts each metadata read in order,
	// so a recovered read clears the warning that cached copies would keep.
	const metadataUnavailable = $derived(Boolean(store.metadataUnavailable))
	const metadataDegraded = $derived(Boolean(store.metadataDegraded))
	const metadataControlsDisabled = $derived(metadataUnavailable || metadataDegraded)
  // Claude's switch is a live-session `set_model` control request, not a
  // per-turn override: the child, its session id and the whole conversation
  // survive it. Same control, different truth — which is why the label says
  // "Session default" there and "Thread default" on codex.
  const claudeModels = $derived(claudeModelChoices(store.meta?.controlInit))
  let codexModels = $state([])
  // loadCodexModels resolves one shared, cached promise per page, so asking on
  // every codex pane costs nothing after the first.
  $effect(() => {
    if (isCodex) loadCodexModels().then((catalog) => (codexModels = catalog))
  })
  // "" is a real, meaningful choice: no override, run on the thread's own
  // model. It is labelled with that model rather than left blank, because a
  // blank dropdown reads as "unknown" when it actually means "unchanged".
	// Once canonical metadata is unavailable, stale turn options are no longer
	// authoritative posture. Keep the native identity and rebuildable branch in
	// the footer, but blank every provider-setting fact and disable its controls.
  const turnModel = $derived(metadataUnavailable ? '' : (store.turnOptions?.model ?? ''))
	// Hosted facts come only from the canonical session summary/state posture.
	// Transcript metadata and the tab cache are historical/display aids, never
	// a fallback for a hosted control after its durable identity exists.
  const threadModel = $derived(metadataUnavailable ? '' : (store.posture?.model || ''))
  const effortChoices = $derived(effortsForModel(codexModels, turnModel || threadModel))
  const turnEffort = $derived(metadataUnavailable ? '' : (store.turnOptions?.effort ?? ''))
  const postureEffort = $derived(metadataUnavailable ? '' : (store.posture?.effort ?? ''))
  const turnServiceTier = $derived(metadataUnavailable ? '' : (store.turnOptions?.serviceTier ?? ''))
  const threadServiceTier = $derived(metadataUnavailable ? '' : (store.posture?.serviceTier || ''))
  const speedChoices = $derived(codexModels.find((m) => m.model === (turnModel || threadModel))?.serviceTiers ?? [])
  const controlServiceTier = $derived(turnServiceTier || threadServiceTier)
  const tierEvidence = $derived(metadataUnavailable ? null : store.hostedMetadata?.fields?.serviceTier)
  const effectiveServiceTier = $derived(tierEvidence?.effectiveValue || (store.hostedMetadata ? '' : threadServiceTier))
  const serviceTierConfirmed = $derived(tierEvidence?.effectiveDisposition === 'confirmed' && tierEvidence?.effectiveSource === 'provider')
  const pendingServiceTier = $derived(tierEvidence?.desiredDisposition === 'requested' ? tierEvidence.desiredValue : '')
  const speedRejection = $derived(tierEvidence?.desiredDisposition === 'rejected' && tierEvidence.desiredValue !== effectiveServiceTier
    ? `Speed request ${tierEvidence.desiredValue} was rejected by Codex. Confirmed speed: ${effectiveServiceTier || 'unknown'}.`
    : '')

  let speedCatalogError = $state('')
  async function refreshSpeedChoices() {
    speedCatalogError = ''
    try {
      codexModels = await loadCodexModels(true)
    } catch (err) {
      speedCatalogError = err.message ?? String(err)
    }
  }

  function toggleTurnControls() {
    menuTurnControls = !menuTurnControls
    if (menuTurnControls && isCodex) void refreshSpeedChoices()
  }

	function chooseModel(value) {
	if (metadataControlsDisabled) return
    // Effort levels are per-model; an override carried onto a model that does
    // not advertise it would be rejected by the app-server at turn/start, so
    // clear it with the switch and let the operator re-pick.
    const patch = { model: value }
    if (turnEffort && !effortsForModel(codexModels, value || threadModel).includes(turnEffort)) patch.effort = ''
    session.setTurnOptions?.(patch)
  }

  const barFields = $derived(
    sessionBarFields({
      provider: store.provider,
		cwd: metadataUnavailable ? '' : (store.meta?.cwd ?? info?.cwd ?? ''),
      gitBranch: store.meta?.gitBranch ?? '',
      model: turnModel,
      threadModel,
      effort: turnEffort,
		threadEffort: postureEffort,
      serviceTier: effectiveServiceTier,
      serviceTierConfirmed,
      pendingServiceTier,
      threadId: store.resolvedNativeThreadId,
      threadPending: Boolean(store.hostedThreadId) && store.nativeIdentityStatus !== 'ready',
    })
  )

  // --- The thread menu -------------------------------------------------------
  // Model and effort are things an operator changes occasionally, so they no
  // longer hold a permanent row above the composer; the ⋯ at the end of the info
  // bar opens them. It also carries what the phone layout has nowhere else to
  // put — the view toggle and detach, both of which stay inline on desktop.
  //
  // Everything in here is scoped to THIS thread. The settings gear stays global:
  // that split is what keeps either surface predictable.
  const hasModelControl = $derived(isCodex || (isClaude && claudeModels.length > 0))
  let menuOpen = $state(false)
  let menuTurnControls = $state(false)
  let menuRoot = $state(null)

  function closeMenu() {
    menuOpen = false
    menuTurnControls = false
  }

  function toggleMenu() {
    if (menuOpen) closeMenu()
    else menuOpen = true
  }

  function onWindowClick(e) {
    if (menuOpen && menuRoot && !menuRoot.contains(e.target)) closeMenu()
  }

  function onWindowKeydown(e) {
    if (e.key === 'Escape' && menuOpen) closeMenu()
  }

  // Deliberately leaves the menu open: a detach that succeeds unmounts this
  // whole component with the pane's live view, and one that fails has an error
  // to show — closing would hide the only report of it on a phone.
  function detachFromMenu() {
    onDetach?.()
  }

  let sendBlocked = $state(false)
  let sendBlockedTimer = null
  let renaming = $state(false)
  let renameValue = $state('')
  let renameSaving = $state(false)
  let renameError = $state(null)
  let renameInput = $state(null)

  function beginRename() {
    renameValue = info?.name ?? ''
    renameError = null
    renaming = true
    void tick().then(() => renameInput?.focus())
  }

  function cancelRename() {
    if (renameSaving) return
    renaming = false
    renameError = null
  }

  async function saveRename() {
    const name = renameValue.trim()
    if (!name) {
      renameError = 'A name is required.'
      return
    }
    if (!info?.id || renameSaving) return
    renameSaving = true
    renameError = null
    try {
      await renameSession(info.id, name)
      renaming = false
      menuOpen = false
    } catch (error) {
      renameError = error?.message ?? String(error)
    } finally {
      renameSaving = false
    }
  }

  function onRenameKeydown(event) {
    if (event.key === 'Enter') {
      event.preventDefault()
      void saveRename()
    } else if (event.key === 'Escape') {
      event.preventDefault()
      cancelRename()
    }
  }

  function pulseSendBlocked() {
    sendBlocked = false
    requestAnimationFrame(() => {
      sendBlocked = true
      clearTimeout(sendBlockedTimer)
      sendBlockedTimer = setTimeout(() => {
        sendBlocked = false
        sendBlockedTimer = null
      }, 360)
    })
    textareaEl?.focus()
  }

  // nextTurn is read off an options object; a click handler's event has none.
  async function submit({ nextTurn = false } = {}) {
    let text = draft.trim()
    if (!text || composerDisabled || submitClaimed) return
    submitClaimed = true
    try {
      // Only a session that does not accept prompts holds the draft back. The
      // network is not asked first: a send that gets no answer is kept below
      // and retried until Nano answers.
      if (!session.canSubmitTurn?.()) {
        pulseSendBlocked()
        return
      }
      // /fast is a client command in Codex, not prompt text understood by the
      // app-server. Toggle Nano's requested speed setting and clear the
      // composer only after Nano durably accepts it; a failed change remains editable.
      if (isCodex && ['/fast', '/ultrafast'].includes(text.toLowerCase())) {
		if (metadataControlsDisabled) return
        const commandedTier = text.toLowerCase() === '/ultrafast' ? 'ultrafast' : 'priority'
        const nextTier = controlServiceTier === commandedTier ? 'default' : commandedTier
        const changed = await session.setTurnOptions?.({ serviceTier: nextTier })
        if (!changed) return
        saveDraft(draftStorageKey, '')
        if (sessionDraftStorageKey !== draftStorageKey) saveDraft(sessionDraftStorageKey, '')
        draft = ''
        return
      }
      // This is the ownership handoff. Clear ordinary draft storage in the same
      // event that creates the pending operation; an uncertain acknowledgement
      // must never put a second editable copy beside that operation.
      saveDraft(draftStorageKey, '')
      // A fresh session briefly uses a session-id fallback before its durable
      // provider thread id lands. Clear both identities at the handoff so a
      // pre-migration copy cannot return to the composer after a reload.
      if (sessionDraftStorageKey !== draftStorageKey) saveDraft(sessionDraftStorageKey, '')
      draft = ''
      // The store decides send-vs-queue from its own turn state, so the composer
      // never has to race it: a turn that completes between the keystroke and the
      // call is still handled correctly on the other side.
      const clientActionId = newClientActionId()
      const send = session.sendTurn(text, { nextTurn: nextTurn === true, clientActionId })
      submittedActionID = clientActionId
      submittedShown = false
      // Ownership has moved to the operation. Release the preflight latch now
      // so the operator can type and queue a genuinely different next prompt
      // while this request waits for its acknowledgement.
      submitClaimed = false
      // Submitting is an explicit request to move to the live edge, even when the
      // reader had deliberately scrolled up. The saved row may arrive after
      // this request returns; the reactive anchoring path handles that arrival.
      await tick()
      atBottom = true
      tailNavigation.clearManualScrollHold()
      onManualScrollAwayAt?.(0)
      if (submittedActionID) settleSubmittedPrompt(submittedActionID, 'pending')
      else tailNavigation.settleToBottom()
      // A definitive rejection hands the text back through the store's
      // restoreDraft, above anything typed meanwhile.
      if (await send === 'failed') submittedActionID = ''
    } finally {
      submitClaimed = false
    }
  }

  function onKeydown(e) {
    shiftDown = e.shiftKey
    if (!enterSubmits(e.key, { shiftKey: e.shiftKey, coarse: viewport.coarse })) return
    e.preventDefault()
    enterHandled = true
    submit()
  }

  function onBeforeInput(e) {
    if (e.inputType !== 'insertLineBreak' || viewport.coarse || shiftDown) return
    e.preventDefault()
    if (!enterHandled) submit()
    enterHandled = false
  }

  function onKeyup(e) {
    shiftDown = e.shiftKey
  }

  function resizeTextarea() {
    if (!textareaEl) return
    const max = Math.max(48, window.innerHeight * 0.4)
    textareaEl.style.height = 'auto'
    const nextHeight = Math.min(textareaEl.scrollHeight, max)
    textareaEl.style.height = String(nextHeight) + 'px'
    textareaEl.style.overflowY = textareaEl.scrollHeight > max ? 'auto' : 'hidden'
  }

  function scheduleResize() {
    if (typeof requestAnimationFrame === 'function') requestAnimationFrame(resizeTextarea)
    else resizeTextarea()
  }

  function onInput() {
    enterHandled = false
    scheduleResize()
  }

  // Drafts can arrive from per-thread storage without an input event. Keep
  // those drafts, paste operations, and viewport changes on the same sizing
  // path as ordinary typing. `visible` is tracked too: a background tab's
  // composer mounts under `display:none`, where scrollHeight reads 0, so it
  // needs a re-measure once its pane actually becomes visible.
  $effect(() => {
    draft
    visible
    scheduleResize()
  })

  onMount(() => {
    resizeTextarea()
    const removeTranscriptAuditListener = addTranscriptAuditListener((request) => {
      void fulfillTranscriptAudit(request)
    })
    window.addEventListener('resize', resizeTextarea)
    window.visualViewport?.addEventListener('resize', resizeTextarea)
    return () => {
      tailNavigation.destroy()
      removeTranscriptAuditListener()
      clearTimeout(sendBlockedTimer)
      window.removeEventListener('resize', resizeTextarea)
      window.visualViewport?.removeEventListener('resize', resizeTextarea)
    }
  })

  // --- Per-tab drafts --------------------------------------------------------
  // Keyed by thread, so a half-written message survives switching tabs, a
  // reload, and a re-attach (which mints a new session id). The key resolves
  // late — a fresh claude session has no native id until its handshake lands —
  // so the effect below re-reads storage when it changes, and only adopts the
  // stored draft when the box is empty: overwriting what the operator is
  // currently typing would be worse than losing a stale draft.
  const sessionDraftStorageKey = $derived(draftKey({ sessionId: session.id }))
  const draftStorageKey = $derived(draftKey({
    provider: store.provider,
    threadId: store.threadId || store.meta?.threadId,
    sessionId: session.id,
  }))
  let previousDraftStorageKey = ''
  $effect(() => {
    const key = draftStorageKey
    const previousKey = previousDraftStorageKey
    previousDraftStorageKey = key
    let stored = loadDraft(key)
    if (key && key !== sessionDraftStorageKey) {
      const fallback = loadDraft(sessionDraftStorageKey)
      // Migrate only when this mounted composer genuinely started on the
      // fallback key. If stable identity was known from its first render, any
      // fallback value belongs to an older handoff and must be discarded.
      if (previousKey === sessionDraftStorageKey && fallback && !stored) {
        saveDraft(key, fallback)
        stored = fallback
      }
      if (fallback) saveDraft(sessionDraftStorageKey, '')
    }
    // This effect is for a key change, not for every keystroke. Tracking
    // draft here races the save effect when the operator deletes the text:
    // the old localStorage value can be loaded back before it is removed.
    if (stored && !untrack(() => draft.trim())) draft = stored
  })
  $effect(() => {
    const restoration = store.restoreDraft
    if (!restoration) return
	const recovered = Array.isArray(restoration.texts) ? restoration.texts : [restoration.text]
	const current = untrack(() => draft)
	draft = [...recovered.filter((text) => typeof text === 'string' && text.length > 0), current]
	  .filter((text) => text.length > 0)
	  .join('\n\n')
    session.consumeRestoreDraft?.()
  })
  $effect(() => {
    const text = draft
    saveDraft(draftStorageKey, text)
  })
  // A tab does not reload onto a new Nano build by itself while this holds text.
  const composer = {}
  const composerHasText = $derived(Boolean(draft.trim()))
  $effect(() => {
    noteComposerText(composer, composerHasText)
    return () => noteComposerText(composer, false)
  })

  // Aux items render as a dim left-ruled block, mirroring v3's transcript idiom.
  const AUX_LABELS = {
    reasoning: 'reasoning',
    commandExecution: 'shell',
    fileChange: 'file change',
    mcpToolCall: 'mcp tool',
    dynamicToolCall: 'tool',
    webSearch: 'web search',
    plan: 'plan',
    subAgentActivity: 'sub-agent',
    contextCompaction: 'compaction',
  }

  function auxBody(it) {
    switch (it.type) {
      case 'reasoning':
        return it.text
      case 'fileChange':
        return (it.item.changes ?? []).map((c) => `${c.kind ?? 'edit'} ${c.path}`).join('\n')
      case 'mcpToolCall':
        return `${it.item.server}.${it.item.tool ?? ''} ${JSON.stringify(it.item.arguments ?? {})}`
      case 'dynamicToolCall':
        return `${it.item.namespace ?? ''}/${it.item.tool ?? ''} ${JSON.stringify(it.item.arguments ?? {})}`
      case 'webSearch':
        return it.item.query ?? ''
      case 'plan':
        return it.text
      case 'contextCompaction':
        // Arrives content-free ({id, type} only, probe-verified): a marker,
        // not a JSON dump.
        return 'context compacted'
      default:
        return JSON.stringify(it.item)
    }
  }

</script>

<svelte:window onclick={onWindowClick} onkeydown={onWindowKeydown} />

{#if store.deliveryExpiries > 0}
  <div class="history-fallback-note" role="status">{providerName} is slow right now: prompts may take minutes to be taken.</div>
{/if}
{#if speedCatalogError}
  <div class="history-fallback-note" role="alert">{speedCatalogError}</div>
{/if}
{#if speedRejection}
  <div class="history-fallback-note" role="alert">{speedRejection}</div>
{/if}
{#if metadataUnavailable}
  <div class="history-fallback-note" role="alert">Hosted metadata is unavailable. Current model, effort, permission, tier, and working directory are unknown until the canonical control store recovers.</div>
{:else if metadataDegraded}
  <div class="history-fallback-note" role="alert">Hosted metadata may be stale because a newer provider update could not be recorded. Showing the last committed canonical values.</div>
{/if}

<!-- One definition, two homes: inline in the view bar on desktop, inside the
     thread menu on a phone. Duplicating the markup would let the two drift. -->
{#snippet viewToggle()}
  <div class="view-toggle" role="radiogroup" aria-label="transcript detail">
    <button class:on={view === 'clean'} onclick={() => chooseView('clean')} aria-pressed={view === 'clean'}>Enhanced</button>
    <button class:on={view === 'all'} onclick={() => chooseView('all')} aria-pressed={view === 'all'}>All</button>
  </div>
{/snippet}

{#snippet viewHint()}
  {#if view === 'all'}
    <!-- Counts only the live block; durable items are rendered above it. -->
    <span class="view-hint">{liveItems.length} items, nothing suppressed</span>
  {/if}
{/snippet}

{#snippet transcriptAuditControls()}
  <div class="transcript-audit-controls" data-testid="transcript-audit-controls">
    <button
      class="transcript-audit-button"
      title="Reload this pane from Nano's saved transcript"
      onclick={repairFileTranscript}
    >Repair transcript</button>
    {#if repairNote && liveTurnActive}<span class="view-hint" role="status">{repairNote}</span>{/if}
    <FeedbackButton context={feedbackContext} />
  </div>
{/snippet}

{#snippet threadDetailsToggle()}
  {#if onOpenThreadDetails}
    <button
      class="thread-details-toggle"
      aria-label="Open Thread Details"
      title="Open thread details in another pane"
      onclick={onOpenThreadDetails}
    >Open Thread Details</button>
  {/if}
{/snippet}

{#snippet detachToggle()}
  {#if onDetach}
    <button
      class="detach-toggle"
      disabled={detachState?.busy}
      title={detachUi?.title}
      onclick={detachFromMenu}
    >{detachState?.busy ? detachUi?.busy : detachUi?.label}</button>
  {/if}
{/snippet}

<!-- The server decides which actions a prompt offers; a send Nano never
     acknowledged offers both. -->
{#snippet promptActions(actions, onResend, onDiscard)}
  {#if actions.length}
    <div class="pending-send-actions">
      {#if actions.includes('resend')}
        <button type="button" class="pending-send-action" onclick={onResend}>Resend</button>
      {/if}
      {#if actions.includes('discard')}
        <button type="button" class="pending-send-action" onclick={onDiscard}>Discard</button>
      {/if}
    </div>
  {/if}
{/snippet}

{#snippet pendingUserDot(dot)}
  {#if dot}
    {@const name = dot === 'queued' ? 'Queued' : `On its way to ${providerName}`}
    <span class="pending-user-dot" class:queued={dot === 'queued'} role="img" title={name} aria-label={name}></span>
  {/if}
{/snippet}

{#if !viewport.mobile}
  <!-- A 390px screen cannot spare a whole band for one toggle and a count, so
       the bar leaves the flow entirely there and both move into the ⋯ menu. -->
  <div class="view-bar" class:in-pane={!chrome}>
    {@render viewToggle()}
    {@render viewHint()}
    {@render transcriptAuditControls()}
    {@render threadDetailsToggle()}
    {@render detachToggle()}
  </div>
  {#if detachState?.error}<div class="view-control-error" role="alert">{detachState.error}</div>{/if}
{/if}
{#if transcriptAuditState && transcriptAuditState.status !== 'checking'}
  <div class="transcript-audit-result" class:mismatch={transcriptAuditState.status === 'mismatch'} class:inconclusive={transcriptAuditState.status === 'inconclusive'} role="status" data-testid="transcript-audit-result">
    <strong>{transcriptAuditState.status === 'match' ? 'Displayed window matches saved transcript' : transcriptAuditState.status === 'mismatch' ? 'Displayed window differs from saved transcript' : 'Transcript check inconclusive'}</strong>
    {#if transcriptAuditState.counts}
      <span>saved {transcriptAuditState.counts.reference} · browser {transcriptAuditState.counts.browser} · checked text {transcriptAuditState.counts.contentChecked}</span>
    {/if}
    {#if transcriptAuditState.range?.partial}<span>Partial displayed range only.</span>{/if}
    {#if transcriptAuditState.counts && transcriptAuditState.status === 'mismatch'}
      <span>missing {transcriptAuditState.counts.missing} · extra {transcriptAuditState.counts.extra} · duplicate {transcriptAuditState.counts.duplicate} · reordered {transcriptAuditState.counts.reordered} · changed {transcriptAuditState.counts.changed}</span>
    {/if}
    {#if transcriptAuditState.reasons?.length}<span>{transcriptAuditState.reasons.join(' · ')}</span>{/if}
    {#if transcriptAuditState.exclusions?.length}<span>{transcriptAuditState.exclusions.reduce((sum, item) => sum + item.count, 0)} row(s) not checked: {transcriptAuditState.exclusions.map((item) => `${transcriptAuditExclusionLabel(item.reason)} (${item.count})`).join(' · ')}.</span>{/if}
  </div>
{/if}
{#if transcriptValidatorEnabled}
  <div
    class="transcript-validator"
    class:mismatch={transcriptValidationReport && !transcriptValidationReport.ok}
    role="status"
    data-testid="transcript-validator"
  >
    {#if transcriptValidationReport}
      <div class="transcript-validator-summary">
        <strong>{transcriptValidationReport.ok ? 'Render matches' : 'Render mismatch'}</strong>
        <span>should show {transcriptValidationReport.expected.length} · browser shows {transcriptValidationReport.actual.length}</span>
      </div>
      {#if !transcriptValidationReport.ok}
        <div class="transcript-validator-counts">
          missing {transcriptValidationReport.missing.length} · extra {transcriptValidationReport.extra.length} · duplicate {transcriptValidationReport.duplicates.length} · semantic duplicate {transcriptValidationReport.semanticDuplicates.length} · blank {transcriptValidationReport.blank.length} · wrong type {transcriptValidationReport.typeMismatch.length} · out of order {transcriptValidationReport.orderMismatchCount}
        </div>
      {/if}
      <details>
        <summary>Compare rows</summary>
        <div class="transcript-validator-columns">
          <div>
            <b>Should show</b>
            {#each transcriptValidationReport.expected as row, index (row.key)}
              <code>{index + 1}. {row.type} · {row.source}/{compactTranscriptValidationID(row.id)}</code>
            {/each}
          </div>
          <div>
            <b>Browser shows</b>
            {#each transcriptValidationReport.actual as row, index (`${row.key}:${index}`)}
              <code>{index + 1}. {row.type} · {row.source}/{compactTranscriptValidationID(row.id)}{row.hasContent ? '' : ' · blank'}</code>
            {/each}
          </div>
        </div>
      </details>
    {:else}
      <div class="transcript-validator-summary"><strong>Checking render…</strong></div>
    {/if}
  </div>
{/if}
<!-- svelte-ignore a11y_no_static_element_interactions (wheel/touch/pointer input distinguishes deliberate history reading from programmatic pins) -->
<div
  class="convo"
  data-thread-open-history="stream"
  data-thread-open-view={view === 'all' ? 'dev' : 'enhanced'}
  data-thread-open-warm={fileFeed?.seeded ? 'true' : undefined}
  data-thread-open-state={history?.state === 'error' || fileFeedError ? 'error' : fileFeed?.state.tailed && fileFeed === fileFeedShown ? 'ready' : 'loading'}
  class:initial-position={!initialPositioned && hasTranscript}
  bind:this={scroller}
  onscroll={onScroll}
  onwheel={() => tailNavigation.markScrollInput()}
  ontouchstart={() => tailNavigation.markScrollInput()}
  onpointerdown={() => tailNavigation.markScrollInput()}
>
  {#if liveItems.length === 0 && durableItems.length === 0 && history?.state !== 'loading'}
    <div class="convo-empty">
      {startingAs === 'attaching'
        ? 'Attaching to thread…'
        : startingAs === 'creating'
          ? 'Starting session…'
          : 'Session ready — send a message to start the turn.'}
    </div>
  {/if}

  {#if sessionStatus === 'exited'}
    <!-- The resume handshake runs in the background after the POST returns, so
         its failure has to land here — the pane is all the user is looking at. -->
    <div class="convo-exited-note">
      session exited{store.lastError ? ` — ${store.lastError}` : ''}
    </div>
  {/if}

  {#if history || durableItems.length}
    <!-- Saved rows and ordinary history cursors come from this pane's file stream. -->
    {#if history?.state === 'loading'}
      <div class="history-fallback-note">loading history…</div>
    {:else if history?.state === 'pending'}
      <div class="history-fallback-note">history is still indexing — it will appear in the Thread Explorer</div>
    {:else if history?.state === 'error'}
      <div class="history-fallback-note">history unavailable — {history?.error}</div>
    {/if}
    {#if fileFeedError}
      <div class="history-fallback-note">history unavailable — {fileFeedError}</div>
    {/if}
    <!-- Earlier pages arrive by scrolling up; the note is a progress signal,
         not an affordance. Only a failed page needs a manual retry. -->
    {#if history?.loadingEarlier}
      <div class="history-fallback-note" role="status">loading earlier…</div>
    {:else if history?.earlierError}
      <div class="history-fallback-note" role="status">
        earlier history failed: {history?.earlierError}
        <button class="load-older" onclick={showEarlierHistory}>Retry</button>
      </div>
    {/if}
    {#each durableItems as it (it.id)}
      {@const kind = transcriptItemKind(it)}
      {@const timestamp = formatTranscriptTimestamp(it.createdAt)}
      {@const took = durableTurnDurations.get(it.id)}
      {#if kind === 'user'}
        {@const submittedUTC = view === 'clean' ? senderSubmissionTimestampFromFooter(durableItemText(it)) : ''}
        {@const submittedAt = formatSubmissionTimestamp(submittedUTC)}
        <div class="item userMessage" data-transcript-row data-transcript-key={transcriptValidationKey('durable', it.id)} data-transcript-source="durable" data-transcript-id={it.id} data-transcript-semantic-id={it.id} data-transcript-projected-seq={it.projectedSeq || undefined} data-transcript-projection-key={history?.projection?.key ?? history?.projection?.epoch ?? undefined} data-transcript-type={it.type} data-transcript-category={transcriptItemCategory(it)} data-transcript-role={it.role || 'user'} data-transcript-truncated={it.contentTruncated || undefined}>
          <div class="bubble msg-user-surface msg-user-body" data-transcript-content>{durableItemText(it)}</div>
          {#if timestamp}
            <time class="item-timestamp" datetime={it.createdAt} title={it.createdAt}>{timestamp}</time>
          {/if}
          {#if submittedAt}
            <div class="item-submission-timestamp" title={submittedUTC}>Submitted {submittedAt}</div>
          {/if}
        </div>
      {:else if kind === 'agent'}
        <div class="item agentMessage" data-transcript-row data-transcript-key={transcriptValidationKey('durable', it.id)} data-transcript-source="durable" data-transcript-id={it.id} data-transcript-semantic-id={it.id} data-transcript-projected-seq={it.projectedSeq || undefined} data-transcript-projection-key={history?.projection?.key ?? history?.projection?.epoch ?? undefined} data-transcript-type={it.type} data-transcript-category={transcriptItemCategory(it)} data-transcript-role={it.role || 'assistant'} data-transcript-truncated={it.contentTruncated || undefined} data-transcript-empty={!durableItemText(it) || undefined}>
          <div class="kind">{transcriptItemLabel(it)}</div>
          <div class="bubble msg-agent-surface msg-agent-body md-body" data-transcript-content use:fileReferenceLinks={{ onOpen: openTranscriptFile }}>{@html messageBodyHtml('agent', durableItemText(it) || '(empty message)')}</div>

          {#if timestamp || took}
            <div class="item-timestamp">
              {#if timestamp}<time datetime={it.createdAt} title={it.createdAt}>{timestamp}</time>{/if}
              {#if took}<span class="turn-took" title="Turn duration, as the provider recorded it">({took})</span>{/if}
            </div>
          {/if}
        </div>
      {:else}
        <div class="item aux" data-transcript-row data-transcript-key={transcriptValidationKey('durable', it.id)} data-transcript-source="durable" data-transcript-id={it.id} data-transcript-semantic-id={it.id} data-transcript-type={it.type}>
          <div class="kind">{transcriptItemLabel(it)}</div>
          <div class="body mono" data-transcript-content>{durableItemText(it) || '(no display text)'}</div>
          {#if timestamp}
            <time class="item-timestamp" datetime={it.createdAt} title={it.createdAt}>{timestamp}</time>
          {/if}
        </div>
      {/if}
    {/each}
  {/if}

  {#each visibleItems as it (it.id)}
    {@const timestamp = formatTranscriptTimestamp(it.createdAt)}
    {#if it.type === 'userMessage'}
      {@const dot = promptDot(it)}
	  {@const submittedAt = submissionTimestamp(it)}
      <div
        class="item userMessage"
        class:pending-send={needsPerson(it)}
        data-status={it.status}
        data-client-action-id={it.clientActionId || undefined}
        data-transcript-row
        data-transcript-key={transcriptValidationKey('live', it.id)}
        data-transcript-source="live"
        data-transcript-id={it.id}
        data-transcript-semantic-id={liveSemanticID(it)}
        data-transcript-projected-seq={it.projectedSeq || undefined}
        data-transcript-awaiting-durable={transcriptAuditAwaitingDurable(it) || undefined}
        data-transcript-type={it.type}
        data-transcript-category={liveItemCategory(it)}
        data-transcript-role="user"
      >
        <div
          class="bubble msg-user-surface msg-user-body"
          class:pending-user={dot !== ''}
          data-transcript-content
        >{it.text}{@render pendingUserDot(dot)}</div>
        {#if timestamp}
          <time class="item-timestamp" datetime={it.createdAt} title={it.createdAt}>{timestamp}</time>
        {/if}
		{#if view === 'clean' && submittedAt}
		  <div class="item-submission-timestamp" title={it.senderSubmittedAt}>Submitted {submittedAt}</div>
		{/if}
        {#if it.label}<div class="prompt-label">{it.label}</div>{/if}
        {@render promptActions(it.allowedActions ?? [], () => session.resendPendingSend?.(it.clientActionId), () => session.discardPendingSend?.(it.clientActionId, it.text))}
      </div>
    {:else if it.type === 'agentMessage'}
      <div class="item agentMessage" data-transcript-row data-transcript-key={transcriptValidationKey('live', it.id)} data-transcript-source="live" data-transcript-id={it.id} data-transcript-semantic-id={liveSemanticID(it)} data-transcript-projected-seq={it.projectedSeq || undefined} data-transcript-awaiting-durable={transcriptAuditAwaitingDurable(it) || undefined} data-transcript-type={it.type} data-transcript-category={liveItemCategory(it)} data-transcript-role="assistant">
        <div class="kind">agent{#if !it.done}<span class="live">●</span>{/if}</div>

        <StreamingAgentMessage
          text={it.text}
          smooth={visible && it.id === newestVisibleItemID}
          complete={it.done}
          provider={store.provider || info?.provider || tab?.provider}
          animateInitial={false}
          diagnosticContext={{
            paneId: tab?.id || tab?.catalogId,
            threadId: store.threadId || info?.threadId || tab?.nativeId,
            sessionId: session?.id || info?.id || tab?.sessionId,
            hostedThreadId: store.hostedThreadId || info?.hostedThreadId,
          }}
          onOpen={openTranscriptFile}
          onPresentationUpdate={followStreamPresentation}
        />
        {#if timestamp}
          <div class="item-timestamp">
            <time datetime={it.createdAt} title={it.createdAt}>{timestamp}</time>
          </div>
        {/if}
      </div>
    {:else if it.type === 'toolUse'}
      <div class="item aux" data-transcript-row data-transcript-key={transcriptValidationKey('live', it.id)} data-transcript-source="live" data-transcript-id={it.id} data-transcript-type={it.type}>
        <div class="kind">
          tool: {it.item.name}{#if !it.done}<span class="live">●</span>{/if}
          {#if it.item.isError}<span class="exit-bad">error</span>{/if}
        </div>
        <div class="body mono" data-transcript-content>
          <div class="cmd-line">{it.item.inputJson ?? JSON.stringify(it.item.input ?? {})}</div>
          {#if it.item.output}<div class="cmd-out">{it.item.output}</div>{/if}
        </div>
        {#if timestamp}
          <time class="item-timestamp" datetime={it.createdAt} title={it.createdAt}>{timestamp}</time>
        {/if}
      </div>
    {:else if it.type === 'commandExecution'}
      <div class="item aux" data-transcript-row data-transcript-key={transcriptValidationKey('live', it.id)} data-transcript-source="live" data-transcript-id={it.id} data-transcript-type={it.type}>
        <div class="kind">
          shell{#if !it.done}<span class="live">●</span>{/if}
          {#if it.done && it.item.exitCode !== 0 && it.item.exitCode != null}
            <span class="exit-bad">exit {it.item.exitCode}</span>
          {/if}
        </div>
        <div class="body mono" data-transcript-content>
          <div class="cmd-line">$ {it.item.command}</div>
          {#if it.item.aggregatedOutput}<div class="cmd-out">{it.item.aggregatedOutput}</div>{/if}
        </div>
        {#if timestamp}
          <time class="item-timestamp" datetime={it.createdAt} title={it.createdAt}>{timestamp}</time>
        {/if}
      </div>
    {:else}
      <div class="item aux" data-transcript-row data-transcript-key={transcriptValidationKey('live', it.id)} data-transcript-source="live" data-transcript-id={it.id} data-transcript-type={it.type}>
        <div class="kind">{AUX_LABELS[it.type] ?? it.type}{#if !it.done}<span class="live">●</span>{/if}</div>
        <div class="body mono" data-transcript-content>{auxBody(it) || '(empty)'}</div>
        {#if timestamp}
          <time class="item-timestamp" datetime={it.createdAt} title={it.createdAt}>{timestamp}</time>
        {/if}
      </div>
    {/if}
    {#if view === 'all'}
      <details class="item-raw">
        <summary>{it.type} · {it.id}</summary>
        <pre>{JSON.stringify(it.item, null, 2)}</pre>
      </details>
    {/if}
  {/each}

  {#each pendingInteractions as interaction (interaction.id)}
    <!-- The live edge: below everything the transcript has, above the composer.
         Outside the item loop on purpose — a pending request is runtime state,
         not a transcript item, and the answered exchange enters the record
         through the durable projection instead. -->
    <InteractionCard {interaction} onAnswer={(id, body) => session.answerInteraction(id, body)} />
  {/each}

  {#if offlineStatus}
    <div class="thinking">
      <div class="thinking-head" aria-live="polite"><span class="thinking-label">{offlineStatus}</span></div>
    </div>
  {:else if thinking}
    <!-- Ephemeral by construction: this lives outside the item loop, so there
         is nothing to clean up when the answer arrives — the surface simply
         stops matching. The hand-off is deliberately instant; fading it out
         would leave stale reasoning under the answer that replaced it. -->
    <div class="thinking">
      <div class="thinking-head" aria-live="polite">
        {#if compactingSinceMs}
          <span class="thinking-label">Compacting…</span>
        {:else}
          <span class="thinking-label">{thinkingActivity?.label ?? 'Working'}</span>
          {#if thinkingActivity?.detail}<code class="thinking-detail">{thinkingActivity.detail}</code>{/if}
        {/if}
        {#if waitingOnOperator}
          <span class="thinking-elapsed waiting" title="Waiting for you to answer">waiting</span>
        {:else if liveElapsed}
          <span class="thinking-elapsed">{liveElapsed}</span>
        {/if}
      </div>
      {#if thinkingWords.length}
        <!-- aria-hidden: this is the model's process, not its answer, and it
             streams too fast to narrate. The head above announces the state. -->
        <div class="thinking-stream" aria-hidden="true">
          <div class="thinking-stream-text">
            <!-- The separator is part of the expression, not template
                 whitespace: Svelte trims the latter, which ran every word
                 together into one string. -->
            {#each thinkingWords as w (w.key)}<span class="thinking-word">{w.word + ' '}</span>{/each}
          </div>
        </div>
      {/if}
    </div>
  {/if}
</div>

<!-- The selects themselves are unchanged — only where they live is. They set
     per-turn overrides on codex and switch the live session on claude, and both
     stay usable mid-turn: changing them while one runs is how you set up the
     retry. -->
{#snippet turnControls()}
  {#if isCodex}
    <div class="turn-controls">
      <label class="turn-control">
        <span class="turn-control-label">Model</span>
        <select
          value={turnModel}
          onchange={(e) => chooseModel(e.currentTarget.value)}
          aria-label="model for the next turn"
          disabled={metadataControlsDisabled}
        >
          <option value="">{threadModel ? `Thread default (${threadModel})` : 'Thread default'}</option>
          {#each codexModels as m (m.id ?? m.model)}
            <option value={m.model}>{m.displayName || m.model}</option>
          {/each}
          {#if turnModel && !codexModels.some((m) => m.model === turnModel)}
            <!-- An override the catalog no longer advertises still has to be
                 shown as selected, or the control would silently misreport. -->
            <option value={turnModel}>{turnModel}</option>
          {/if}
        </select>
      </label>
      <label class="turn-control">
        <span class="turn-control-label">Effort</span>
        <select
          value={turnEffort}
          onchange={(e) => !metadataControlsDisabled && session.setTurnOptions?.({ effort: e.currentTarget.value })}
          aria-label="reasoning effort for the next turn"
          disabled={metadataControlsDisabled}
        >
          <option value="">Model default</option>
          {#each effortChoices as effort (effort)}
            <option value={effort}>{effort}</option>
          {/each}
          {#if turnEffort && !effortChoices.includes(turnEffort)}
            <option value={turnEffort}>{turnEffort}</option>
          {/if}
        </select>
      </label>
      <label class="turn-control">
        <span class="turn-control-label">Speed</span>
        <select
          value={controlServiceTier}
          onchange={(e) => !metadataControlsDisabled && session.setTurnOptions?.({ serviceTier: e.currentTarget.value })}
          aria-label="speed for this session"
          disabled={metadataControlsDisabled}
        >
		  <option value="" disabled>Pending / unknown</option>
          <option value="default">Standard</option>
          {#each speedChoices as tier (tier.id)}
            <option value={tier.id}>{tier.name || tier.id}</option>
          {/each}
          {#if controlServiceTier && controlServiceTier !== 'default' && !speedChoices.some((tier) => tier.id === controlServiceTier)}
            <option value={controlServiceTier} disabled>{controlServiceTier} (unavailable)</option>
          {/if}
        </select>
      </label>
    </div>
  {:else if isClaude && claudeModels.length}
    <!-- Claude has one control here, not two: `set_model` switches the live
         session, while effort is fixed at spawn with no control-channel
         equivalent — so offering an effort select would be a control that
         cannot do anything. -->
    <div class="turn-controls">
      <label class="turn-control">
        <span class="turn-control-label">Model</span>
        <select
          value={turnModel}
          onchange={(e) => !metadataControlsDisabled && session.setTurnOptions?.({ model: e.currentTarget.value })}
          aria-label="model for this session"
          disabled={metadataControlsDisabled}
        >
          <option value="">{threadModel ? `Session default (${threadModel})` : 'Session default'}</option>
          {#each claudeModels as m (m.value)}
            <option value={m.value}>{m.label}</option>
          {/each}
          {#if turnModel && !claudeModels.some((m) => m.value === turnModel)}
            <option value={turnModel}>{turnModel}</option>
          {/if}
        </select>
      </label>
    </div>
  {/if}
{/snippet}

<div class="composer-wrap">
  {#if store.sendError && sessionStatus !== 'exited'}
    <!-- The prompt's row is the action surface. This is only a quiet notice:
         a refused send restores its text, one Nano never acknowledged stays
         below with Resend and Discard, and a Resend or Discard Nano refused
         says why in Nano's own words. -->
    <div class="send-notice" role="status">
      <span class="send-error-text">
        {#if store.sendError.refusal}
          {store.sendError.reason}
        {:else}
          {store.sendError.unsent ? 'Not delivered yet' : 'Message not sent'} — {store.sendError.reason}.
          {store.sendError.fromQueue
            ? 'It is still in the queue.'
            : store.sendError.unsent
              ? 'It is kept below and sent again automatically until Nano has it.'
              : 'Your text was restored below.'}
        {/if}
      </span>
    </div>
  {/if}
  <!-- A send Nano never acknowledged has no transcript row: it is kept here,
       across reloads, until Nano has it or Discard returns it to the composer.
       A thread's sends go one at a time, oldest first; the front one posts
       itself again until Nano answers, and Resend posts it now. Discard is
       the one way to stop a send. The count line is the length of this same
       list. -->
  {#if unsentRows.length}
    <button
      type="button"
      class="unsent-count"
      aria-expanded={unsentOpen}
      onclick={() => (unsentToggled = !unsentOpen)}
    >{unsentRows.length} not delivered{unsentSummary}</button>
  {/if}
  {#if unsentOpen}
    {#each unsentRows as unsent (unsent.clientActionId)}
      <div class="unsent-send" role="group" aria-label="Message Nano has not acknowledged">
        <span class="unsent-send-label">{unsentLabel(unsent)}</span>
        <div class="unsent-send-text">{unsent.text}</div>
        <div class="pending-send-actions">
          {#if !unsent.inFlight && (unsent.stopped || (!unsent.behind && unsent.posted && unsent.retry))}
            <button type="button" class="pending-send-action" onclick={() => session.resendPendingSend?.(unsent.clientActionId)}>Resend</button>
          {/if}
          <button type="button" class="pending-send-action" onclick={() => session.discardPendingSend?.(unsent.clientActionId)}>Discard</button>
        </div>
      </div>
    {/each}
  {/if}
  <!-- Small and muted, directly above the text entry: the session's standing
       facts, which are read at a glance and never acted on. Everything that IS
       acted on sits behind the ⋯ at the end of it. -->
  <div class="info-bar">
	<InfoBarFacts fields={barFields} />

    <div class="thread-menu" bind:this={menuRoot}>
      <button
        class="thread-menu-btn"
        aria-haspopup="menu"
        aria-expanded={menuOpen}
        aria-label="thread menu"
        title="Thread actions"
        onclick={toggleMenu}>⋯</button
      >
      {#if menuOpen}
        <div class="thread-menu-pop" role="menu">
          {#if renaming}
            <div class="thread-menu-section">
              <label class="thread-menu-label" for="session-rename">Session name</label>
              <input
                id="session-rename"
                class="xname-input"
                aria-label="Rename session"
                bind:this={renameInput}
                bind:value={renameValue}
                disabled={renameSaving}
                onkeydown={onRenameKeydown}
              />
              {#if renameError}<div class="thread-menu-error">{renameError}</div>{/if}
              <div class="thread-menu-actions">
                <button class="thread-menu-item" disabled={renameSaving} onclick={() => void saveRename()}>Save name</button>
                <button class="thread-menu-item" disabled={renameSaving} onclick={cancelRename}>Cancel</button>
              </div>
            </div>
          {:else}
            <button class="thread-menu-item" onclick={beginRename}>Rename session</button>
          {/if}

          {#if hasModelControl}
            <button
              class="thread-menu-item"
              aria-expanded={menuTurnControls}
              onclick={toggleTurnControls}
              >{isCodex ? 'Change model / effort / speed' : 'Change model'}</button
            >
            {#if menuTurnControls}
              {@render turnControls()}
            {/if}
          {/if}

          {#if viewport.mobile}
            <div class="thread-menu-section">
              <div class="thread-menu-label">Transcript</div>
              {@render viewToggle()}
              {@render viewHint()}
              {@render transcriptAuditControls()}
              {@render threadDetailsToggle()}
              {@render detachToggle()}
              {#if detachState?.error}<div class="thread-menu-error" role="alert">{detachState.error}</div>{/if}
            </div>
          {/if}
        </div>
      {/if}
    </div>
  </div>

  <div class="composer">
    <textarea
      class="composer-input"
      bind:this={textareaEl}
      rows="1"
      placeholder={composerPlaceholder(sessionStatus, store.turnActive, spawnMode, joinsTurn)}
      autocomplete="off"
      enterkeyhint="send"
      bind:value={draft}
      onkeydown={onKeydown}
      onbeforeinput={onBeforeInput}
      onkeyup={onKeyup}
      oninput={onInput}
      onpaste={scheduleResize}
      class:send-blocked={sendBlocked}
    ></textarea>
    {#if liveTurnActive}
      <!-- Both affordances, because both actions apply mid-turn: Queue lines
           the message up behind the running turn, Stop ends that turn. Stop is
           always present so it is in the same place every time; Queue appears
           only when there is something to queue. -->
      {#if draft.trim() && joinsTurn}
        <button aria-label="Queue for the next turn" title="Queue for the next turn" onclick={() => submit({ nextTurn: true })} disabled={composerDisabled}>Queue</button>
        <button class="submit-icon" aria-label="Send" title="Add to the running turn" onclick={submit} disabled={composerDisabled}>
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <line x1="12" y1="19" x2="12" y2="5"></line>
            <polyline points="5 12 12 5 19 12"></polyline>
          </svg>
        </button>
      {:else if draft.trim()}
        <button class="submit-icon" aria-label="Queue" title="Queue the next message" onclick={submit} disabled={composerDisabled}>
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <line x1="12" y1="19" x2="12" y2="5"></line>
            <polyline points="5 12 12 5 19 12"></polyline>
          </svg>
        </button>
      {/if}
      {#if statusLive}
        <button class="stop" onclick={session.interruptTurn} disabled={store.interrupting}>
          {store.interrupting ? 'Stopping' : 'Stop'}
        </button>
      {/if}
    {:else}
      <button class="submit-icon" aria-label={queuePrompt ? 'Queue' : 'Send'} title={queuePrompt ? 'Queue the next message' : 'Send message'} onclick={submit} disabled={!draft.trim() || composerDisabled}>
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <line x1="12" y1="19" x2="12" y2="5"></line>
          <polyline points="5 12 12 5 19 12"></polyline>
        </svg>
      </button>
    {/if}
  </div>
</div>
