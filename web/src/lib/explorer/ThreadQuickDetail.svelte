<script>
  import { runtimeLabel } from '../shared/lifecycle_state.js'
  import MailPane from '../mail/MailPane.svelte'
  import MetaValue from './MetaValue.svelte'
  import { app, dismissDetached, openSession, openThread, openThreadReference } from '../../app.svelte.js'
  import {
    clearThreadName,
    explorer,
    loadDataset,
    patchSidebarPlacement,
    renameThread,
  } from '../../explorer.svelte.js'
  import { displayTitle, neatTime } from './explorer_format.js'
  import { quickDetail, closeQuickDetail } from './quick_detail.svelte.js'
  import { requestParentClose } from './descendant_close.svelte.js'
  import { requestBulkClose } from './bulk_close.svelte.js'
  import { readAPIError } from '../../api_error.js'
  import { untrack } from 'svelte'

  const target = $derived(quickDetail.target)
  const item = $derived(target ? ((target.catalogId
    ? explorer.dataset.find((candidate) => candidate.id === target.catalogId)
    : explorer.dataset.find((candidate) => candidate.provider === target.provider && candidate.nativeId === target.nativeId)) ?? null) : null)
  const session = $derived(target?.sessionId ? app.sessions.find((candidate) => candidate.id === target.sessionId) ?? null : null)
  const provider = $derived(item?.provider ?? target?.provider ?? session?.provider ?? '')
  const nativeId = $derived(item?.nativeId ?? target?.nativeId ?? session?.threadId ?? '')
  const runtime = $derived(item?.runtime)
  const title = $derived(displayTitle(item ?? { cwd: session?.cwd, nativeId }).text || 'Thread')
  const editId = $derived(item?.transient ? '' : item?.id ?? '')
  const hostedThreadId = $derived(item?.hostedThreadId ?? session?.hostedThreadId ?? '')
  const assignment = $derived(item?.assignment ?? session?.assignment ?? null)
  const assignmentUnavailable = $derived(item?.assignmentUnavailable ?? session?.assignmentUnavailable ?? false)
  const invokerHostedThreadId = $derived(item?.invokerHostedThreadId ?? session?.invokerHostedThreadId ?? '')
  const invokerNativeThreadId = $derived(item?.invokerNativeThreadId ?? session?.invokerNativeThreadId ?? '')
  const invokerProvider = $derived(item?.invokerProvider ?? session?.invokerProvider ?? provider)
  const sidebarPlacement = $derived(item?.sidebarPlacement ?? session?.sidebarPlacement ?? runtime?.sidebarPlacement ?? 'invoker')
  const assignmentControlsPlacement = $derived(Boolean(!assignmentUnavailable && assignment?.relatedHostedThreadId))
  const invokerCandidate = $derived(explorer.dataset.find((candidate) => (
    (invokerHostedThreadId && candidate.hostedThreadId === invokerHostedThreadId)
    || (invokerNativeThreadId && candidate.nativeId === invokerNativeThreadId && candidate.provider === invokerProvider)
  )) ?? null)
  const relatedThreadOptions = $derived(explorer.dataset.filter((candidate) => (
    candidate.hostedThreadId && candidate.hostedThreadId !== hostedThreadId
  )))
  let nameValue = $state('')
  let nameSaving = $state(false)
  let nameError = $state(null)
  let closeError = $state(null)
  let failedAttachment = $state(null)
  let detail = $state(null)
  let roleEditing = $state(false)
  let roleValue = $state('worker')
  let relatedThreadValue = $state('')
  let focusValue = $state('')
  let roleSaving = $state(false)
  let roleError = $state(null)
  let presentationSaving = $state(false)
  let presentationError = $state(null)
  let placementSaving = $state(false)
  let placementError = $state(null)
  let subscriptions = $state(null)
  let subscriptionsSaving = $state(false)
  let subscriptionsError = $state(null)
  let invokerIdentity = $state(null)
  // The live truth for the toggle label: session summary first (flips
  // immediately), then the catalog's runtime overlay.
  const presentationMode = $derived(session?.presentationMode ?? runtime?.presentationMode ?? 'shared')
  const sourceDetail = $derived(detail?.sources?.[0] ?? null)
  const currentValues = $derived(detail?.currentResolution?.values ?? null)
  const placementLabel = $derived(assignmentControlsPlacement
    ? 'Controlled by role assignment'
    : sidebarPlacement === 'standalone' ? 'Standalone' : 'Under invoker')
  const invokerLabel = $derived(invokerCandidate
    ? relatedLabel(invokerCandidate)
    : invokerIdentity?.displayName
      ? `${invokerIdentity.displayName}${invokerIdentity.provider || invokerIdentity.nativeThreadId ? ` (${invokerIdentity.provider ?? invokerProvider}${invokerIdentity.nativeThreadId ? `, ${invokerIdentity.nativeThreadId}` : ''})` : ''}`
      : invokerNativeThreadId ? `${invokerProvider || 'thread'} (${invokerNativeThreadId})` : '')

  const transientMetadata = $derived({
    cwd: { state: session?.cwd ? 'known' : 'unknown', value: session?.cwd ?? null, evidence: session?.cwd ? 'runtime' : null },
    model: { state: session?.model ? 'known' : 'unknown', value: session?.model ?? null, evidence: session?.model ? 'runtime' : null },
    effort: { state: 'unknown' }, priority: { state: 'unknown' }, permission: { state: 'unknown' },
    historicalBranch: { state: 'unknown' }, origin: { state: 'unknown' },
  })
  const metadata = $derived(item?.metadata ?? transientMetadata)
  const META_FIELDS = [['cwd', 'cwd'], ['model', 'model'], ['effort', 'effort'], ['priority', 'priority'], ['permission', 'permission'], ['historicalBranch', 'git branch']]
  const closeSessionID = $derived(session?.id ?? target?.sessionId ?? '')
  const canClose = $derived(Boolean(target?.closable && (target?.detached ? nativeId : closeSessionID)))
  const closeLabel = $derived(provider === 'claude' ? 'End session' : target?.detached ? 'Forget thread' : 'Close session')
  const descendantCount = $derived(target?.descendantActivity?.total
    ?? item?.subagentActivity?.total
    ?? item?.sidebar?.descendantActivity?.total
    ?? 0)
  const canBulkClose = $derived(Boolean(hostedThreadId && descendantCount > 0))

  function status() {
    if (runtime?.attached || session) return `Nano attached · ${runtimeLabel(session ?? runtime)}`
    if (runtime?.status === 'starting') return 'Starting after restart · durable history'
    if (runtime?.status === 'resting') return 'Resting since restart · reconnects when needed'
    if (runtime?.detached) return 'Detached · durable history'
    return 'Historical · durable history'
  }

  function fmt(iso) {
    const value = neatTime(iso)
    return value ? `${value.day} ${value.time}` : '—'
  }

  function currentValue(key, fallback = 'unknown') {
    const field = currentValues?.[key]
    if (!field || field.state !== 'known') return detail ? fallback : 'resolving…'
    return typeof field.value === 'string' ? field.value : JSON.stringify(field.value)
  }

  function open() {
    if (target?.catalogId) openThread(target.catalogId)
    else if (target?.sessionId) openSession(target.sessionId)
    closeQuickDetail()
  }

  async function close() {
    closeError = null
    failedAttachment = null
    const attempted = provider === 'claude' && target?.detached
      ? app.detached.find((row) => row.provider === provider && row.nativeThreadId === nativeId)
      : null
    try {
      if (target?.detached) {
        await dismissDetached(provider, nativeId)
        closeQuickDetail()
        return
      }
      if (!closeSessionID) return
      const result = await requestParentClose({ parent: item ?? {
        provider, nativeId, hostedThreadId: session?.hostedThreadId,
      }, sessionId: closeSessionID, provider })
      if (result.parentClosed) closeQuickDetail()
    } catch (error) {
      closeError = String(error?.message ?? error)
      failedAttachment = attempted ? { ...attempted } : null
    }
  }

  async function forgetUnconfirmed() {
    const attachment = failedAttachment
    if (!attachment || !window.confirm('Claude may still be running. Forget only Nano’s attachment? This does not stop the process or delete the saved conversation.')) return
    try {
      await dismissDetached(attachment.provider, attachment.nativeThreadId, attachment)
      closeQuickDetail()
    } catch (error) {
      closeError = String(error?.message ?? error)
    }
  }

  async function closeTree() {
    closeError = null
    try {
      const root = item ?? {
        id: target?.catalogId,
        provider,
        nativeId,
        hostedThreadId,
        title,
        runtime,
      }
      const result = await requestBulkClose(root)
      if (result.complete) closeQuickDetail()
    } catch (error) {
      closeError = String(error?.message ?? error)
    }
  }

  async function saveName() {
    const id = editId
    if (!id || nameSaving) return
    const trimmed = nameValue.trim()
    nameSaving = true
    nameError = null
    try {
      if (trimmed === '') await clearThreadName(id)
      else await renameThread(id, trimmed)
    } catch (e) {
      nameError = e?.message ?? String(e)
    } finally {
      nameSaving = false
    }
  }

  function onNameKey(event) {
    if (event.key === 'Enter') { event.preventDefault(); void saveName() }
  }

  function relatedLabel(candidate) {
    return `${displayTitle(candidate).text} (${candidate.provider}${candidate.nativeId ? `, ${candidate.nativeId}` : ''})`
  }
  function relatedLabelFor(hostedId) {
    const candidate = relatedThreadOptions.find((entry) => entry.hostedThreadId === hostedId)
    return candidate ? relatedLabel(candidate) : hostedId
  }
  function relatedHostedFor(value) {
    const candidate = relatedThreadOptions.find((entry) => relatedLabel(entry) === value || entry.hostedThreadId === value || entry.nativeId === value)
    return candidate?.hostedThreadId ?? value
  }

  function editRole() {
    roleValue = assignment?.assignmentRole ?? 'worker'
    relatedThreadValue = relatedLabelFor(assignment?.relatedHostedThreadId
      ?? assignment?.auditTargets?.[0]?.hostedThreadId
      ?? invokerHostedThreadId
      ?? '')
    focusValue = assignment?.focus ?? ''
    roleError = null
    roleEditing = true
  }

  function assignmentActionID() {
    const suffix = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`
    return `ui-role-${suffix}`
  }

  async function saveRole() {
    if (!hostedThreadId || roleSaving) return
    const related = relatedHostedFor(relatedThreadValue.trim())
    const focus = focusValue.trim()
    if (roleValue !== 'unassigned' && !related) {
      roleError = 'Choose the thread this agent belongs under.'
      return
    }
    if (roleValue !== 'unassigned' && !focus) {
      roleError = 'Focus is required.'
      return
    }
    roleSaving = true
    roleError = null
    try {
      const nextAssignment = roleValue === 'unassigned' ? null : {
        assignmentRole: roleValue,
        focus,
        relatedHostedThreadId: related,
        auditTargets: roleValue === 'auditor' ? [{ hostedThreadId: related }] : [],
      }
      const response = await fetch(`/api/v1/hosted-threads/${encodeURIComponent(hostedThreadId)}/assignment`, {
        method: 'PUT',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ clientActionId: assignmentActionID(), assignment: nextAssignment }),
      })
      if (!response.ok) {
        throw new Error((await readAPIError(response, `Role update failed (${response.status}).`)).message)
      }
      const result = await response.json()
      explorer.dataset = explorer.dataset.map((candidate) => candidate.hostedThreadId === hostedThreadId
        ? { ...candidate, assignment: result.assignment ?? null, assignmentUnavailable: false }
        : candidate)
      app.sessions = app.sessions.map((candidate) => candidate.hostedThreadId === hostedThreadId
        ? { ...candidate, assignment: result.assignment ?? null, assignmentUnavailable: false }
        : candidate)
      roleEditing = false
      await loadDataset()
    } catch (error) {
      roleError = String(error?.message ?? error)
    } finally {
      roleSaving = false
    }
  }

  async function togglePresentation() {
    if (!hostedThreadId || presentationSaving) return
    presentationSaving = true
    presentationError = null
    const next = presentationMode === 'background' ? 'shared' : 'background'
    try {
      const response = await fetch(`/api/v1/hosted-threads/${encodeURIComponent(hostedThreadId)}/presentation`, {
        method: 'PUT',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ presentationMode: next }),
      })
      if (!response.ok) {
        throw new Error((await readAPIError(response, `Presentation update failed (${response.status}).`)).message)
      }
      // Patch both local stores so the sidebar reacts on this frame; the
      // runtime SSE nudge then confirms from the server.
      app.sessions = app.sessions.map((candidate) => candidate.hostedThreadId === hostedThreadId
        ? { ...candidate, presentationMode: next }
        : candidate)
      explorer.dataset = explorer.dataset.map((candidate) => candidate.hostedThreadId === hostedThreadId
        ? { ...candidate, runtime: { ...(candidate.runtime ?? {}), presentationMode: next } }
        : candidate)
    } catch (error) {
      presentationError = String(error?.message ?? error)
    } finally {
      presentationSaving = false
    }
  }

  async function toggleSidebarPlacement() {
    if (!hostedThreadId || placementSaving || assignmentControlsPlacement) return
    placementSaving = true
    placementError = null
    const next = sidebarPlacement === 'standalone' ? 'invoker' : 'standalone'
    try {
      const response = await fetch(`/api/v1/hosted-threads/${encodeURIComponent(hostedThreadId)}/sidebar-placement`, {
        method: 'PUT',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ sidebarPlacement: next }),
      })
      if (!response.ok) {
        throw new Error((await readAPIError(response, `Sidebar placement update failed (${response.status}).`)).message)
      }
      const saved = (await response.json()).sidebarPlacement ?? next
      app.sessions = app.sessions.map((candidate) => candidate.hostedThreadId === hostedThreadId
        ? { ...candidate, sidebarPlacement: saved }
        : candidate)
      patchSidebarPlacement(hostedThreadId, saved)
    } catch (error) {
      placementError = String(error?.message ?? error)
    } finally {
      placementSaving = false
    }
  }

  async function toggleSubscription(channel) {
    const ref = hostedThreadId
    if (!ref || !subscriptions || subscriptionsSaving) return
    const next = { ...subscriptions, [channel]: subscriptions[channel] === 'send' ? 'none' : 'send' }
    subscriptionsSaving = true
    subscriptionsError = null
    try {
      const response = await fetch(`/api/v1/hosted-threads/${encodeURIComponent(ref)}/subscriptions`, {
        method: 'PUT',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(next),
      })
      if (!response.ok) {
        throw new Error((await readAPIError(response, `Notification update failed (${response.status}).`)).message)
      }
      const saved = (await response.json()).subscriptions
      if (hostedThreadId === ref) subscriptions = saved
    } catch (error) {
      if (hostedThreadId === ref) subscriptionsError = String(error?.message ?? error)
    } finally {
      if (hostedThreadId === ref) subscriptionsSaving = false
    }
  }

  $effect(() => {
    const ref = hostedThreadId
    subscriptions = null
    subscriptionsError = null
    subscriptionsSaving = false
    if (!ref || !invokerHostedThreadId) return
    const controller = new AbortController()
    void (async () => {
      try {
        const response = await fetch(`/api/v1/hosted-threads/${encodeURIComponent(ref)}`, { signal: controller.signal })
        if (!response.ok) {
          throw new Error((await readAPIError(response, `Notifications unavailable (${response.status}).`)).message)
        }
        const body = await response.json()
        if (!controller.signal.aborted) subscriptions = body.subscriptions
      } catch (error) {
        if (!controller.signal.aborted) subscriptionsError = String(error?.message ?? error)
      }
    })()
    return () => controller.abort()
  })

  function openInvoker() {
    if (invokerCandidate?.runtime?.sessionId) openSession(invokerCandidate.runtime.sessionId)
    else if (invokerCandidate?.id) openThread(invokerCandidate.id)
    else openThreadReference(invokerIdentity?.nativeThreadId || invokerHostedThreadId || invokerNativeThreadId)
    closeQuickDetail()
  }

  function onWindowKeydown(event) {
    if (event.key === 'Escape' && target) closeQuickDetail()
  }

  // Established style: the name field is a plain input, always editable, no
  // click-to-open step. Re-seed it from the thread's current name only when
  // the thread being viewed changes (editId), not on every reactive read of
  // its name — otherwise a keystroke-triggered rerender could clobber
  // whatever the operator is mid-typing.
  $effect(() => {
    const id = editId
    untrack(() => {
      nameValue = id ? (item?.name?.value ?? '') : ''
      nameError = null
    })
  })

  $effect(() => {
    const ref = invokerHostedThreadId
    invokerIdentity = null
    if (!ref) return
    const controller = new AbortController()
    void (async () => {
      try {
        const response = await fetch(`/api/v1/identity/resolve?ref=${encodeURIComponent(ref)}`, { signal: controller.signal })
        if (!response.ok) return
        const body = await response.json()
        if (!controller.signal.aborted) invokerIdentity = body
      } catch {
        // The immutable provider/native snapshot remains a useful fallback.
      }
    })()
    return () => controller.abort()
  })

  $effect(() => {
    const id = hostedThreadId
    untrack(() => {
      roleEditing = false
      roleError = null
      roleSaving = false
    })
  })

  // The compact list carries only source health. Fetch the popup's one primary
  // source path from the canonical detail endpoint; never use it for a mem_*
  // transient row, which intentionally has no durable detail resource.
  $effect(() => {
    const id = target?.catalogId
    detail = null
    if (!id) return
    const controller = new AbortController()
    void (async () => {
      try {
        const response = await fetch(`/api/v1/threads/${encodeURIComponent(id)}`, { signal: controller.signal })
        if (!response.ok) return
        const body = await response.json()
        if (!controller.signal.aborted) detail = body
      } catch {
        // The list's source status remains useful when this optional fetch fails.
      }
    })()
    return () => controller.abort()
  })
</script>

<svelte:window onkeydown={onWindowKeydown} />

{#if target}
  <div class="quick-detail-overlay" role="presentation" onclick={(event) => { if (event.target === event.currentTarget) closeQuickDetail() }}>
    <div class="quick-detail" class:with-mail={Boolean(hostedThreadId)} role="dialog" aria-modal="true" aria-label="Thread details">
      <button class="quick-detail-close" aria-label="Close thread details" onclick={closeQuickDetail}>×</button>
      <div class="quick-detail-main">
      <h3>{provider ? `${provider} : ` : ''}{title}</h3>
      <dl class="quick-detail-grid">
        <dt>Thread</dt><dd class="wrap">{nativeId || 'unknown'}</dd>
        {#if editId}<dt>Nano ID</dt><dd class="wrap">{editId}</dd>{/if}
        <dt>Name</dt>
        <dd>{#if editId}
          <div class="xname-row">
            <input class="xname-input" aria-label="Rename thread" placeholder="Add name…" value={nameValue} disabled={nameSaving} oninput={(event) => (nameValue = event.currentTarget.value)} onkeydown={onNameKey} />
            <button class="xname-save-btn" disabled={nameSaving} onclick={() => void saveName()}>Save</button>
          </div>
          {#if nameError}<div class="ifile-error">{nameError}</div>{/if}
        {:else}unknown{/if}</dd>
        <dt>Status</dt><dd>{status()}</dd>
        <dt>Role</dt><dd>{assignmentUnavailable ? 'unavailable' : assignment?.assignmentRole ?? 'unassigned'}</dd>
        <dt>Sidebar placement</dt><dd>{placementLabel}</dd>
        {#if invokerLabel}<dt>Invoked by</dt><dd><button class="xresume-btn" onclick={openInvoker}>{invokerLabel}</button></dd>{/if}
        {#if provider === 'codex' && !session}
          <dt>cwd</dt><dd class="wrap">{currentValue('cwd')}</dd>
          <dt>model</dt><dd class="wrap">{currentValue('model')}</dd>
          <dt>effort</dt><dd class="wrap">{currentValue('reasoningEffort', 'default')}</dd>
          <dt>sandbox</dt><dd class="wrap">{currentValue('sandbox')}</dd>
          <dt>approvals</dt><dd class="wrap">{currentValue('approvalPolicy')}</dd>
        {:else}
          {#each META_FIELDS as [key, label]}<dt>{label}</dt><dd class="wrap"><MetaValue field={metadata[key]} {provider} /></dd>{/each}
        {/if}
        <dt>Source</dt><dd class="wrap">{sourceDetail?.path ?? item?.source?.status ?? 'not indexed'}{#if sourceDetail}<span class="mv-evidence">{sourceDetail.status}</span>{:else if item?.source?.diagnosticCount} · {item.source.diagnosticCount} diagnostics{/if}</dd>
        <dt>Created</dt><dd>{fmt(item?.createdAt ?? session?.createdAt)}</dd>
        <dt>Updated</dt><dd>{fmt(item?.updatedAt ?? session?.updatedAt)}</dd>
      </dl>
      {#if roleEditing}
        <section class="quick-detail-role-editor" aria-label="Set thread role">
          <label>
            <span>Set as</span>
            <select aria-label="Set as" value={roleValue} disabled={roleSaving} onchange={(event) => (roleValue = event.currentTarget.value)}>
              <option value="worker">Worker</option>
              <option value="auditor">Auditor</option>
              <option value="unassigned">Unassigned</option>
            </select>
          </label>
          {#if roleValue !== 'unassigned'}
            <label>
              <span>{roleValue === 'worker' ? 'Supervisor thread' : 'Audit target'}</span>
              <input
                aria-label="Related thread"
                list="quick-detail-related-threads"
                placeholder="Thread name or native id"
                value={relatedThreadValue}
                disabled={roleSaving}
                oninput={(event) => (relatedThreadValue = event.currentTarget.value)}
              />
              <datalist id="quick-detail-related-threads">
                {#each relatedThreadOptions as candidate}
                  <option value={relatedLabel(candidate)}></option>
                {/each}
              </datalist>
            </label>
            <label>
              <span>Focus</span>
              <textarea aria-label="Role focus" rows="2" maxlength="500" value={focusValue} disabled={roleSaving} oninput={(event) => (focusValue = event.currentTarget.value)}></textarea>
            </label>
          {/if}
          <p class="quick-detail-role-note">{roleValue === 'unassigned'
            ? 'Clearing the role restores the saved sidebar placement.'
            : 'Saving moves this thread under the related thread. Its original spawn lineage remains in history.'}</p>
          <div class="quick-detail-role-actions">
            <button class="xresume-btn" disabled={roleSaving} onclick={() => (roleEditing = false)}>Cancel</button>
            <button class="xname-save-btn" disabled={roleSaving} onclick={() => void saveRole()}>{roleSaving ? 'Saving…' : 'Save role'}</button>
          </div>
          {#if roleError}<div class="ifile-error">{roleError}</div>{/if}
        </section>
      {/if}
      {#if hostedThreadId && invokerHostedThreadId}
        <section aria-label="Completion notifications">
          <p>Notify the invoker when this thread finishes a turn.</p>
          {#each [['inbox', 'Inbox notices'], ['prompt', 'Prompt notices']] as [channel, label]}
            <button class="xresume-btn" aria-pressed={subscriptions?.[channel] === 'send'} disabled={!subscriptions || subscriptionsSaving} onclick={() => void toggleSubscription(channel)}>{label}: {subscriptions ? subscriptions[channel] === 'send' ? 'On' : 'Off' : 'Loading…'}</button>
          {/each}
          {#if subscriptionsError}<div class="ifile-error" role="alert">{subscriptionsError}</div>{/if}
        </section>
      {/if}
      <div class="quick-detail-actions">
        {#if canClose}<button class="quick-detail-danger" onclick={() => void close()}>{closeLabel}</button>{/if}
        {#if canBulkClose}<button class="quick-detail-danger" onclick={() => void closeTree()}>Bulk close tree</button>{/if}
        {#if hostedThreadId && !assignmentUnavailable && !roleEditing}<button class="xresume-btn" onclick={editRole}>Set role</button>{/if}
        {#if hostedThreadId && invokerHostedThreadId && !assignmentControlsPlacement}<button class="xresume-btn" disabled={placementSaving} onclick={() => void toggleSidebarPlacement()}>{placementSaving ? 'Saving…' : sidebarPlacement === 'standalone' ? 'Nest under invoker' : 'Make standalone'}</button>{/if}
        {#if hostedThreadId}<button class="xresume-btn" disabled={presentationSaving} onclick={() => void togglePresentation()}>{presentationSaving ? 'Saving…' : presentationMode === 'background' ? 'Bring to sidebar' : 'Move to background'}</button>{/if}
        <button class="xresume-btn" onclick={open}>Open in pane</button>
      </div>
      {#if closeError}<div class="ifile-error">{closeError}</div>{/if}
      {#if failedAttachment && failedAttachment.nativeThreadId === nativeId && provider === 'claude'}
        <p>Process state unknown. Forgetting the attachment does not stop Claude.</p>
        <button class="quick-detail-danger" onclick={() => void forgetUnconfirmed()}>Forget attachment anyway</button>
      {/if}
      {#if presentationError}<div class="ifile-error">{presentationError}</div>{/if}
      {#if placementError}<div class="ifile-error">{placementError}</div>{/if}
      </div>
      {#if hostedThreadId}
        <MailPane {hostedThreadId} />
      {/if}
    </div>
  </div>
{/if}
