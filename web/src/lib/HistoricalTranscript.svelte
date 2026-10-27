<script>
	import { onMount, tick } from 'svelte'
  import {
    loadEarlier,
    loadTranscript,
		loadTranscriptBody,
		scheduleTranscriptBody,
    loadTranscriptEvidence,
		nudgeTranscriptTail,
    setTranscriptCategoryVisible,
    setTranscriptView,
    showAllTranscriptCategories,
    transcript,
  } from '../transcript_store.svelte.js'
  import {
    TRANSCRIPT_CATEGORIES,
    defaultHiddenCategories,
  } from '../transcript_query.js'
  import {
    evidenceDisplay,
    evidenceProvenance,
    consumeTranscriptFilterEscape,
    formatTranscriptTimestamp,
    itemText,
    normalizedItemMetadata,
    shouldAutoLoadEarlier,
    transcriptItemCategory,
    transcriptItemKind,
    transcriptItemLabel,
  } from '../transcript_view.js'
  import { messageBodyHtml } from './message_body.js'
  import { openFileTab } from '../panes.svelte.js'
  import { fileReferenceLinks } from './file_references.js'

  function openTranscriptFile(reference) {
    openFileTab(reference, { catalogThreadId: transcript.threadId || '' })
  }

  let scroller = $state(null)
  let filterWrap = $state(null)
  let filtersOpen = $state(false)
  let initialScrollKey = $state(null)

	// Live tail follow (Slice G). Sticking to the bottom must be decided from the
	// scroll position *before* new items grow the DOM, so it is tracked on every
	// scroll rather than measured inside the post-growth effect. tailPrev* remember
	// the last rendered tail so the effect only reacts to genuine forward growth
	// (never to a paged-in older page or a profile switch).
	const TAIL_STICK_PX = 72
	let atBottom = true
	let tailPrevSeq = -1
	let tailPrevKey = null
	let bodyObserver = null

	function autoBody(node, item) {
		let disposed = false
		let current = item
		function observe() {
			void tick().then(() => {
				if (disposed || !current?.contentRef || !scroller || typeof IntersectionObserver === 'undefined') return
			bodyObserver ??= new IntersectionObserver((entries) => {
				for (const entry of entries) if (entry.isIntersecting) scheduleTranscriptBody(entry.target.dataset.itemId)
			}, { root: scroller, rootMargin: '100% 0px' })
			bodyObserver.observe(node)
			})
		}
		observe()
		return {
			update(next) { bodyObserver?.unobserve(node); current = next; observe() },
			destroy() { disposed = true; bodyObserver?.unobserve(node) },
		}
	}

	// When a hydrated message above the viewport grows, compensate by its
	// measured top-edge delta. Messages in view are intentionally allowed to
	// expand in place so the reader sees the completion.
	function anchorHydration(node, item) {
		let observer; let disposed = false
		let current = item
		let observationVersion = 0
		function observe() {
			const version = ++observationVersion
			observer?.disconnect()
			void tick().then(() => {
				if (disposed || version !== observationVersion || !current?.contentRef || !scroller || typeof ResizeObserver === 'undefined') return
			let priorHeight = node.getBoundingClientRect().height
			let wasAbove = node.getBoundingClientRect().bottom <= scroller.getBoundingClientRect().top
			observer = new ResizeObserver(() => {
				const rect = node.getBoundingClientRect()
				if (wasAbove) scroller.scrollTop += rect.height - priorHeight
				priorHeight = rect.height; wasAbove = rect.bottom <= scroller.getBoundingClientRect().top
			})
			observer.observe(node)
			})
		}
		observe()
		return {
			update(next) { current = next; observe() },
			destroy() { disposed = true; observer?.disconnect() },
		}
	}

  const hiddenSet = $derived(new Set(transcript.hiddenCategories))
  const scrollProfileKey = $derived(
    `${transcript.threadId ?? ''}\u0000${transcript.view}\u0000${transcript.hiddenCategories.join(',')}`,
  )
  const filtersAreDefault = $derived(
    transcript.hiddenCategories.join(',') === defaultHiddenCategories(transcript.view).join(','),
  )

  function hasOlder() {
    return Boolean(transcript.page?.hasMoreBefore && (transcript.page?.beforeCursor ?? transcript.page?.nextBeforeCursor))
  }

  async function earlier() {
    if (!scroller) return loadEarlier()
    const profileKey = scrollProfileKey
    const height = scroller.scrollHeight
    const top = scroller.scrollTop
    await loadEarlier()
    await tick()
    if (!scroller || scrollProfileKey !== profileKey) return
    scroller.scrollTop = top + (scroller.scrollHeight - height)
  }

  function nearBottom() {
		return Boolean(scroller) && scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < TAIL_STICK_PX
	}

  function transcriptScroll() {
		atBottom = nearBottom()
		if (atBottom) transcript.newerAvailable = false
    if (!shouldAutoLoadEarlier({
      scrollTop: scroller?.scrollTop,
      hasOlder: hasOlder(),
      loadingEarlier: transcript.loadingEarlier,
      earlierError: transcript.earlierError,
    })) return
    void earlier()
  }

	async function jumpToNewest() {
		await tick()
		if (scroller) scroller.scrollTop = scroller.scrollHeight
		transcript.newerAvailable = false
	}

	onMount(() => {
		// EventSource can miss bumps while the tab is hidden; re-sync the tail on
		// refocus. The SSE nudge (explorer.svelte.js) drives all other follow.
		const visible = () => { if (document.visibilityState === 'visible') void nudgeTranscriptTail() }
		document.addEventListener('visibilitychange', visible)
		return () => document.removeEventListener('visibilitychange', visible)
	})

	onMount(() => () => bodyObserver?.disconnect())

	// Follow the tail as the store appends live items. atBottom is the pre-growth
	// position (tracked on scroll), so a reader parked in history keeps their exact
	// place — an append below the fold never moves scrollTop — while a reader at the
	// bottom stays pinned to newest. A profile switch resets the baseline so the
	// initial-load scroll (below) owns the first paint.
	$effect(() => {
		const key = scrollProfileKey
		const seq = transcript.items.at(-1)?.projectedSeq ?? -1
		if (key !== tailPrevKey) {
			tailPrevKey = key
			tailPrevSeq = seq
			return
		}
		if (seq <= tailPrevSeq) return
		tailPrevSeq = seq
		const stick = atBottom
		void tick().then(() => {
			if (!scroller || scrollProfileKey !== key) return
			if (stick) {
				scroller.scrollTop = scroller.scrollHeight
				transcript.newerAvailable = false
			} else {
				transcript.newerAvailable = true
			}
		})
	})

  $effect(() => {
    const key = scrollProfileKey
    const ready = ['ready', 'retained', 'degraded'].includes(transcript.state)
    const itemCount = transcript.items.length
    if (!scroller || !key || !ready || itemCount === 0 || initialScrollKey === key) return
    initialScrollKey = key
    void tick().then(() => {
      if (scroller && scrollProfileKey === key) scroller.scrollTop = scroller.scrollHeight
    })
  })

  function chooseView(view) {
    if (view !== transcript.view) setTranscriptView(view)
  }

  function windowPointerDown(event) {
    if (filtersOpen && filterWrap && !filterWrap.contains(event.target)) filtersOpen = false
  }

  function windowKeydown(event) {
    if (consumeTranscriptFilterEscape(event, filtersOpen, () => (filtersOpen = false))) {
      filterWrap?.querySelector('.history-filter-button')?.focus()
    }
  }

  function evidenceToggle(event, itemId) {
    if (!event.currentTarget.open) return
    for (const detail of scroller?.querySelectorAll('.history-native[open]') ?? []) {
      if (detail !== event.currentTarget) detail.open = false
    }
    loadTranscriptEvidence(itemId)
  }

	function bodyToggle(event, itemId) {
		if (event.currentTarget.open) loadTranscriptBody(itemId)
	}

  function detailText(detail) {
    if (typeof detail === 'string') return detail
    return JSON.stringify(detail, null, 2)
  }

	function turnFor(item) {
		return transcript.turns.find((turn) => turn.id === item.turnId)
	}
