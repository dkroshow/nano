<script>
  import { untrack } from 'svelte'
  import { mailState } from './mail.svelte.js'
  import { mailIsEditable, mailSenderLabel, mailStatusLabel } from './mail_format.js'
  import { neatTime } from '../explorer/explorer_format.js'

  // The inbox half of the thread pop-up: this thread's durable mail,
  // newest first, plus the operator's compose box. Every read peeks — the UI
  // never marks mail seen on the agent's behalf — and the operator can edit
  // only their own messages while the agent has not read them.
  let { hostedThreadId } = $props()

  const PAGE_LIMIT = 100

  let messages = $state([])
  let loading = $state(false)
  let error = $state(null)
  let truncated = $state(false)
  let expandedId = $state(null)
  let eventsById = $state({})
  let composeBody = $state('')
  let sending = $state(false)
  let sendError = $state(null)
  let editingId = $state(null)
  let editBody = $state('')
  let editSaving = $state(false)
  let editError = $state(null)

  async function readError(response, fallback) {
    try {
      const body = await response.json()
      return body?.detail || body?.error || fallback
    } catch {
      return fallback
    }
  }

  // The inbox API pages forward by sequence, so walk it to the end (bounded)
  // and render newest first. MAIL_CAP keeps a pathological inbox from holding
  // the pane hostage; the tail is what the operator needs.
  const MAIL_CAP = 500

  async function loadMail(id, signal) {
    const collected = []
    let since = 0
    for (;;) {
      const query = `actorThreadRef=${encodeURIComponent(id)}&peek=true&limit=${PAGE_LIMIT}&since=${since}`
      const response = await fetch(`/api/v1/hosted-threads/${encodeURIComponent(id)}/inbox?${query}`, { signal })
      if (!response.ok) throw new Error(await readError(response, `Inbox read failed (${response.status}).`))
      const page = await response.json()
      const items = page.items ?? []
      collected.push(...items)
      if (items.length < PAGE_LIMIT || collected.length >= MAIL_CAP) {
        return {
          items: collected.slice(-MAIL_CAP).reverse(),
          truncated: collected.length >= MAIL_CAP && items.length >= PAGE_LIMIT,
        }
      }
      since = page.nextSince
    }
  }

  // One effect owns the list: it refetches when the thread changes and on
  // every mail SSE delta (revision). Peek reads are idempotent and cheap.
  $effect(() => {
    const id = hostedThreadId
    void mailState.revision
    if (!id) {
      messages = []
      return
    }
    const controller = new AbortController()
    // untrack: this effect must depend only on the thread id and the mail
    // revision — reading `messages` here would make its own writes re-trigger
    // the effect in an infinite refetch loop.
    loading = untrack(() => messages.length === 0)
    void (async () => {
      try {
        const page = await loadMail(id, controller.signal)
        if (controller.signal.aborted) return
        messages = page.items
        truncated = page.truncated
        error = null
      } catch (e) {
        if (!controller.signal.aborted) error = String(e?.message ?? e)
      } finally {
        if (!controller.signal.aborted) loading = false
      }
    })()
    return () => controller.abort()
  })

  function operatorMessageId() {
    const suffix = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`
    return `operator:${suffix}`
  }

  async function send() {
    const body = composeBody.trim()
    if (!hostedThreadId || sending || !body) return
    sending = true
    sendError = null
    try {
      const response = await fetch(`/api/v1/hosted-threads/${encodeURIComponent(hostedThreadId)}/messages`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({
          clientMessageId: operatorMessageId(),
          operator: true,
          body,
        }),
      })
      if (!response.ok) throw new Error(await readError(response, `Send failed (${response.status}).`))
      const message = await response.json()
      composeBody = ''
      // Show the send immediately; the mail SSE delta confirms from the server.
      if (!messages.some((existing) => existing.messageId === message.messageId)) {
        messages = [message, ...messages]
      }
    } catch (e) {
      sendError = String(e?.message ?? e)
    } finally {
      sending = false
    }
  }

  function startEdit(message) {
    editingId = message.messageId
    editBody = message.body
    editError = null
  }

  async function saveEdit() {
    const body = editBody.trim()
    if (!editingId || editSaving || !body) return
    editSaving = true
    editError = null
    try {
      const response = await fetch(`/api/v1/messages/${encodeURIComponent(editingId)}/edit`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ body }),
      })
      if (!response.ok) throw new Error(await readError(response, `Edit failed (${response.status}).`))
      const updated = await response.json()
      messages = messages.map((existing) => existing.messageId === updated.messageId ? updated : existing)
      editingId = null
    } catch (e) {
      editError = String(e?.message ?? e)
    } finally {
      editSaving = false
    }
  }

  function toggleExpanded(message) {
    const id = message.messageId
    expandedId = expandedId === id ? null : id
    if (expandedId && !eventsById[id]) void loadEvents(id)
  }

  async function loadEvents(id) {
    try {
      const response = await fetch(`/api/v1/messages/${encodeURIComponent(id)}`)
      if (!response.ok) return
      const detail = await response.json()
      eventsById = { ...eventsById, [id]: detail.events ?? [] }
    } catch {
      // The message list already shows current status; history is optional.
    }
  }

  function fmt(iso) {
    const value = neatTime(iso)
    return value ? `${value.day} ${value.time}` : ''
  }

  function eventLabel(event) {
    const actor = event.actorKind === 'thread' ? 'agent' : event.actorKind
    return `${event.kind} · ${actor}`
  }

  function onComposeKey(event) {
    if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) {
      event.preventDefault()
      void send()
    }
  }
</script>

<section class="mail-pane" aria-label="Agent inbox">
  <header class="mail-pane-head">
    <h4>Inbox</h4>
    <span class="mail-pane-note" title="Reading here never marks mail seen for the agent">operator view · peek only</span>
  </header>

  <div class="mail-compose">
    <textarea
      class="mail-compose-body"
      aria-label="Message to this agent"
      placeholder="Write to this agent… (⌘↵ to send)"
      rows="3"
      value={composeBody}
      disabled={sending}
      oninput={(event) => (composeBody = event.currentTarget.value)}
      onkeydown={onComposeKey}
    ></textarea>
    <div class="mail-compose-actions">
      {#if sendError}<span class="mail-error" role="alert">{sendError}</span>{/if}
      <button class="xname-save-btn" disabled={sending || !composeBody.trim()} onclick={() => void send()}>
        {sending ? 'Sending…' : 'Send as Operator'}
      </button>
    </div>
  </div>

  {#if error}
    <div class="mail-error" role="alert">{error}</div>
  {:else if loading}
    <div class="mail-empty">Loading mail…</div>
  {:else if messages.length === 0}
    <div class="mail-empty">No mail yet. Messages sent to this agent land here.</div>
  {:else}
    <ul class="mail-list">
      {#each messages as message (message.messageId)}
        <li class="mail-item" class:unread={message.status === 'unread'}>
          {#if editingId === message.messageId}
            <div class="mail-edit" aria-label="Edit message">
              <textarea
                class="mail-compose-body"
                aria-label="Edit message body"
                rows="3"
                value={editBody}
                disabled={editSaving}
                oninput={(event) => (editBody = event.currentTarget.value)}
              ></textarea>
              <div class="mail-compose-actions">
                {#if editError}<span class="mail-error" role="alert">{editError}</span>{/if}
                <button class="xresume-btn" disabled={editSaving} onclick={() => (editingId = null)}>Cancel</button>
                <button class="xname-save-btn" disabled={editSaving || !editBody.trim()} onclick={() => void saveEdit()}>
                  {editSaving ? 'Saving…' : 'Save'}
                </button>
              </div>
            </div>
          {:else}
            <button class="mail-item-row" aria-expanded={expandedId === message.messageId} onclick={() => toggleExpanded(message)}>
              <span class="mail-item-head">
                <span class="mail-sender" class:operator={message.sender?.kind === 'operator'}>{mailSenderLabel(message.sender)}</span>
                <span class="mail-status {message.status}">{mailStatusLabel(message.status)}</span>
                <span class="mail-time">{fmt(message.createdAt)}</span>
              </span>
              {#if message.subject}<span class="mail-subject">{message.subject}</span>{/if}
              <span class="mail-preview" class:expanded={expandedId === message.messageId}>{message.body}</span>
            </button>
            {#if expandedId === message.messageId}
              <div class="mail-item-detail">
                {#if message.statusNote}<div class="mail-status-note">Note: {message.statusNote}</div>{/if}
                {#if message.refs?.length}
                  <div class="mail-refs">
                    {#each message.refs as ref}<span class="mail-ref" title="{ref.kind}">{`${ref.kind}:${ref.id}`}</span>{/each}
                  </div>
                {/if}
                {#if eventsById[message.messageId]?.length}
                  <ol class="mail-events">
                    {#each eventsById[message.messageId] as event (event.id)}
                      <li><span class="mail-event-kind">{eventLabel(event)}</span> <span class="mail-time">{fmt(event.at)}</span></li>
                    {/each}
                  </ol>
                {/if}
                {#if mailIsEditable(message)}
                  <div class="mail-item-actions">
                    <button class="xresume-btn" onclick={() => startEdit(message)}>Edit</button>
                    <span class="mail-pane-note">Editable until the agent reads it</span>
                  </div>
                {/if}
              </div>
            {/if}
          {/if}
        </li>
      {/each}
    </ul>
    {#if truncated}
      <div class="mail-pane-note mail-truncated">Showing the latest {MAIL_CAP} messages.</div>
    {/if}
  {/if}
</section>
