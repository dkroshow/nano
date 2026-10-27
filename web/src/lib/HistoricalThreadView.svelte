<script>
  import { onDestroy, tick } from 'svelte'
  import {
    formatTranscriptTimestamp,
    formatSubmissionTimestamp,
    senderSubmissionTimestampFromFooter,
    itemText,
    shouldAutoLoadEarlier,
    transcriptItemKind,
    transcriptItemLabel,
    turnDurationsByFinalItem,
  } from '../transcript_view.js'
  import { messageBodyHtml } from './message_body.js'
  import InfoBarFacts from './InfoBarFacts.svelte'
  import FeedbackButton from './FeedbackButton.svelte'
  import { createTailNavigation } from './tail_navigation.js'
  import { openFileTab } from '../panes.svelte.js'
  import { fileReferenceLinks } from './file_references.js'

  let {
    // The tab-owned durable-history channel (ThreadPaneView creates, loads and
    // closes it). Sharing it across the attach/detach swap is what lets this
    // view come back with its items already in memory.
    history,
    tab = null,
    paneId = null,
    manualScrollAwayAt = 0,
    onManualScrollAwayAt = null,
    // Recorded session facts from the thread's catalog row (historicalBarFields):
    // the branch it ran on, the last model/effort it was seen with. No ⋯ menu —
    // nothing here is editable, and Resume already has its own affordance.
    barFields = [],
    visible = true,
    onResume = null,
    resumeBusy = false,
    resumeError = '',
    // Archived and subagent threads keep the affordance visible but off, with
    // the gate's reason attached — the same posture the detail view takes.
    resumeDisabled = false,
    resumeNote = '',
    watchingSubagent = false,
    subagentWorking = false,
  } = $props()
  const transcript = $derived(history.state)

  function feedbackContext() {
    return {
      threadId: tab?.nativeId || transcript.threadId || '',
      provider: tab?.provider || '',
      view: 'enhanced',
      paneId: String(tab?.id || paneId || ''),
      pane: { working: subagentWorking, pendingPrompts: 0, lastSavedRowId: transcript.items.at(-1)?.id || '' },
    }
  }

  function openTranscriptFile(reference) {
    openFileTab(reference, {
      cwd: tab?.cwd || '',
      catalogThreadId: tab?.catalogId || transcript.threadId || '',
      sessionId: tab?.sessionId || '',
    }, paneId)
  }
  // Turn durations hang off the item that ended each turn — native evidence
  // only, and absent for the turns whose provider never recorded one.
  const turnDurations = $derived(turnDurationsByFinalItem(transcript.turns))
  let scroller = $state(null)
  const tailNavigation = createTailNavigation(() => scroller)
  onDestroy(tailNavigation.destroy)
  let placed = $state(false)
  // Same model as Conversation: stick-vs-preserve is decided from the position
  // BEFORE the DOM grows, tracked on every scroll event. A fresh pane counts
  // as at-bottom so the initial pin and any tail merge land on the newest item.
  let atBottom = true

  function onScroll() {
    atBottom = tailNavigation.observeScroll()
    if (transcript.items.length > 0) placed = true
    onManualScrollAwayAt?.(tailNavigation.manualScrollHoldAt())
    // Reading upward pages the thread in, the way the Explorer's transcript
    // already does. A failed page stops the loop so a broken cursor cannot
    // retry on every scroll event; the error note carries the manual retry.
    if (shouldAutoLoadEarlier({
      scrollTop: scroller?.scrollTop,
      hasOlder: hasEarlier,
      loadingEarlier: transcript.loadingEarlier,
      earlierError: transcript.earlierError,
    })) void earlier()
  }

  $effect(() => {
    if (visible) history.revalidateTail()
  })

  $effect(() => {
    const count = transcript.items.length
    if (!visible || !scroller || placed || count === 0) return
    void tick().then(() => {
      if (!scroller) return
      tailNavigation.settleToBottom()
      // Reveal after the synchronous pin: absent frames, a replacement settle,
      // or wheel input can prevent the settle callback from running.
      placed = true
    })
  })

  // Follow the tail while pinned: the post-detach revalidation merges the
  // turns that just ran, and without this the reader is left stranded above
  // them. "Show earlier" prepends (tail identity unchanged), so it never
  // fires this; a reader scrolled up is never yanked.
  $effect(() => {
    void transcript.items.at(-1)?.id
    if (!placed || !visible || !scroller) return
    void tick().then(() => {
      if (scroller && atBottom) tailNavigation.settleToBottom()
    })
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
    tailNavigation.settleToBottom()
  })

  async function earlier() {
    const height = scroller?.scrollHeight ?? 0
    const top = scroller?.scrollTop ?? 0
    await history.loadEarlier()
    await tick()
    if (scroller) tailNavigation.pinInstant(top + scroller.scrollHeight - height)
  }

  const hasEarlier = $derived(
    Boolean(transcript.page?.hasMoreBefore && (transcript.page?.beforeCursor ?? transcript.page?.nextBeforeCursor)),
  )
