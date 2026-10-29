<script>
  import { runtimeLabel } from '../shared/lifecycle_state.js'
  import { panes, closeTab, focusTab, isThreadTab, reorderTab } from '../../panes.svelte.js'
  import { app } from '../../app.svelte.js'
  import { cwdTail, displayModel } from '../explorer/explorer_format.js'

  let { pane, canClose = false, onRemovePane, onOpenDetail, onCloseDetail, detailTab = null } = $props()

  function infoFor(tab) {
    return app.sessions.find((session) => session.id === tab.sessionId)
  }

  function status(tab) {
    if (!isThreadTab(tab)) return tab.kind
    const info = infoFor(tab)
    if (!info) return tab.sessionId ? 'detached' : 'history'
    return runtimeLabel(info)
  }

  function close(tabId) {
    const tab = panes.tabs[tabId]
    if (tab?.kind === 'file' && tab.fileDirty && !window.confirm('Close file with unsaved changes?')) return
    closeTab(pane.id, tabId)
  }

  function parentPath(path) {
    const clean = String(path || '').replace(/\/+$/, '')
    const index = clean.lastIndexOf('/')
    return index > 0 ? clean.slice(0, index) : ''
  }

  function startDrag(event, tabId) {
    event.dataTransfer.setData('text/plain', String(tabId))
    event.dataTransfer.effectAllowed = 'move'
    panes.dragTabId = tabId
    event.currentTarget.style.opacity = '0.5'
  }

  function endDrag(event) {
    event.currentTarget.style.opacity = ''
    panes.dragTabId = null
    document.querySelectorAll('.thread-pane-tab.drag-left,.thread-pane-tab.drag-right').forEach((element) => {
      element.classList.remove('drag-left', 'drag-right')
    })
  }

  function dragOver(event) {
    if (!pane.tabIds.includes(panes.dragTabId)) return
    event.preventDefault()
    event.stopPropagation()
    const rect = event.currentTarget.getBoundingClientRect()
    event.currentTarget.classList.toggle('drag-left', event.clientX < rect.left + rect.width / 2)
    event.currentTarget.classList.toggle('drag-right', event.clientX >= rect.left + rect.width / 2)
  }

  function drop(event, targetTabId) {
    const sourceTabId = Number(event.dataTransfer.getData('text/plain'))
    if (!sourceTabId || sourceTabId === targetTabId || !pane.tabIds.includes(sourceTabId)) return
    event.preventDefault()
    event.stopPropagation()
    const before = event.currentTarget.classList.contains('drag-left')
    event.currentTarget.classList.remove('drag-left', 'drag-right')
    reorderTab(pane.id, sourceTabId, targetTabId, before)
  }
</script>

<div class="thread-pane-tabs" role="tablist" aria-label="Open threads and viewers">
  {#each pane.tabIds as tabId (tabId)}
    {@const tab = panes.tabs[tabId]}
    {#if tab}
      {@const model = isThreadTab(tab) ? displayModel(tab.model) : ''}
      <div
        class="thread-pane-tab"
        class:active={pane.activeTabId === tabId && !detailTab}
        role="tab"
        tabindex="0"
        aria-selected={pane.activeTabId === tabId && !detailTab}
        draggable="true"
        onclick={() => focusTab(pane.id, tabId)}
        onkeydown={(event) => {
          if (event.key === 'Enter' || event.key === ' ') {
            event.preventDefault()
            focusTab(pane.id, tabId)
          }
        }}
        ondragstart={(event) => startDrag(event, tabId)}
        ondragend={endDrag}
        ondragover={dragOver}
        ondragleave={(event) => event.currentTarget.classList.remove('drag-left', 'drag-right')}
        ondrop={(event) => drop(event, tabId)}
      >
        {#if tab.kind === 'file'}
          <span class="thread-pane-file-icon" aria-hidden="true">▤</span>
        {:else}
          <span class="thread-pane-dot {status(tab)}"></span>
        {/if}
        <span class="thread-pane-tab-copy">
          <span class="thread-pane-tab-name" title={tab.title}>{tab.title}</span>
          {#if tab.kind === 'file'}
            <span class="thread-pane-tab-meta" title={tab.path || tab.requestedPath}>{tab.path ? parentPath(tab.path) : 'resolving…'}</span>
          {:else if isThreadTab(tab)}
            {#if tab.cwd}<span class="thread-pane-tab-meta" title={tab.cwd}>{cwdTail(tab.cwd)}</span>{/if}
            {#if model}<span class="thread-pane-tab-meta">{model}{tab.effort ? ` · ${tab.effort}` : ''}</span>{/if}
          {/if}
        </span>
        {#if tab.kind === 'file' && tab.fileDirty}<span class="file-dirty-dot"></span>{/if}
        <button
          class="thread-pane-tab-close"
          aria-label="Close {tab.title}"
          onclick={(event) => {
            event.stopPropagation()
            close(tabId)
          }}
        >×</button>
      </div>
    {/if}
  {/each}
  {#if detailTab}
    <div class="thread-pane-tab active thread-pane-detail-tab" role="tab" aria-selected="true">
      <span class="thread-pane-tab-copy"><span class="thread-pane-tab-name">Details · {detailTab.title}</span></span>
      <button class="thread-pane-tab-close" aria-label="Close thread details" onclick={onCloseDetail}>×</button>
    </div>
  {/if}
  <button
    class="thread-pane-detail"
    aria-label="Open thread details in another pane"
    title="Open thread details in another pane"
    disabled={!pane.activeTabId || Boolean(detailTab) || !isThreadTab(panes.tabs[pane.activeTabId])}
    onclick={onOpenDetail}
  >Details</button>
  {#if canClose}
    <button class="thread-pane-close" aria-label="Close pane" title="Close pane" onclick={onRemovePane}>×</button>
  {/if}
</div>
