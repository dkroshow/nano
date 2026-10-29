<script>
  import { runtimeLabel } from '../shared/lifecycle_state.js'
  import MetaValue from './MetaValue.svelte'
  import HistoricalTranscript from '../transcript/HistoricalTranscript.svelte'
  import { openSession } from '../../app.svelte.js'
  import { explorer, select, closeDetail } from '../../explorer.svelte.js'
  import { displayTitle, displayName, neatTime } from './explorer_format.js'
  import { createNameEditor } from './name_editor.svelte.js'
  import { resumeGate, createResumeAction } from './thread_resume.svelte.js'

  // Historical transcript and evidence inspector. The transcript stays
  // read-only (watching is not interacting) — Resume bridges to a hosted
  // session, where interaction happens.
  const detail = $derived(explorer.detail)
  const selected = $derived(explorer.dataset.find((item) => item.id === explorer.selectedId))
  const title = $derived(displayTitle(detail ?? selected ?? {}).text)

  // Resume bridge (Slice R1). Gate from whichever record is at hand; runtime
  // prefers the dataset row — it refetches on every `runtime` SSE nudge, while
  // the detail snapshot is only as fresh as its open.
  const gateItem = $derived(detail ?? selected ?? null)
  const gate = $derived(resumeGate(gateItem))
  const runtime = $derived((selected ?? detail)?.runtime ?? null)
  const threadDiagnostics = $derived(detail?.runtime?.threadDiagnostics ?? selected?.runtime?.threadDiagnostics ?? [])
  const attached = $derived(Boolean(runtime?.attached))
  const resumer = createResumeAction()

  async function resumeThisThread() {
    // The gated record travels whole (Slice R3): its native id is the resume
    // target, its nano catalog id sources the instant durable history in the
    // live view, and its recorded metadata is a claude resume's posture.
    if (await resumer.resume(gateItem)) closeDetail()
  }

  function jumpToSession() {
    openSession(runtime.sessionId)
    closeDetail()
  }

  // The heading is click-to-edit like the list name cell. The editable value is
  // the user-assigned name only (null falls back to the derived title heading).
  const editor = createNameEditor()
  const editId = $derived(detail?.id ?? selected?.id ?? explorer.selectedId)
  const editName = $derived(displayName(detail ?? selected ?? {}))

  function onTitleKey(e) {
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

  const META_FIELDS = [
    ['cwd', 'cwd'],
    ['model', 'model'],
    ['effort', 'effort'],
    ['priority', 'priority'],
    ['permission', 'permission'],
    ['historicalBranch', 'historical branch'],
    ['origin', 'origin'],
  ]

  const SOURCE_STATUS_CLASS = { present: 'ok', source_missing: 'bad', error: 'bad' }

  // Same datetime formatting as the list columns.
  function fmtTime(iso) {
    const t = neatTime(iso)
    return t ? `${t.day} ${t.time}` : '—'
  }
</script>

<div class="xdetail">
	<button class="nw-close-x xdetail-close" aria-label="Close thread detail" title="Close thread detail" onclick={closeDetail}>×</button>

  <div class="xdetail-heading">
    <div>
      <div class="xdetail-title">
        {#if editor.state.editing}
          <input
            class="xname-input xname-input-title"
            type="text"
            aria-label="Rename thread"
            value={editor.state.value}
            disabled={editor.state.saving}
            oninput={(e) => (editor.state.value = e.currentTarget.value)}
            onkeydown={onTitleKey}
            onblur={editor.cancel}
            use:focusOnMount
          />
          {#if editor.state.error}<div class="ifile-error">{editor.state.error}</div>{/if}
        {:else}
          <button
            class="xname-edit xname-edit-title"
            type="button"
            aria-label="Rename thread"
            disabled={!editId}
            onclick={() => editor.begin(editId, editName)}>{title || 'Thread history'}</button
          >
        {/if}
      </div>
      <div class="xdetail-subtitle">
        {#if attached}
          Attached to live session · {runtimeLabel(runtime)} ·
          transcript read only ·
          <button class="xdetail-jump" onclick={jumpToSession}>jump to session</button>
        {:else}
          Historical thread · read only
        {/if}
      </div>
    </div>

    {#if gate}
      <div class="xdetail-actions">
        {#if attached}
          <button class="xresume-btn" data-testid="jump-to-session" onclick={jumpToSession}>Jump to session</button>
        {:else}
          <button
            class="xresume-btn"
            data-testid="resume-thread"
            disabled={!gate.enabled || resumer.state.busy}
            onclick={resumeThisThread}
          >{resumer.state.busy ? 'Resuming…' : 'Resume'}</button>
          {#if !gate.enabled}<div class="xresume-note">{gate.note}</div>{/if}
          {#if resumer.state.error}<div class="ifile-error xresume-error">{resumer.state.error}</div>{/if}
        {/if}
      </div>
    {/if}
  </div>

  {#if threadDiagnostics.length}
    <div class="xdetail-thread-diagnostics" role="alert" data-testid="thread-diagnostics">
      <strong>Thread diagnostics</strong>
      {#each threadDiagnostics as diagnostic}
        <span>{diagnostic.code}: {diagnostic.message}{diagnostic.occurrences > 1 ? ` · ${diagnostic.occurrences} occurrences` : ''}</span>
      {/each}
    </div>
  {/if}

  <div class="xdetail-layout">
    <HistoricalTranscript />

    <aside class="xdetail-evidence" aria-label="Thread evidence">
      {#if explorer.detailState === 'loading'}
        <div class="muted">Loading thread evidence…</div>
      {:else if explorer.detailState === 'error'}
        <div class="ifile-error">{explorer.detailError}</div>
        <button class="load-older" onclick={() => select(explorer.selectedId)}>Retry</button>
      {:else if detail}
        <section class="xsect">
        <div class="xsect-head">Identity</div>
        <dl class="kv">
          {#if detail.name}
            <dt>name</dt>
            <dd class="wrap"><span class="mv-val">{detail.name.value}</span><span class="mv-evidence">{detail.name.evidence}</span><span class="mv-evidence">{detail.name.provenance}</span></dd>
          {/if}
          <dt>provider</dt><dd>{detail.provider}</dd>
          <dt>kind</dt><dd>{detail.kind}</dd>
          <dt>native id</dt><dd class="wrap">{detail.nativeId}</dd>
          <dt>nano id</dt><dd class="wrap">{detail.id}</dd>
          <dt>created</dt><dd>{fmtTime(detail.createdAt)}</dd>
          <dt>updated</dt><dd>{fmtTime(detail.updatedAt)}</dd>
          <dt>archived</dt><dd>{detail.archived ? 'yes' : 'no'}</dd>
        </dl>
        </section>

        <section class="xsect">
        <div class="xsect-head">Metadata</div>
        <dl class="kv">
          {#each META_FIELDS as [key, label]}
            <dt>{label}</dt>
            <dd class="wrap"><MetaValue field={detail.metadata?.[key]} provider={detail.provider} /></dd>
          {/each}
        </dl>
        </section>

        <section class="xsect">
        <div class="xsect-head">Sources <span class="xsect-n">{detail.sources?.length ?? 0}</span></div>
        {#each detail.sources ?? [] as s}
          <div class="xsource">
            <div class="xsource-path">{s.path}</div>
            <dl class="kv">
              <dt>root</dt><dd>{s.rootKind}</dd>
              <dt>status</dt><dd><span class="{SOURCE_STATUS_CLASS[s.status] ?? ''}">{s.status}</span></dd>
              <dt>archived</dt><dd>{s.archived ? 'yes' : 'no'}</dd>
              <dt>generation</dt><dd>{s.generation}</dd>
              {#if s.parserVersion != null}<dt>parser</dt><dd>{s.parserVersion}</dd>{/if}
              {#if s.identityEvidence}<dt>identity</dt><dd>{s.identityEvidence}</dd>{/if}
              <dt>indexed</dt><dd>{fmtTime(s.lastIndexedAt)}</dd>
            </dl>
            {#if s.error}<div class="ifile-error">{s.error}</div>{/if}
            {#if s.diagnostics?.length}
              <ul class="xdiags">
                {#each s.diagnostics as d}<li>{d}</li>{/each}
              </ul>
            {/if}
          </div>
        {:else}
          <div class="muted">No source records.</div>
        {/each}
        </section>
      {/if}
    </aside>
  </div>
</div>
