<script>
  import { untrack } from 'svelte'
  import { explorer } from '../../explorer.svelte.js'
  import { displayTitle, neatTime } from '../explorer/explorer_format.js'
  import { mailState, applyMailDelta } from './mail.svelte.js'
  import { MAIL_STATUSES, mailSenderLabel, mailStatusLabel } from './mail_format.js'
  import { openQuickDetail } from '../explorer/quick_detail.svelte.js'

  // The cross-thread mail view: a button in the sidebar header with
  // the global unread count, opening a pop-up of recent mail across every
  // agent inbox. Read-only over the global listing — nothing here marks mail
  // seen — and one click on a row opens that thread's own pop-up, where the
  // full per-thread inbox and compose live.
  let open = $state(false)
  let items = $state([])
  let loading = $state(false)
  let error = $state(null)
  let statusFilter = $state('all')

  const summary = $derived(mailState.summary)

  async function fetchGlobal(signal) {
    const query = statusFilter === 'all' ? '' : `&status=${statusFilter}`
    const response = await fetch(`/api/v1/agent-messages?limit=100${query}`, { signal })
    if (!response.ok) throw new Error(`Mail listing failed (${response.status}).`)
    return await response.json()
  }

  // The button's count needs the summary before any SSE delta arrives; one
  // cheap fetch at mount seeds it through the same last-value-wins path.
  $effect(() => {
    if (mailState.summary) return
    const controller = new AbortController()
    void (async () => {
      try {
        const response = await fetch('/api/v1/agent-messages?limit=1', { signal: controller.signal })
        if (!response.ok) return
        const page = await response.json()
        if (!controller.signal.aborted && page.summary) applyMailDelta({ counts: {}, summary: page.summary })
      } catch {
        // The badge appears with the first mail delta instead.
      }
    })()
    return () => controller.abort()
  })

  // While the pop-up is open, refetch on filter changes and on every mail
  // delta so the list follows live activity.
  $effect(() => {
    if (!open) return
    void statusFilter
    void mailState.revision
    const controller = new AbortController()
    // untrack: the effect keys on open/filter/revision; reading `items` here
    // would make its own writes re-trigger it in a loop.
    loading = untrack(() => items.length === 0)
    void (async () => {
      try {
        const page = await fetchGlobal(controller.signal)
        if (controller.signal.aborted) return
        items = page.items ?? []
        if (page.summary) applyMailDelta({ counts: {}, summary: page.summary })
        error = null
      } catch (e) {
        if (!controller.signal.aborted) error = String(e?.message ?? e)
      } finally {
        if (!controller.signal.aborted) loading = false
      }
    })()
    return () => controller.abort()
  })

  function threadFor(hostedThreadId) {
    return explorer.dataset.find((candidate) => candidate.hostedThreadId === hostedThreadId) ?? null
  }

  function recipientLabel(message) {
    const thread = threadFor(message.recipient?.hostedThreadId)
    return thread ? displayTitle(thread).text : 'unknown thread'
  }

  function openRecipient(message) {
    const thread = threadFor(message.recipient?.hostedThreadId)
    if (!thread) return
    open = false
    openQuickDetail({
      catalogId: thread.transient ? null : thread.id,
      sessionId: thread.runtime?.sessionId ?? null,
      provider: thread.provider,
      nativeId: thread.nativeId,
      closable: Boolean(thread.runtime?.attached),
      detached: Boolean(thread.runtime?.detached),
    })
  }

  function fmt(iso) {
    const value = neatTime(iso)
    return value ? `${value.day} ${value.time}` : ''
  }

  function onWindowKeydown(event) {
    if (event.key === 'Escape' && open) open = false
  }
</script>

<svelte:window onkeydown={onWindowKeydown} />

<button
  class="sort-btn global-mail-btn"
  class:has-unread={Boolean(summary?.unread)}
  type="button"
  aria-haspopup="dialog"
  aria-expanded={open}
  title="Agent mail across all threads{summary ? ` — ${summary.unread} unread of ${summary.total}` : ''}"
  aria-label="Agent mail{summary ? ` — ${summary.unread} unread of ${summary.total}` : ''}"
  onclick={() => (open = !open)}
>
  <span class="sort-btn-glyph" aria-hidden="true">✉</span>
  Mail{#if summary?.total}&nbsp;<span class="global-mail-count" class:unread={summary.unread > 0}>{summary.unread > 0 ? `${summary.unread}/${summary.total}` : summary.total}</span>{/if}
</button>

{#if open}
  <div class="quick-detail-overlay" role="presentation" onclick={(event) => { if (event.target === event.currentTarget) open = false }}>
    <div class="quick-detail global-mail" role="dialog" aria-modal="true" aria-label="Agent mail across all threads">
      <button class="quick-detail-close" aria-label="Close mail" onclick={() => (open = false)}>×</button>
      <h3>Agent mail</h3>
      <div class="global-mail-filters" role="group" aria-label="Filter by status">
        {#each ['all', ...MAIL_STATUSES] as status}
          <button
            class="global-mail-chip"
            class:on={statusFilter === status}
            aria-pressed={statusFilter === status}
            onclick={() => (statusFilter = status)}
          >{status === 'seen' ? 'undispositioned' : status}</button>
        {/each}
      </div>
      {#if error}
        <div class="mail-error" role="alert">{error}</div>
      {:else if loading}
        <div class="mail-empty">Loading mail…</div>
      {:else if items.length === 0}
        <div class="mail-empty">No mail{statusFilter === 'all' ? ' yet' : ` with status ${mailStatusLabel(statusFilter)}`}.</div>
      {:else}
        <ul class="mail-list global-mail-list">
          {#each items as message (message.messageId)}
            <li class="mail-item" class:unread={message.status === 'unread'}>
              <button class="mail-item-row" onclick={() => openRecipient(message)} title="Open this thread's inbox">
                <span class="mail-item-head">
                  <span class="mail-recipient">→ {recipientLabel(message)}</span>
                  <span class="mail-status {message.status}">{mailStatusLabel(message.status)}</span>
                  <span class="mail-time">{fmt(message.createdAt)}</span>
                </span>
                <span class="mail-item-head">
                  <span class="mail-sender" class:operator={message.sender?.kind === 'operator'}>{mailSenderLabel(message.sender)}</span>
                  {#if message.subject}<span class="mail-subject">{message.subject}</span>{/if}
                </span>
                <span class="mail-preview">{message.body}</span>
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  </div>
{/if}
