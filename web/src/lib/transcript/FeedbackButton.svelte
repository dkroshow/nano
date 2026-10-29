<script>
  // The operator's bug note. Save first uploads this tab's diagnostic ring
  // through the ordinary diagnostics path, so Nano can keep a copy of that
  // evidence with the note.
  import { tick } from 'svelte'
  import { browserDiagnosticIdentity, recordBrowserDiagnostic, uploadBrowserDiagnosticRing } from '../../browser_diagnostics.js'

  // context() returns the pane's thread identity, view and pane state at the
  // moment of saving.
  let { context } = $props()

  let open = $state(false)
  let note = $state('')
  let saving = $state(false)
  let error = $state('')
  let confirmation = $state('')
  let clientActionId = ''
  let clientActionText = ''
  let confirmationTimer = null
  let textareaEl = $state(null)

  function toggle() {
    open = !open
    error = ''
    if (open) {
      confirmation = ''
      void tick().then(() => textareaEl?.focus())
    }
  }

  async function save() {
    const text = note.trim()
    if (!text || saving) return
    saving = true
    error = ''
    // One id per note text, kept across retries, so a retry never saves twice.
    if (text !== clientActionText) {
      clientActionId = `feedback-${globalThis.crypto?.randomUUID?.() ?? Date.now()}`
      clientActionText = text
    }
    const fields = context()
    try {
      // Nano copies the evidence from what this upload leaves it holding, so
      // a note is never saved without it.
      recordBrowserDiagnostic('feedback.save', {
        hostedThreadId: fields.hostedThreadId, threadId: fields.threadId, provider: fields.provider,
        paneId: fields.paneId, view: fields.view,
      })
      const bootId = await uploadBrowserDiagnosticRing()
      if (!bootId) throw new Error('the browser logs could not be sent to Nano')
      const response = await fetch('/api/v1/feedback', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ clientActionId, note: text, bootId, ...fields, ...browserDiagnosticIdentity() }),
      })
      const body = await response.json().catch(() => null)
      if (!response.ok) throw new Error(body?.error?.message || `HTTP ${response.status}`)
      const savedAt = new Date(body.createdAt).toLocaleString([], { dateStyle: 'medium', timeStyle: 'medium' })
      confirmation = `Feedback saved at ${savedAt}`
      note = ''
      clientActionText = ''
      open = false
      clearTimeout(confirmationTimer)
      confirmationTimer = setTimeout(() => (confirmation = ''), 10_000)
    } catch (failure) {
      error = `Feedback not saved: ${failure.message}`
    } finally {
      saving = false
    }
  }

  function onKeydown(event) {
    if (event.key === 'Escape') {
      open = false
    } else if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) {
      event.preventDefault()
      void save()
    }
  }
</script>

<span class="feedback">
  <button class="transcript-audit-button" aria-expanded={open} onclick={toggle}>Feedback</button>
  {#if confirmation}<span class="view-hint" role="status">{confirmation}</span>{/if}
  {#if open}
    <div class="feedback-box">
      <textarea
        bind:this={textareaEl}
        bind:value={note}
        aria-label="Feedback note"
        placeholder="What went wrong?"
        rows="4"
        onkeydown={onKeydown}
      ></textarea>
      {#if error}<div class="feedback-error" role="alert">{error}</div>{/if}
      <div class="feedback-actions">
        <button onclick={() => (open = false)}>Cancel</button>
        <button class="feedback-save" disabled={saving || !note.trim()} onclick={save}>{saving ? 'Saving…' : 'Save'}</button>
      </div>
    </div>
  {/if}
</span>
