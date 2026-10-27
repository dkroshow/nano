<script>
  import { openSidebarRow } from '../app.svelte.js'
  import { isActiveCatalogThread, isActiveSession } from '../panes.svelte.js'
  import { cwdTail, labelIsFallback, sidebarAge, sidebarAgeClass, workerSummaryText } from './explorer_format.js'
  import { openQuickDetail } from './quick_detail.svelte.js'
  import ThreadStarButton from './ThreadStarButton.svelte'
  import { starRequestId } from './thread_stars.js'
  import { liveMailCounts } from './mail.svelte.js'
  import { mailBadge } from './mail_format.js'
  import { toggleWorkerGroup } from './worker_disclosure.svelte.js'
  import SubagentHistoryRow from './SubagentHistoryRow.svelte'
  import WorkingTimer from './WorkingTimer.svelte'
  import TreeBranch from './TreeBranch.svelte'
  import { workingIndicator } from './explorer_format.js'

  // One sidebar row over a projected model (lib/sidebar_projection.js): the
  // liveness indicator, title, working directory, model, activity clock, and
  // thread-actions menu. The click routes on the row's descriptor — attached sessions open live; every
  // indexed non-live row opens durable history. Close/dismiss lives in the
  // popup rather than displacing the activity clock in the rail.
  let { row } = $props()

  const click = $derived(row.click)
  const cwdDisplay = $derived(cwdTail(row.cwd) ?? '—')
  const age = $derived(sidebarAge(row.updatedAt))
  const ageClass = $derived(sidebarAgeClass(row.updatedAt))
  const ageTitle = $derived(row.updatedAt ? new Date(row.updatedAt).toLocaleString() : '')
  const detached = $derived(row.kind === 'detached')
  const workerParent = $derived(row.assignment?.relatedHostedThreadId ?? row.invokerNativeThreadId ?? row.invokerHostedThreadId ?? row.parentNativeId)
  // The worker roll-up lives on the working-directory line, never on the title
  // line: at rail width a pill ahead of the title pushes the name out of view,
  // and the name is what the rail is scanned for.
  const workerSummary = $derived(workerSummaryText({
    current: row.currentSubagentCount,
    historical: row.historicalAgentCount,
    working: row.workingSubagentCount,
  }))
  const onStage = $derived(
    (click.sessionId && isActiveSession(click.sessionId)) ||
    (click.catalogId && isActiveCatalogThread(click.catalogId)),
  )
  const hint = $derived(row.indicator?.title ?? (click.catalogId ? 'Open thread history' : null))
  // The turn's elapsed takes over the row's right-hand slot while a turn is in
  // flight: "how long has this been working" is the live question, and the
  // last-updated age answers it badly (it resets on every wire line). The slot
  // is the same one either way, so the row's geometry never shifts.
  // A lost connection shows once, as the page's reconnecting banner; a row
  // keeps the state it last showed.
  const live = $derived(workingIndicator(row))
  // Inbox badge: live SSE counts win; the row's catalog-stamped
  // counts cover the gap before the first delta names this thread.
  const mail = $derived(mailBadge(liveMailCounts(row.hostedThreadId) ?? row.mail))
  async function activate() {
    await openSidebarRow(click)
  }

  function openDetails(e) {
    e.stopPropagation()
    openQuickDetail({
      ...click,
      provider: row.provider,
      nativeId: row.nativeId,
      descendantActivity: row.subagentActivity ?? row.sidebar?.descendantActivity ?? null,
      closable: row.cls === 'nano',
      detached,
    })
  }

  function toggleWorkers(e) {
    e.stopPropagation() // never open the thread from the disclosure control
    toggleWorkerGroup(row)
  }

  function onRowKey(event) {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault()
      activate()
    }
  }
</script>

<div
  class="thread-row"
  class:on={onStage}
  class:working={row.indicator?.state === 'working'}
  class:detached
  class:dim={row.cls === 'none' && !row.working}
  class:worker-child={row.treeDepth > 0}
  role="button"
  tabindex="0"
  title={hint}
  onclick={activate}
  onkeydown={onRowKey}
  data-sidebar-row
  data-sidebar-key={`sidebar:${row.key}`}
  data-sidebar-id={row.key}
  data-sidebar-type={row.kind || 'thread'}
  data-sidebar-status={row.indicator?.state ?? row.cls ?? 'idle'}
  data-sidebar-active={onStage}
  data-sidebar-group={workerParent ?? ''}
  data-sidebar-depth={row.treeDepth ?? 0}
>
  {#if row.treeDepth > 0}
    <TreeBranch depth={row.treeDepth} path={row.treePath} last={row.treeLast} parent={workerParent} />
  {/if}
  <span class="t-dot {row.indicator?.state ?? ''}"></span>
  <span class="t-main">
    <span class="t-title" class:t-title-fallback={labelIsFallback(row.titleKind)}>
      <span class="t-title-text" data-sidebar-content>{row.title}</span>
    </span>
    <span class="t-cwd-line">
      <span class="t-cwd" title={row.cwd ?? ''}>{cwdDisplay}</span>
      {#if workerSummary && row.workersCollapsible}
        <button
          class="worker-summary worker-toggle"
          class:active={row.workingSubagentCount > 0}
          type="button"
          aria-expanded={row.workersExpanded}
          aria-label="{row.workersExpanded ? 'Hide' : 'Show'} subagents of {row.title}"
          title="{row.workersExpanded ? 'Hide' : 'Show'} current subagents"
          onclick={toggleWorkers}
        ><span class="worker-caret" aria-hidden="true">{row.workersExpanded ? '▾' : '▸'}</span>{workerSummary}</button>
      {:else if workerSummary}
        <!-- Counted but not here to open: the roll-up spans the whole fetched
             window, so these workers are outside the rail's current rows. -->
        <span class="worker-summary" class:active={row.workingSubagentCount > 0}>{workerSummary}</span>
      {/if}
    </span>
    <span class="t-sub">{row.subtitle}</span>
  </span>
  <span class="t-end">
    {#if mail}
      <button
        class="t-mail-badge {mail.state}"
        title={mail.title}
        aria-label="Inbox: {mail.title}"
        onclick={openDetails}
      >{mail.text}</button>
    {/if}
    {#if live?.state === 'working' && row.runtime?.compactingSinceMs}
      <span class="t-compacting" title="Compacting the thread's context">compacting</span>
    {/if}
    {#if live?.state === 'waiting'}
      <span class="t-age t-waiting" title={live.title}>{live.text}</span>
    {:else if live}
      <span class="t-age t-working"><WorkingTimer item={row} /></span>
    {:else if age}
      <span class="t-age {ageClass}" title={ageTitle}>{age}</span>
    {/if}
    <span class="t-trail">
      <!-- Stop the row's open handler here, not only inside the star button:
           a child component's onclick does not reliably cut the parent row's
           bubble in this tree. -->
      <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
      <span class="t-star-slot" role="presentation" onclick={(event) => event.stopPropagation()} onkeydown={(event) => event.stopPropagation()}>
        <ThreadStarButton id={starRequestId(row)} starred={Boolean(row.starred)} />
      </span>

      <button class="t-detail" aria-label="Thread details and actions" title="Thread details and actions" onclick={openDetails}>⋮</button>
    </span>
  </span>
</div>
{#each row.subagentHistoryControlsAfter ?? [] as control}
  <SubagentHistoryRow {control} label={control.parent.title} lazy />
{/each}
