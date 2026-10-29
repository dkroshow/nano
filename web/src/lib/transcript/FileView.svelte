<script>
  import { tick } from 'svelte'
  import { openFileTab, reconcileFileTab, setFileTabDirty } from '../../panes.svelte.js'
  import { fileReferenceLinks } from './file_references.js'
  import { md as renderMarkdown } from './markdown.js'
  import { unwrapMarkdownProse } from './markdown_reflow.js'

  const WORD_WRAP_KEY = 'nano:file-word-wrap'

  let { session = null, file = null, tab = null, paneId = null, visible = true } = $props()

  const general = $derived(tab?.kind === 'file')
  const api = $derived(general ? '/api/v1/files' : `/api/sessions/${session.id}/instruction-files`)
  const path = $derived(general ? (tab.path || tab.requestedPath) : file.path)
  const loadIdentity = $derived(general ? `file:${tab.id}` : `instruction:${path}`)

  let loading = $state(true)
  let error = $state('')
  // Candidate paths returned when a link matches more than one file; the
  // operator picks the right one and this same tab reloads against it.
  let candidates = $state([])
  let saveError = $state('')
  let content = $state('')
  let savedContent = $state('')
  let fileName = $state('')
  let editable = $state(false)
  let readOnlyReason = $state('')
  let provenLoaded = $state(true)
  let evidence = $state('wire-authoritative')
  let mtime = $state(null)
  let rawView = $state(false)
  let editing = $state(false)
  let saving = $state(false)
  let externalChange = $state(false)
  let lineNotice = $state('')
  let wordWrap = $state(readWordWrapPreference())
  let loadedKey = ''
  let rootEl

  const dirty = $derived(editing && content !== savedContent)

  function setDirty(value) {
    if (general) setFileTabDirty(tab.id, value)
    else file.dirty = Boolean(value)
  }

  function readWordWrapPreference() {
    try {
      return localStorage.getItem(WORD_WRAP_KEY) !== 'off'
    } catch {
      return true
    }
  }

  function toggleWordWrap() {
    wordWrap = !wordWrap
    try {
      localStorage.setItem(WORD_WRAP_KEY, wordWrap ? 'on' : 'off')
    } catch {
      // Wrapping still works for this tab when storage is unavailable.
    }
  }

  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;')
  }

  function fileExt(name) {
    const i = String(name || '').lastIndexOf('.')
    return i >= 0 ? String(name).slice(i + 1).toLowerCase() : ''
  }

  function isMarkdown(name) {
    return fileExt(name) === 'md'
  }

  function mdFile(text) {
    return renderMarkdown(text)
  }

  function highlightMdRaw(text) {
    const lines = String(text || '').split('\n')
    const out = []
    let inFence = false
    for (const line of lines) {
      const e = esc(line)
      if (/^\s*```/.test(line)) {
        inFence = !inFence
        out.push('<span class="md-fence">' + e + '</span>')
      } else if (inFence) {
        out.push('<span class="md-fenced">' + e + '</span>')
      } else if (/^(#{1,6})\s/.test(line)) {
        const m = line.match(/^(#{1,6})\s/)
        out.push('<span class="md-hm">' + esc(m[1]) + '</span> <span class="md-h">' + esc(line.slice(m[0].length)) + '</span>')
      } else if (/^>/.test(line)) {
        out.push('<span class="md-blockquote">' + e + '</span>')
      } else if (/^(\s*)([-*+]|\d+\.)\s/.test(line)) {
        const m = line.match(/^(\s*)([-*+]|\d+\.)\s/)
        out.push(esc(m[1]) + '<span class="md-bullet">' + esc(m[2]) + '</span> ' + esc(line.slice(m[0].length)))
      } else if (/^(\|.*\|)\s*$/.test(line)) {
        out.push(e.replace(/\|/g, '<span class="md-pipe">|</span>'))
      } else {
        out.push(e)
      }
    }
    return out.join('\n')
  }

  function highlightCode(text, ext) {
    const kw = /\b(function|const|let|var|return|if|else|for|while|class|import|export|from|async|await|def|struct|type|func|package|interface|select|case|switch|break|continue|true|false|null|nil|None)\b/g
    return String(text || '').split('\n').map((line) => {
      const trimmed = line.trimStart()
      let e = esc(line)
      if (trimmed.startsWith('//') || trimmed.startsWith('#')) return '<span class="code-comment">' + e + '</span>'
      e = e.replace(/(&quot;.*?&quot;|&#39;.*?&#39;)/g, '<span class="code-string">$1</span>')
      if (!['txt', 'log', 'csv'].includes(ext)) e = e.replace(kw, '<span class="code-kw">$&</span>')
      return e
    }).join('\n')
  }

  function bodyHtml() {
    if (loading) return '<div class="file-status">Loading…</div>'
    if (error) return '<div class="file-status">' + esc(error) + '</div>'
    if (isMarkdown(fileName) && !rawView) {
      return '<div class="fb-reader"><div class="fb-reader-body">' + mdFile(content) + '</div></div>'
    }
    if (isMarkdown(fileName)) {
      return sourceLinesHtml(content, 'md-raw', highlightMdRaw)
    }
    return sourceLinesHtml(content, 'code-view', (value) => highlightCode(value, fileExt(fileName)))
  }

  function sourceLinesHtml(text, className, highlighter) {
    const highlighted = String(highlighter(String(text || '')) || '').split('\n')
    return '<div class="' + className + '">' + highlighted.map((line, index) =>
      '<div class="file-source-line" data-line="' + (index + 1) + '">' + (line || ' ') + '</div>'
    ).join('') + '</div>'
  }

  function markEdited(value) {
    content = value
    setDirty(content !== savedContent)
    saveError = ''
  }

  function unwrapEditorProse() {
    if (!isMarkdown(fileName) || saving || loading || error) return
    const next = unwrapMarkdownProse(content)
    if (next === content) return
    markEdited(next)
    tick().then(() => rootEl?.querySelector('.file-editor')?.focus())
  }

  async function readFileFromServer() {
    const query = new URLSearchParams({ path })
    if (general) {
      if (tab.cwd) query.set('cwd', tab.cwd)
      if (tab.catalogThreadId) query.set('threadId', tab.catalogThreadId)
      if (tab.sessionId) query.set('sessionId', tab.sessionId)
    }
    const r = await fetch(`${api}/read?${query}`, { cache: 'no-store' })
    const data = await r.json().catch(async () => ({ error: { message: await r.text() } }))
    if (!r.ok) {
      const failure = new Error(data?.error?.message || 'Cannot read file')
      failure.candidates = Array.isArray(data?.candidates) ? data.candidates : []
      throw failure
    }
    return general ? { file: data, content: data.content } : data
  }

  function applyRead(data) {
    content = data.content || ''
    savedContent = content
    fileName = data.file?.basename || fileName
    editable = !!data.file?.editable
    readOnlyReason = data.file?.readOnlyReason || ''
    provenLoaded = data.file?.provenLoaded !== false
    evidence = data.file?.evidence || (provenLoaded ? 'wire-authoritative' : 'present-only')
    mtime = Number.isFinite(Number(data.file?.mtime)) ? Number(data.file.mtime) : null
    setDirty(false)
    if (general) {
      reconcileFileTab(tab.id, data.file)
      if (tab.line && isMarkdown(data.file?.basename)) rawView = true
    }
  }

  async function loadFile() {
    if (!path || loadedKey === loadIdentity) return
    loadedKey = loadIdentity
    loading = true
    error = ''
    saveError = ''
    candidates = []
    fileName = general ? (tab.basename || tab.title || '') : (file.basename || '')
    editing = false
    saving = false
    externalChange = false
    setDirty(false)
    try {
      applyRead(await readFileFromServer())
    } catch (e) {
      error = e?.message || 'Error loading file'
      candidates = general && Array.isArray(e?.candidates) ? e.candidates : []
    } finally {
      loading = false
    }
  }

  // Reload this tab against a chosen candidate. The absolute path resolves on
  // its own, so the authoring directory is dropped from the request.
  function chooseCandidate(candidate) {
    if (!general || !candidate) return
    tab.requestedPath = candidate
    tab.cwd = ''
    tab.basename = candidate.split('/').filter(Boolean).at(-1) || tab.basename
    tab.title = tab.basename
    loadedKey = ''
    error = ''
    candidates = []
    void loadFile()
  }

  function toggleEdit() {
    if (editing) {
      if (dirty && !window.confirm('Discard unsaved changes?')) return
      content = savedContent
      setDirty(false)
      editing = false
      saveError = ''
    } else {
      editing = true
      tick().then(() => rootEl?.querySelector('.file-editor')?.focus())
    }
  }

  async function saveFile({ force = false } = {}) {
    if (!editing || saving || loading || error) return
    saving = true
    saveError = ''
    try {
      const payload = { path, content }
      if (!force && mtime != null) payload.mtime = mtime
      const r = await fetch(`${api}/write`, {
        method: 'PUT',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(payload),
      })
      if (r.status === 409) {
        if (window.confirm('File was modified on disk. Overwrite anyway?')) {
          saving = false
          await saveFile({ force: true })
          return
        }
        // Declined: keep the stale mtime so the next Save 409s and re-prompts
        // instead of silently overwriting the concurrent edit.
        externalChange = true
        return
      }
      const data = await r.json().catch(async () => ({ error: { message: await r.text() } }))
      if (!r.ok) throw new Error(data?.error?.message || 'Save failed')
      const meta = general ? data : data.file
      mtime = Number.isFinite(Number(meta?.mtime)) ? Number(meta.mtime) : mtime
      savedContent = content
      setDirty(false)
      editing = false
      externalChange = false
    } catch (e) {
      saveError = e?.message || 'Save failed'
    } finally {
      saving = false
    }
  }

  async function reloadFromDisk() {
    if (dirty && !window.confirm('Discard unsaved changes and reload from disk?')) return
    try {
      applyRead(await readFileFromServer())
      editing = false
      externalChange = false
      saveError = ''
    } catch (e) {
      saveError = e?.message || 'Reload failed'
    }
  }

  function dismissExternalChange() {
    externalChange = false
  }

  async function checkMtime() {
    if (!visible || loading || error || mtime == null) return
    try {
      const r = await fetch(`${api}/mtime?path=${encodeURIComponent(path)}`, { cache: 'no-store' })
      if (!r.ok) return
      const data = await r.json().catch(() => ({}))
      const next = Number(data.mtime)
      if (!Number.isFinite(next) || Math.abs(next - mtime) <= 0.01) return
      if (dirty || editing) externalChange = true
      else await reloadFromDisk()
    } catch {
      // Ignore transient mtime checks.
    }
  }

  function parentDirectory(value) {
    const clean = String(value || '').replace(/\/+$/, '')
    const index = clean.lastIndexOf('/')
    return index > 0 ? clean.slice(0, index) : ''
  }

  function openNestedFile(reference) {
    openFileTab(reference, {
      cwd: parentDirectory(path),
      catalogThreadId: general ? tab.catalogThreadId : '',
      sessionId: general ? tab.sessionId : (session?.id || ''),
    }, paneId)
  }

  async function navigateToLine() {
    if (!general || !visible || !tab.line || loading || error) return
    lineNotice = ''
    const line = Number(tab.line)
    const lines = content.split('\n')
    if (line < 1 || line > lines.length) {
      lineNotice = `Line ${line} is outside this ${lines.length}-line file.`
      return
    }
    if (editing) {
      const editor = rootEl?.querySelector('.file-editor')
      const offset = lines.slice(0, line - 1).reduce((total, value) => total + value.length + 1, 0)
      editor?.focus()
      if (editor) editor.selectionStart = editor.selectionEnd = offset
      return
    }
    if (isMarkdown(fileName)) rawView = true
    await tick()
    const target = rootEl?.querySelector(`[data-line="${line}"]`)
    if (!target) return
    target.classList.add('file-line-target')
    target.scrollIntoView?.({ block: 'center' })
    window.setTimeout(() => target.classList.remove('file-line-target'), 1800)
  }

  function onEditorKeydown(e) {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') {
      e.preventDefault()
      saveFile()
      return
    }
    if (e.key === 'Tab' && !e.metaKey && !e.ctrlKey && !e.altKey) {
      e.preventDefault()
      const target = e.currentTarget
      const start = target.selectionStart
      const end = target.selectionEnd
      markEdited(target.value.slice(0, start) + '  ' + target.value.slice(end))
      tick().then(() => {
        target.selectionStart = target.selectionEnd = start + 2
      })
    }
  }

  $effect(() => {
    if (visible) loadFile()
  })

  $effect(() => {
    if (!visible || loading || error) return
    const timer = window.setInterval(checkMtime, 5000)
    return () => window.clearInterval(timer)
  })

  let handledNavigation = 0
  $effect(() => {
    const nonce = general ? Number(tab.navigationNonce || 0) : 0
    if (!nonce || nonce === handledNavigation || !visible || loading || error) return
    handledNavigation = nonce
    void navigateToLine()
  })
</script>

<div class="file-view" style={visible ? '' : 'display:none'} bind:this={rootEl}>
  <div class="file-tab-toolbar">
    <span class="file-tab-path" title={path}>
      {#if dirty}<span class="file-dirty-dot"></span>{/if}
      {path}
    </span>
    {#if isMarkdown(fileName) && !loading && !error && !editing}
      <div class="file-tab-toggle">
        <button class:active={!rawView} onclick={() => (rawView = false)}>Formatted</button>
        <button class:active={rawView} onclick={() => (rawView = true)}>Raw</button>
      </div>
    {/if}
    {#if editing}
      <button
        class="file-format-btn"
        class:active={wordWrap}
        aria-pressed={wordWrap}
        title="Visually wrap long lines to the pane width"
        onclick={toggleWordWrap}
      >Wrap</button>
      {#if isMarkdown(fileName)}
        <button
          class="file-format-btn"
          disabled={saving}
          title="Unwrap hard line breaks in prose paragraphs"
          onclick={unwrapEditorProse}
        >Unwrap</button>
      {/if}
      <button
        class="file-save-btn"
        class:dirty
        class:saving
        disabled={saving}
        onclick={() => saveFile()}
      >{saving ? 'Saving…' : 'Save'}</button>
    {/if}
    {#if editable && !loading && !error}
      <button class="file-edit-btn" class:active={editing} onclick={toggleEdit}>{editing ? 'View' : 'Edit'}</button>
    {/if}
  </div>
  {#if !editable && !loading && !error && readOnlyReason}
    <div class="file-readonly-note">view-only · {readOnlyReason}</div>
  {/if}
  {#if !provenLoaded && !loading && !error}
    <div class="file-readonly-note subtle">
      {#if evidence === 'documented-expected'}
        documented Claude Code instruction input; not wire-attested
      {:else}
        present on disk, not proven-loaded into this thread
      {/if}
    </div>
  {/if}
  {#if externalChange}
    <div class="file-external-change">
      <span>File changed on disk</span>
      <button onclick={reloadFromDisk}>Reload</button>
      <button class="secondary" onclick={dismissExternalChange}>Dismiss</button>
    </div>
  {/if}
  {#if saveError}
    <div class="file-external-change">
      <span>{saveError}</span>
      <button class="secondary" onclick={() => (saveError = '')}>Dismiss</button>
    </div>
  {/if}
  {#if lineNotice}<div class="file-line-notice">{lineNotice}</div>{/if}
  {#if candidates.length}
    <div class="file-candidates">
      <div class="file-candidates-label">{candidates.length} files match this link — open which one?</div>
      {#each candidates as candidate (candidate)}
        <button class="file-candidate" title={candidate} onclick={() => chooseCandidate(candidate)}>{candidate}</button>
      {/each}
    </div>
  {/if}
  {#if editing}
    <div class="file-editor-wrap">
      <textarea
        class="file-editor"
        class:no-wrap={!wordWrap}
        spellcheck="false"
        wrap={wordWrap ? 'soft' : 'off'}
        value={content}
        oninput={(e) => markEdited(e.currentTarget.value)}
        onkeydown={onEditorKeydown}
      ></textarea>
    </div>
  {:else}
    <div class="file-body" use:fileReferenceLinks={{ onOpen: openNestedFile }}>{@html bodyHtml()}</div>
  {/if}
</div>