</script>

<div class="pane-feedback-float" class:hidden={!visible}>
  <FeedbackButton context={feedbackContext} />
</div>

<!-- Resume is the primary action on a historical thread, so it floats over the
     transcript instead of hiding in the status strip. Positioned against
     .thread-pane-content, which owns the pane's content box. -->
{#if onResume}
  <div class="pane-resume-float" class:hidden={!visible}>
    <button
      class="pane-resume-btn"
      disabled={resumeBusy || resumeDisabled}
      title={resumeNote || null}
      onclick={onResume}
    >
      {resumeBusy ? 'Resuming…' : 'Resume thread'}
    </button>
    {#if resumeNote}<div class="pane-resume-note">{resumeNote}</div>{/if}
    {#if resumeError}<div class="pane-resume-error">{resumeError}</div>{/if}
  </div>
{/if}

<!-- svelte-ignore a11y_no_static_element_interactions (wheel/touch/pointer input distinguishes deliberate history reading from programmatic pins) -->
<div
  class="pane-transcript"
  data-thread-open-history="catalog"
  data-thread-open-view="enhanced"
  data-thread-open-state={['ready', 'empty', 'retained', 'degraded'].includes(transcript.state) ? 'ready' : transcript.state === 'error' ? 'error' : 'loading'}
  class:hidden={!visible}
  class:initial-position={!placed && transcript.items.length > 0}
  bind:this={scroller}
  onscroll={onScroll}
  onwheel={() => tailNavigation.markScrollInput()}
  ontouchstart={() => tailNavigation.markScrollInput()}
  onpointerdown={() => tailNavigation.markScrollInput()}
>
  {#if transcript.state === 'loading' || transcript.state === 'idle'}
    <div class="pane-transcript-empty">Loading thread history…</div>
  {:else if transcript.state === 'error'}
    <div class="pane-transcript-empty error">{transcript.error}</div>
  {:else if transcript.items.length === 0}
    <div class="pane-transcript-empty">No transcript items.</div>
  {/if}

  <!-- Earlier pages arrive by scrolling up; the note is a progress signal, not
       an affordance. Only a failed page needs a manual retry. -->
  {#if transcript.loadingEarlier}
    <div class="history-fallback-note" role="status">loading earlier…</div>
  {:else if transcript.earlierError}
    <div class="history-fallback-note" role="status">
      earlier history failed: {transcript.earlierError}
      <button class="load-older" onclick={earlier}>Retry</button>
    </div>
  {/if}

  {#each transcript.items as item (item.id)}
    {@const kind = transcriptItemKind(item)}
    {@const timestamp = formatTranscriptTimestamp(item.createdAt)}
    {@const took = turnDurations.get(item.id)}
    {#if kind === 'user'}
      {@const submittedUTC = senderSubmissionTimestampFromFooter(itemText(item))}
      {@const submittedAt = formatSubmissionTimestamp(submittedUTC)}
      <div class="item userMessage">
        <div class="bubble msg-user-surface msg-user-body">{itemText(item)}</div>
        {#if timestamp}
          <time class="item-timestamp" datetime={item.createdAt} title={item.createdAt}>{timestamp}</time>
        {/if}
        {#if submittedAt}
          <div class="item-submission-timestamp" title={submittedUTC}>Submitted {submittedAt}</div>
        {/if}
      </div>
    {:else if kind === 'agent'}
      <div class="item agentMessage">
        <div class="kind">{transcriptItemLabel(item)}</div>
        <div class="bubble msg-agent-surface msg-agent-body md-body" use:fileReferenceLinks={{ onOpen: openTranscriptFile }}>
          {@html messageBodyHtml('agent', itemText(item) || '(empty message)')}
        </div>

        {#if timestamp || took}
          <div class="item-timestamp">
            {#if timestamp}<time datetime={item.createdAt} title={item.createdAt}>{timestamp}</time>{/if}
            {#if took}<span class="turn-took" title="Turn duration, as the provider recorded it">({took})</span>{/if}
          </div>
        {/if}
      </div>
    {:else}
      <div class="item aux">
        <div class="kind">{transcriptItemLabel(item)}</div>
        <div class="body mono">{itemText(item) || '(no display text)'}</div>
        {#if timestamp}
          <time class="item-timestamp" datetime={item.createdAt} title={item.createdAt}>{timestamp}</time>
        {/if}
      </div>
    {/if}
  {/each}
</div>

{#if barFields.length}
  <div class="info-bar historical" class:hidden={!visible}>
    <InfoBarFacts fields={barFields} />
  </div>
{/if}
<div class="pane-readonly" class:hidden={!visible}>
  <span>{watchingSubagent
    ? subagentWorking
      ? 'Watching worker · live updates'
      : 'Worker transcript · read only'
    : 'Read only · historical thread'}</span>
</div>
