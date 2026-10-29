<script>
  import { cwdTail, labelIsFallback, rowRenderModel, workerSummaryText } from './explorer_format.js'
  import { rowSubtitle } from './sidebar_projection.js'
  import TreeBranch from './TreeBranch.svelte'
  import { toggleWorkerGroup } from './worker_disclosure.svelte.js'
  import SubagentHistoryRow from './SubagentHistoryRow.svelte'
  import { openSession, openThread } from '../../app.svelte.js'
  import { isActiveCatalogThread } from '../../panes.svelte.js'
  import { createNameEditor } from './name_editor.svelte.js'
  import WorkingTimer from './WorkingTimer.svelte'
  import { workingIndicator } from './explorer_format.js'

  // One catalog row: a single-line grid sharing the .xgrid column template with
  // the list header. Unknown/unsupported cells render a muted placeholder —
  // never a fabricated value. Clicking the row stages history in the workspace; the name
  // cell is click-to-edit and stops that propagation. The row is a role="button"
  // (not a <button>) so the inline name <input> is valid interactive content.
  let { item } = $props()
  const m = $derived(rowRenderModel(item))
  // The rail's working-directory and model lines render here too, so rows keep
  // their geometry while the rail expands. The model is hidden at compact
  // density; the title and working directory remain visible in both modes.
  const cwd = $derived(m.cells.project?.title ?? null)
  const cwdDisplay = $derived(cwdTail(cwd) ?? '—')
  const subtitle = $derived(rowSubtitle(item))
  const selected = $derived(isActiveCatalogThread(item.id))
  // Same projection the rail renders, so the two surfaces cannot disagree about
  // a row (the operator 2026-07-29). Placement differs — the table has no age slot to
  // borrow, so the elapsed rides the title line beside the other row badges.
  const live = $derived(workingIndicator(item))
  const workerSummary = $derived(workerSummaryText({
    current: item.currentSubagentCount,
    historical: item.historicalAgentCount,
    working: item.workingSubagentCount,
  }))
  const workerParent = $derived(item.invokerNativeThreadId ?? item.invokerHostedThreadId ?? item.parentNativeId)
  const editor = createNameEditor()

  function toggleWorkers(e) {
    e.stopPropagation() // never open history from the disclosure control
    toggleWorkerGroup(item)
  }

  // A memory row (`transient`) has no durable detail to open — its id is
  // synthesized from the session and GET /api/v1/threads/mem_* would 404 — so
  // it routes to the live session it stands for instead of historical detail.
  function openRow() {
    if (editor.state.editing) return
    if (item.transient) openSession(item.runtime?.sessionId)
    else openThread(item.id)
  }
  function onRowKey(e) {
    if (editor.state.editing) return
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      openRow()
    }
  }
  function startEdit(e) {
    e.stopPropagation()
    editor.begin(item.id, m.name)
  }
  function onInputKey(e) {
    if (e.key === 'Enter') {
      e.preventDefault()
      editor.commit()
    } else if (e.key === 'Escape') {
      e.preventDefault()
      editor.cancel()
    }
  }
  function focusOnMount(node) {
    node.focus()
    node.select()
  }

  let copied = $state(false)
  let copiedTimer
  function copyNativeId(e) {
    e.stopPropagation() // never open history from the copy affordance
    navigator.clipboard?.writeText(item.nativeId).then(() => {
      copied = true
      clearTimeout(copiedTimer)
      copiedTimer = setTimeout(() => (copied = false), 1200)
    })
  }
</script>

