<script>
  import { panes, closePaneDetail, isThreadTab, moveTabToPane, openDetailForActiveTab, removePane, splitWithTab } from '../panes.svelte.js'
  import PaneTabBar from './PaneTabBar.svelte'
  import FileView from './FileView.svelte'
  import ThreadPaneView from './ThreadPaneView.svelte'
  import ThreadDetailPanel from './thread_detail_panel.svelte'
  import { prefetchThreadDetail } from './thread_detail_cache.js'

  let { pane } = $props()
  let element
  let dropZone = null
  const focused = $derived(panes.activePaneId === pane.id)
  const detailTab = $derived(pane.detail?.tabId ? panes.tabs[pane.detail.tabId] ?? null : null)

  function focusPane() {
    panes.activePaneId = pane.id
  }

  function openFullDetail() {
    const tab = pane.activeTabId ? panes.tabs[pane.activeTabId] : null
    openDetailForActiveTab(pane.id)
    if (tab?.catalogId) prefetchThreadDetail(tab.catalogId)
  }

  function removePaneSafely() {
    const dirtyFiles = pane.tabIds.filter((tabId) => panes.tabs[tabId]?.kind === 'file' && panes.tabs[tabId]?.fileDirty)
    if (dirtyFiles.length && !window.confirm('Close pane and discard unsaved file changes?')) return
    removePane(pane.id)
  }

  function dragOver(event) {
    event.preventDefault()
    const rect = element.getBoundingClientRect()
    const y = (event.clientY - rect.top) / rect.height
    const indicator = element.querySelector('.thread-pane-drop')
    if (y > 0.65 && panes.items.length < 12) {
      indicator.style.display = 'block'
      indicator.style.top = '50%'
      indicator.style.bottom = '0'
      element.classList.remove('drag-over')
      dropZone = 'bottom'
    } else if (y < 0.35 && panes.items.length < 12 && element.parentElement?.classList.contains('thread-pane-stack')) {
      indicator.style.display = 'block'
      indicator.style.top = '0'
      indicator.style.bottom = '50%'
      element.classList.remove('drag-over')
      dropZone = 'top'
    } else {
      indicator.style.display = 'none'
      element.classList.add('drag-over')
      dropZone = 'same'
    }
  }

  function clearDrop() {
    element.classList.remove('drag-over')
    const indicator = element.querySelector('.thread-pane-drop')
    if (indicator) indicator.style.display = 'none'
    dropZone = null
  }

  function drop(event) {
    event.preventDefault()
    const tabId = Number(event.dataTransfer.getData('text/plain'))
    const zone = dropZone
    clearDrop()
    if (!tabId) return
    if (zone === 'bottom') splitWithTab(pane.id, tabId, 'after')
    else if (zone === 'top') splitWithTab(pane.id, tabId, 'before')
    else if (!pane.tabIds.includes(tabId)) moveTabToPane(tabId, pane.id)
  }
</script>

<!-- svelte-ignore a11y_no_noninteractive_element_interactions (the pane surface owns focus and tab drag/drop; its child controls keep their native semantics) -->
<div
  class="thread-pane"
  class:focused
  role="group"
  aria-label="Thread pane"
  bind:this={element}
  onmousedown={focusPane}
  ondragover={dragOver}
  ondragleave={clearDrop}
  ondrop={drop}
>
  <PaneTabBar
    {pane}
    canClose={panes.items.length > 1}
    onRemovePane={removePaneSafely}
    onOpenDetail={openFullDetail}
    onCloseDetail={() => closePaneDetail(pane.id)}
    {detailTab}
  />
  {#each pane.tabIds as tabId (tabId)}
    {@const tab = panes.tabs[tabId]}
    {#if tab}
      {#if tab.kind === 'file'}
        <FileView {tab} paneId={pane.id} visible={pane.activeTabId === tabId && !detailTab} />
      {:else if isThreadTab(tab)}
        <ThreadPaneView
          {tab}
          paneId={pane.id}
          visible={pane.activeTabId === tabId && !detailTab}
          onOpenDetail={openFullDetail}
        />
      {/if}
    {/if}
  {/each}
  {#if detailTab}
    <ThreadDetailPanel tab={detailTab} standalone />
  {:else if !pane.activeTabId}
    <div class="thread-pane-placeholder">Open a thread from the sidebar</div>
  {/if}
  <div class="thread-pane-drop"></div>
</div>
