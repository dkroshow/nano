<script>
  import { runtimeLabel as lifecycleLabel } from '../shared/lifecycle_state.js'
  import { untrack } from 'svelte'
  import MetaValue from './MetaValue.svelte'
  import { app } from '../../app.svelte.js'
  import { explorer } from '../../explorer.svelte.js'
  import { getSessionStore } from '../../state.svelte.js'
  import { displayTitle, displayName, neatTime } from './explorer_format.js'
  import { createNameEditor } from './name_editor.svelte.js'
  import {
    resumeGate,
    resolveThreadIdentity,
    createResumeAction,
    createDetachAction,
    detachLabels,
  } from './thread_resume.svelte.js'
  import ResponsesInput from '../status/ResponsesInput.svelte'
  import HistoricalSetup from './HistoricalSetup.svelte'
  import { cachedThreadDetail, refreshThreadDetail } from './thread_detail_cache.js'

  // This is intentionally tab-scoped. The former detail view used Explorer's
  // single selection, which could show one tab's evidence beside another tab.
  let { tab, standalone = false } = $props()

  let fetchedDetail = $state(null)
  const session = $derived(app.sessions.find((candidate) => candidate.id === tab.sessionId) ?? null)
  // Which thread this panel describes. The live session's own native id, else
  // the one the tab adopted, is the truth: a claude `--resume` successor
  // announces a new id while tab.catalogId still names the predecessor's row,
  // and a panel keyed on that row would offer Resume on the predecessor.
  const identity = $derived(
    resolveThreadIdentity({
      nativeId: session?.threadId || tab.nativeId,
      catalogId: tab.catalogId,
      provider: tab.provider || session?.provider,
      dataset: explorer.dataset,
    }),
  )
  const detail = $derived(fetchedDetail ?? (identity.rowId ? cachedThreadDetail(identity.rowId) : null))
  let loadState = $state('idle')
  let loadError = $state('')

  const item = $derived(identity.row)
  const liveSession = $derived(session ? untrack(() => getSessionStore(session.id)) : null)
  // The list receives name changes and runtime nudges before a detail fetch is
  // repeated. Prefer that fresh name while retaining the loaded source detail.
  const record = $derived(detail ? { ...detail, name: item?.name ?? detail.name } : item)
  const provider = $derived(identity.provider || record?.provider || session?.provider || '')
  const nativeId = $derived(identity.nativeId || record?.nativeId || '')
  const runtime = $derived(item?.runtime ?? detail?.runtime ?? null)
  const attached = $derived(Boolean(session || runtime?.attached))
  const title = $derived(displayTitle(record ?? { title: tab.title, nativeId }).text || tab.title || 'Thread')
  const editId = $derived(record?.transient ? '' : record?.id ?? '')
  const editor = createNameEditor()
  const resumer = createResumeAction()
  const detacher = createDetachAction()
  const detachUi = $derived(detachLabels(provider))
  // The tab's own session, or whichever session the runtime overlay says holds
  // the thread (a historical tab can describe a thread attached elsewhere).
  const attachedSessionId = $derived(session?.id ?? runtime?.sessionId ?? '')
  // One record for both the gate and the request: the loaded detail (or list
  // row) with this panel's resolved identity laid over it. The nano id comes
  // from that identity, never from editId — a row for a different native id
  // would hand the resumed session the predecessor's transcript.
  const target = $derived({ ...(record ?? {}), provider, nativeId, id: identity.rowId })
  const gate = $derived(resumeGate(target))

  const META_FIELDS = [
    ['cwd', 'cwd'], ['model', 'model'], ['effort', 'effort'], ['priority', 'priority'],
    ['permission', 'permission'], ['historicalBranch', 'historical branch'], ['origin', 'origin'],
  ]
  const SOURCE_STATUS_CLASS = { present: 'ok', source_missing: 'bad', error: 'bad' }
  const CATALOG_REVALIDATE_DEBOUNCE_MS = 800

  // A transient tab is intentionally not fetched: mem_* is not a durable
  // detail resource. Its known runtime values render with runtime evidence
  // until the catalog row appears and the tab is upgraded.
  function transientField(key) {
    const values = { cwd: tab.cwd || session?.cwd, model: tab.model || session?.model, effort: tab.effort }
    const value = values[key]
    return value ? { state: 'known', value, evidence: 'runtime' } : { state: 'unknown' }
  }

  function field(key) {
    return record?.metadata?.[key] ?? transientField(key)
  }

  function fmtTime(iso) {
    const t = neatTime(iso)
    return t ? `${t.day} ${t.time}` : '—'
  }

  function runtimeLabel() {
    if (attached) {
      if (session?.status === 'starting') return 'restoring Codex session…'
      if (session?.status === 'exited') return session?.error ? `restore failed · ${session.error}` : 'restore failed'
      return `attached · ${lifecycleLabel(session ?? runtime)}`
    }
    if (runtime?.detached || tab.detached) return 'detached · durable history'
    return 'historical · durable history'
  }

  async function resume() {
    await resumer.resume(target)
  }

  async function detach() {
    await detacher.detach(attachedSessionId, target)
  }

  function onTitleKey(event) {
    if (event.key === 'Enter') {
      event.preventDefault()
      editor.commit()
    } else if (event.key === 'Escape') {
      event.preventDefault()
      editor.cancel()
    }
  }

  function focusOnMount(node) {
    node.focus()
    node.select()
  }

  // Revalidation hands the template a fresh object on every response. Holding
  // the existing one when nothing changed keeps derived values referentially
  // stable, so a catalog tick does not re-render the detail view.
  function applyDetail(body) {
    if (fetchedDetail && JSON.stringify(fetchedDetail) === JSON.stringify(body)) return
    fetchedDetail = body
  }

  // Loading state is scoped to the thread identity alone. A catalog tick must
  // never reset it — that is what made an open panel flash back through its
  // loading shape while the operator was reading it.
  $effect(() => {
    const id = identity.rowId
    const cached = id ? cachedThreadDetail(id) : null
    fetchedDetail = cached
    loadError = ''
    if (!id) {
      loadState = 'idle'
      return
    }
    let cancelled = false
    let retryTimer = null
    loadState = cached ? 'idle' : 'loading'

    const revalidate = async (attempt = 0) => {
      try {
        const body = await refreshThreadDetail(id)
        if (cancelled) return
        applyDetail(body)
        loadState = 'idle'
        const resolutionState = body?.currentResolution?.state
        if ((resolutionState === 'resolving' || resolutionState === 'stale') && attempt < 8) {
          const delay = Math.min(4000, 250 * (2 ** attempt))
          retryTimer = setTimeout(() => void revalidate(attempt + 1), delay)
        }
      } catch (error) {
        if (!cancelled) {
          loadState = 'error'
          loadError = error?.message ?? String(error)
        }
      }
    }
    void revalidate()
    return () => {
      cancelled = true
      if (retryTimer) clearTimeout(retryTimer)
    }
  })

  // A catalog tick (a rename, a resume, a turn starting) revalidates in place:
  // no spinner, no reset, and no swap unless the response actually differs.
  // Debounced because a resume nudges the catalog in bursts, and each pass
  // costs a full detail fetch plus a deep compare — undebounced, that work
  // lands on the main thread often enough to be felt while typing.
  $effect(() => {
    void explorer.catalogVersion
    const id = identity.rowId
    if (!id) return
    let cancelled = false
    const timer = setTimeout(() => {
      void refreshThreadDetail(id)
        .then((body) => {
          if (!cancelled) applyDetail(body)
        })
        .catch(() => {
          // The identity-scoped load above owns the durable error surface.
        })
    }, CATALOG_REVALIDATE_DEBOUNCE_MS)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  })