{#snippet cell(text)}
  {#if text}{text}{:else}<span class="xdash">–</span>{/if}
{/snippet}

{#snippet when(t)}
  {#if t}<span class="xw-day">{t.day}</span>{t.time}{:else}<span class="xdash">–</span>{/if}
{/snippet}

<div
  class="xrow xgrid"
  class:on={selected}
  class:worker-child={item.treeDepth > 0}
  class:working={m.dot?.state === 'working'}
  role="button"
  tabindex="0"
  data-testid="thread-row"
  data-thread-id={item.id}
  data-sidebar-row
  data-sidebar-key={`sidebar:${item.id}`}
  data-sidebar-id={item.id}
  data-sidebar-type={item.transient ? 'transient' : 'catalog'}
  data-sidebar-status={m.dot?.state ?? 'idle'}
  data-sidebar-active={selected}
  data-sidebar-group={workerParent ?? ''}
  data-sidebar-depth={item.treeDepth ?? 0}
  onclick={openRow}
  onkeydown={onRowKey}
>
  <span class="xc-dot">
    {#if m.dot}<span class="xdot {m.dot.state}" title={m.dot.title}></span>{/if}
  </span>
  <span class="xc xc-title" style={item.treeDepth > 0 ? `padding-left: ${item.treeDepth * 14 + 4}px` : ''}>
    {#if item.treeDepth > 0}
      <TreeBranch depth={item.treeDepth} path={item.treePath} last={item.treeLast} parent={workerParent} overlay />
    {/if}
    <span class="xc-line">
      {#if item.transient}
        <span class="ifile-badge neutral xc-origin" title="Live nano session — not indexed yet; opens the session">live</span>
      {/if}
      {#if m.programmatic}<span class="ifile-badge programmatic xc-origin">{m.programmatic}</span>{/if}
      {#if live?.state === 'waiting'}
        <span class="working-elapsed waiting xc-origin" title={live.title}>{live.text}</span>
      {:else if live}
        <span class="xc-origin"><WorkingTimer {item} /></span>
      {/if}
      {#if editor.state.editing}
        <input
          class="xname-input"
          type="text"
          aria-label="Rename thread"
          value={editor.state.value}
          disabled={editor.state.saving}
          oninput={(e) => (editor.state.value = e.currentTarget.value)}
          onkeydown={onInputKey}
          onblur={editor.cancel}
          onclick={(e) => e.stopPropagation()}
          use:focusOnMount
        />
        {#if editor.state.error}<span class="ifile-error xname-error">{editor.state.error}</span>{/if}
      {:else}
        <button class="xname-edit" data-sidebar-content type="button" aria-label="Rename thread" onclick={startEdit}
          >{#if labelIsFallback(m.label.kind)}<span class="xname-fallback">{m.label.text}</span>{:else}{m.label.text}{/if}</button
        >
      {/if}
    </span>
    <span class="xc-cwd-line">
      <span class="xc-cwd" title={cwd ?? ''}>{cwdDisplay}</span>
      {#if workerSummary && item.workersCollapsible}
        <button
          class="worker-summary worker-toggle"
          class:active={item.workingSubagentCount > 0}
          type="button"
          aria-expanded={item.workersExpanded}
          aria-label="{item.workersExpanded ? 'Hide' : 'Show'} subagents of {m.label.text}"
          title="{item.workersExpanded ? 'Hide' : 'Show'} current subagents"
          onclick={toggleWorkers}
        ><span class="worker-caret" aria-hidden="true">{item.workersExpanded ? '▾' : '▸'}</span>{workerSummary}</button>
      {:else if workerSummary}
        <span class="worker-summary" class:active={item.workingSubagentCount > 0}>{workerSummary}</span>
      {/if}
    </span>
    {#if subtitle}<span class="xc-sub">{subtitle}</span>{/if}
  </span>
  <span class="xc xc-project" title={m.cells.project?.title}>{@render cell(m.cells.project?.text)}</span>
  <span class="xc xc-provider"><span class="t-provider {item.provider}">{item.provider}</span></span>
  <span class="xc xc-model" title={m.cells.model?.title}>{@render cell(m.cells.model?.text)}</span>
  <span class="xc xc-effort">{@render cell(m.cells.effort)}</span>
  <span class="xc xc-when xc-start">{@render when(m.started)}</span>
  <span class="xc xc-when">{@render when(m.updated)}</span>
  <span class="xc xc-nativeid" title={item.nativeId}>
    {@render cell(item.nativeId)}
    {#if item.nativeId}
      <button
        class="xcopy-btn"
        type="button"
        aria-label="Copy thread id"
        title="Copy thread id"
        onclick={copyNativeId}
      >{copied ? '✓' : '⧉'}</button>
    {/if}
  </span>
  <span class="xc xc-branch">{@render cell(m.cells.branch)}</span>
  <span class="xc-health">
    {#each m.badges as b}
      <span class="ifile-badge {b.tone}">{b.text}</span>
    {:else}
      <span class="xdash">–</span>
    {/each}
  </span>
</div>
{#each item.subagentHistoryControlsAfter ?? [] as control}
  <SubagentHistoryRow {control} label={rowRenderModel(control.parent).label.text} />
{/each}