</script>

<svelte:window onpointerdown={windowPointerDown} onkeydowncapture={windowKeydown} />

<section class="history-pane" aria-label="Historical transcript">
  <div class="history-head">
    <div class="history-head-main">
      <div>
        <div class="history-title">Historical transcript</div>
        <div class="history-readonly"><span aria-hidden="true">●</span> Read only</div>
      </div>
      <div class="history-head-actions">
        {#if transcript.projection?.itemCount != null}
					<span class="history-count">{transcript.projection.itemCount} projected · {transcript.items.length} loaded</span>
        {/if}
        <div class="history-mode" role="group" aria-label="Transcript mode">
          <button
            class:on={transcript.view === 'enhanced'}
            aria-pressed={transcript.view === 'enhanced'}
            data-testid="transcript-mode-enhanced"
            onclick={() => chooseView('enhanced')}
          >Enhanced</button>
          <button
            class:on={transcript.view === 'dev'}
            aria-pressed={transcript.view === 'dev'}
            data-testid="transcript-mode-dev"
            onclick={() => chooseView('dev')}
          >Dev</button>
        </div>
        <div class="history-filter-wrap" bind:this={filterWrap}>
          <button
            class="history-filter-button"
            class:active={!filtersAreDefault}
            aria-expanded={filtersOpen}
            aria-controls="history-filter-panel"
            onclick={() => (filtersOpen = !filtersOpen)}
          >Filters{transcript.hiddenCategories.length ? ` · ${transcript.hiddenCategories.length} hidden` : ''}</button>
          {#if filtersOpen}
            <div id="history-filter-panel" class="history-filter-panel">
              <div class="history-filter-title">Visible categories</div>
              {#each TRANSCRIPT_CATEGORIES as category (category.key)}
                <label class="history-filter-row">
                  <input
                    type="checkbox"
                    checked={!hiddenSet.has(category.key)}
                    onchange={(event) => setTranscriptCategoryVisible(category.key, event.currentTarget.checked)}
                  />
                  <span>{category.label}</span>
                </label>
              {/each}
              <div class="history-filter-footer">
                <button onclick={showAllTranscriptCategories}>Show all</button>
                <button disabled={filtersAreDefault} onclick={() => setTranscriptView(transcript.view)}>Reset</button>
              </div>
            </div>
          {/if}
        </div>
      </div>
    </div>
    <div class="history-mode-note">
      {transcript.view === 'dev'
        ? 'Every selected event · normalized metadata and native evidence on demand'
        : 'Clean conversation · technical categories hidden by default'}
    </div>
		{#if transcript.state === 'retained' || transcript.projection?.sourceStatus === 'source_missing'}
			<div class="history-projection-notice" role="status">Native source missing · showing Nano's retained projection</div>
		{:else if transcript.state === 'degraded'}
			<div class="history-projection-notice" role="status">
				Indexing needs attention · showing the last safe projection{transcript.projection?.errorSummary ? ` · ${transcript.projection.errorSummary}` : ''}
			</div>
		{/if}
  </div>

	<!-- A scroll region must be keyboard-focusable so Page Up/Down reach it. -->
	<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
  <div
    class="history-scroll"
    bind:this={scroller}
    data-testid="historical-transcript-scroll"
    data-thread-id={transcript.threadId ?? ''}
    data-transcript-view={transcript.view}
    data-transcript-state={transcript.state}
    data-has-more-before={hasOlder() ? 'true' : 'false'}
    data-loading-earlier={transcript.loadingEarlier ? 'true' : 'false'}
		tabindex="0"
		role="region"
		aria-label="Historical transcript messages"
    onscroll={transcriptScroll}
  >
    {#if transcript.state === 'loading'}
      <div class="history-skeleton" aria-label="Loading transcript">
        <span></span><span></span><span></span>
      </div>
    {:else if transcript.state === 'error'}
      <div class="history-state" role="status">
        <strong>Transcript unavailable</strong>
        <span>{transcript.error}</span>
        <button class="load-older" onclick={() => loadTranscript(transcript.threadId, { force: true })}>Retry</button>
      </div>
    {:else if transcript.state === 'pending'}
      <div class="history-state" role="status"><strong>Indexing transcript</strong><span>History will appear after the next background projection.</span></div>
    {:else if transcript.state === 'degraded' && transcript.items.length === 0}
      <div class="history-state" role="status"><strong>Transcript indexing needs attention</strong><span>The last safe projection remains available when possible.</span></div>
    {:else if transcript.state === 'empty'}
      <div class="history-state" role="status"><strong>No items in this view</strong><span>Try enabling more transcript categories.</span></div>
    {:else}
      {#if transcript.loadingEarlier}
        <div class="history-earlier" role="status">Loading earlier…</div>
      {/if}
      {#if transcript.earlierError}
        <div class="history-page-error" role="status">
          <span>Earlier history failed: {transcript.earlierError}</span>
          <button class="load-older" onclick={earlier}>Retry</button>
        </div>
      {/if}
      <div class="history-items">
        {#each transcript.items as item (item.id)}
          {@const kind = transcriptItemKind(item)}
          {@const category = transcriptItemCategory(item)}
          {@const timestamp = formatTranscriptTimestamp(item.createdAt)}
          {@const evidence = transcript.evidence[item.id]}
					{@const body = transcript.body[item.id]}
					{@const displayText = body?.state === 'ready' ? body.data.text : itemText(item)}
					{@const turn = turnFor(item)}
          <article
            class="history-item history-{kind} history-category-{category}"
            class:is-final={item.isFinalAnswer || item.phase === 'final_answer'}
            data-testid="transcript-item"
            data-item-id={item.id}
            data-projected-seq={item.projectedSeq ?? ''}
            data-category={category}
						use:autoBody={kind === 'user' || kind === 'agent' ? item : null}
						use:anchorHydration={kind === 'user' || kind === 'agent' ? item : null}
          >
            <div class="history-kind">{transcriptItemLabel(item)}</div>
            <div
              class="history-card"
              class:msg-user-surface={kind === 'user'}
              class:msg-agent-surface={kind === 'agent'}
            >
              <!-- Same display engine as the live conversation: the shared
                   messageBodyHtml dispatch (agent → md() markdown, user →
                   escaped plain text) plus the shared msg-*/.md-body CSS. -->
              {#if kind === 'agent'}
                <div class="history-bubble msg-agent-body md-body" use:fileReferenceLinks={{ onOpen: openTranscriptFile }}>{@html messageBodyHtml('agent', displayText || '(empty message)')}</div>
              {:else if kind === 'user'}
                <div class="history-bubble msg-user-body">{displayText}</div>
              {:else}
                <pre class="history-aux-body">{itemText(item) || '(no display text)'}</pre>
              {/if}

						{#if (kind === 'user' || kind === 'agent') && item.contentRef && body?.state !== 'ready'}
							<div class="history-body-preview" role="status">{body?.state === 'error' ? 'Preview remains because the full message failed to load.' : 'Preview only · retained message content is loading automatically.'}</div>
							{#if body?.state === 'error'}
								<div class="history-evidence-error">{body.error}</div><button class="load-older" onclick={() => loadTranscriptBody(item.id)}>Retry full message</button>
							{/if}
						{:else if (kind !== 'user' && kind !== 'agent') && (item.contentTruncated || item.contentRef)}
							<details class="history-full-body" ontoggle={(event) => bodyToggle(event, item.id)}>
								<summary>{kind === 'user' || kind === 'agent' ? 'Message' : 'Output'} truncated · {item.originalBytes || 'larger body'} bytes retained · Show full content</summary>
								{#if !body || body.state === 'loading'}
									<div class="history-evidence-state">Loading retained content…</div>
								{:else if body.state === 'error'}
									<div class="history-evidence-error">{body.error}</div>
									<button class="load-older" onclick={() => loadTranscriptBody(item.id)}>Retry</button>
								{:else}
									{#if kind === 'agent' && body.data.text}
										<!-- Hydrated agent bodies go through the same md() path as the bubble. -->
										<div class="history-full-markdown md-body" use:fileReferenceLinks={{ onOpen: openTranscriptFile }}>{@html messageBodyHtml('agent', body.data.text)}</div>
									{:else}
										<pre>{body.data.text || JSON.stringify(body.data.json, null, 2)}</pre>
									{/if}
									{#if body.data.truncated}<div class="history-evidence-state">Nano retained the first {body.data.storedBytes} of {body.data.originalBytes} bytes.</div>{/if}
								{/if}
							</details>
						{/if}
						{#if body?.state === 'ready' && body.data.truncated}<div class="history-body-preview">Nano retained only the first {body.data.storedBytes} of {body.data.originalBytes} bytes.</div>{/if}

              {#if transcript.view === 'dev'}
                <div class="history-dev-meta">
                  {#each normalizedItemMetadata(item) as [key, value]}
                    <span><b>{key}</b>={value}</span>
                  {/each}
                  {#if item.synthetic}<span><b>synthetic</b>=true</span>{/if}
                  {#if item.sidechain}<span><b>sidechain</b>=true</span>{/if}
                  {#if item.isFinalAnswer}<span><b>final</b>=true</span>{/if}
									{#if item.isFinalAnswer && turn?.finalitySource}<span><b>finality</b>={turn.finalitySource}</span>{/if}
									{#if item.isFinalAnswer && turn?.finalityConfidence}<span><b>confidence</b>={turn.finalityConfidence}</span>{/if}
									{#if item.isFinalAnswer && turn?.completionEvidence}<span><b>completion</b>={turn.completionEvidence}</span>{/if}
                </div>
                {#if item.filterTags?.length}
                  <div class="history-dev-tags">tags · {item.filterTags.join(', ')}</div>
                {/if}
                {#if item.detail != null}
                  <details class="history-dev-detail">
                    <summary>Normalized detail</summary>
                    <pre>{detailText(item.detail)}</pre>
                  </details>
                {/if}
                {#if transcript.projection?.evidenceAvailable}
                  <details class="history-dev-detail history-native" ontoggle={(event) => evidenceToggle(event, item.id)}>
                    <summary>Native evidence</summary>
                    {#if !evidence || evidence.state === 'loading'}
                      <div class="history-evidence-state">Loading native row…</div>
                    {:else if evidence.state === 'error'}
                      <div class="history-evidence-error">{evidence.error}</div>
                      <button class="load-older" onclick={() => loadTranscriptEvidence(item.id)}>Retry</button>
                    {:else}
                      <dl class="history-provenance">
                        {#each evidenceProvenance(evidence.data) as [label, value]}
                          <dt>{label}</dt><dd>{value}</dd>
                        {/each}
                      </dl>
                      <div class="history-raw-encoding">{evidenceDisplay(evidence.data).encoding}</div>
                      <pre>{evidenceDisplay(evidence.data).text}</pre>
                    {/if}
                  </details>
                {/if}
              {/if}
            </div>
            {#if timestamp}
              <time class="history-timestamp" datetime={item.createdAt} title={item.createdAt}>{timestamp}</time>
            {/if}
          </article>
        {/each}
      </div>
    {/if}
  </div>
	{#if transcript.newerAvailable}
		<button class="history-newer" onclick={jumpToNewest}>Newer events available ↓</button>
	{/if}
</section>