</script>

<aside class="thread-detail-panel" class:standalone aria-label="Thread detail">
  {#if identity.rowId && !detail && !liveSession}
    <div class="thread-detail-scroll">
      {#if loadState === 'error'}
        <div class="ifile-error">{loadError}</div>
      {:else}
        <div class="muted">Loading full details…</div>
      {/if}
    </div>
  {:else}
  <header class="thread-detail-heading">
    <div>
      <div class="thread-detail-title">
        {#if editor.state.editing}
          <input
            class="xname-input xname-input-title"
            type="text"
            aria-label="Rename thread"
            value={editor.state.value}
            disabled={editor.state.saving}
            oninput={(event) => (editor.state.value = event.currentTarget.value)}
            onkeydown={onTitleKey}
            onblur={editor.cancel}
            use:focusOnMount
          />
          {#if editor.state.error}<div class="ifile-error">{editor.state.error}</div>{/if}
        {:else if editId}
          <button class="xname-edit xname-edit-title" aria-label="Rename thread" onclick={() => editor.begin(editId, displayName(record ?? {}))}>{title}</button>
        {:else}
          <span>{title}</span>
        {/if}
      </div>
      <div class="thread-detail-subtitle">{runtimeLabel()}</div>
    </div>
    <!-- The action slot stays mounted once a thread is resumable, so the button
         leaving on attach does not resize the header under the pointer. -->
    {#if gate || attached}
      <div class="thread-detail-action">
        {#if attached}
          {#if attachedSessionId}
            <button class="xdetach-btn" disabled={detacher.state.busy} title={detachUi.title} onclick={detach}>{detacher.state.busy ? detachUi.busy : detachUi.label}</button>
            {#if detacher.state.error}<div class="ifile-error xresume-error">{detacher.state.error}</div>{/if}
          {/if}
        {:else if gate}
          <button class="xresume-btn" disabled={!gate.enabled || resumer.state.busy} onclick={resume}>{resumer.state.busy ? 'Resuming…' : 'Resume'}</button>
          {#if !gate.enabled}<div class="xresume-note">{gate.note}</div>{/if}
          {#if resumer.state.error}<div class="ifile-error xresume-error">{resumer.state.error}</div>{/if}
        {/if}
      </div>
    {/if}
  </header>

  <div class="thread-detail-scroll">
    {#if loadState === 'loading' && !record}
      <div class="muted">Loading thread detail…</div>
    {:else if loadState === 'error' && !record}
      <div class="ifile-error">{loadError}</div>
    {/if}

    <!-- Current setup outlives the resume it describes. Keeping it mounted (and
         above the live section) means attaching only appends below the reader's
         scroll position instead of tearing down the section they were reading,
         along with whichever disclosures they had opened. -->
    {#if provider === 'codex' || provider === 'claude'}
      <HistoricalSetup resolution={detail?.currentResolution ?? null} recorded={record?.metadata ?? null} {provider} />
    {/if}

    {#if liveSession}
      <section class="thread-live-detail">
        <div class="xsect-head">Live session</div>
        <ResponsesInput session={liveSession} />
      </section>
    {/if}

    <section class="xsect">
      <div class="xsect-head">Identity</div>
      <dl class="kv">
        {#if record?.name}
          <dt>name</dt><dd class="wrap"><span class="mv-val">{record.name.value}</span><span class="mv-evidence">{record.name.evidence}</span><span class="mv-evidence">{record.name.provenance}</span></dd>
        {/if}
        <dt>provider</dt><dd>{provider || 'unknown'}</dd>
        <dt>kind</dt><dd>{record?.kind ?? 'unknown'}</dd>
        <dt>native id</dt><dd class="wrap">{nativeId || 'unknown'}</dd>
        {#if editId}<dt>nano id</dt><dd class="wrap">{editId}</dd>{/if}
        <dt>created</dt><dd>{fmtTime(record?.createdAt ?? session?.createdAt)}</dd>
        <dt>updated</dt><dd>{fmtTime(record?.updatedAt ?? session?.updatedAt)}</dd>
        <dt>archived</dt><dd>{record ? (record.archived ? 'yes' : 'no') : 'unknown'}</dd>
      </dl>
    </section>

    {#if provider !== 'codex' && provider !== 'claude'}
      <section class="xsect">
        <div class="xsect-head">Metadata</div>
        <dl class="kv">
          {#each META_FIELDS as [key, label]}
            <dt>{label}</dt><dd class="wrap"><MetaValue field={field(key)} {provider} /></dd>
          {/each}
        </dl>
      </section>
    {/if}

    <section class="xsect">
      <div class="xsect-head">Sources <span class="xsect-n">{record?.sources?.length ?? 0}</span></div>
      {#each record?.sources ?? [] as source}
        <div class="xsource">
          <div class="xsource-path">{source.path}</div>
          <dl class="kv">
            <dt>root</dt><dd>{source.rootKind}</dd>
            <dt>status</dt><dd><span class={SOURCE_STATUS_CLASS[source.status] ?? ''}>{source.status}</span></dd>
            <dt>archived</dt><dd>{source.archived ? 'yes' : 'no'}</dd>
            <dt>generation</dt><dd>{source.generation}</dd>
            {#if source.parserVersion}<dt>parser</dt><dd>{source.parserVersion}</dd>{/if}
            {#if source.identityEvidence}<dt>identity</dt><dd>{source.identityEvidence}</dd>{/if}
            <dt>indexed</dt><dd>{fmtTime(source.lastIndexedAt)}</dd>
          </dl>
          {#if source.error}<div class="ifile-error">{source.error}</div>{/if}
          {#if source.diagnostics?.length}<ul class="xdiags">{#each source.diagnostics as diagnostic}<li>{diagnostic}</li>{/each}</ul>{/if}
        </div>
      {:else}
        <div class="muted">{record?.transient || !record ? 'No durable source record yet.' : 'No source records.'}</div>
      {/each}
    </section>
  </div>
  {/if}
</aside>
