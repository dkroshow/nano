<script>
  import { tick } from 'svelte'
  import {
    acknowledgeBulkCloseFailure,
    bulkClose,
    cancelBulkClose,
    submitBulkClose,
  } from './bulk_close.svelte.js'
  import { trapTab } from '../shared/modal_focus.js'

  const request = $derived(bulkClose.request)
  const workingCount = $derived(request?.nodes.filter((node) => node.working).length ?? 0)
  const starredCount = $derived(request?.nodes.filter((node) => node.starred).length ?? 0)
  const failures = $derived(request?.results.filter((result) => result.closeAction === 'failed') ?? [])
  let dialogEl = $state(null)
  let openRequest = null
  let opener = null

  $effect(() => {
    const current = request
    if (current === openRequest) return
    if (current) {
      openRequest = current
      opener = document.activeElement
      void tick().then(() => dialogEl?.querySelector('button:not(:disabled)')?.focus())
      return
    }
    if (openRequest) {
      const previous = opener
      openRequest = null
      opener = null
      if (previous instanceof HTMLElement && document.contains(previous)) previous.focus()
    }
  })

  function onKeydown(event) {
    if (!request) return
    if (event.key === 'Tab') trapTab(dialogEl, event)
    else if (event.key === 'Escape' && !request.busy && request.results.length === 0) {
      event.preventDefault()
      cancelBulkClose()
    }
  }
</script>

<svelte:window onkeydown={onKeydown} />

{#if request}
  <div class="descendant-close-overlay" role="presentation">
    <div bind:this={dialogEl} class="descendant-close-dialog" role="dialog" aria-modal="true" aria-label="Bulk close thread tree">
      <h3>Bulk close this tree?</h3>
      <p class="descendant-close-copy">This closes {request.nodes.length} thread{request.nodes.length === 1 ? '' : 's'} in the tree currently shown by Nano.</p>

      <div class="descendant-close-tree" aria-label="Threads that will close">
        {#each request.nodes as node (node.key)}
          <div class="descendant-close-row" style:padding-left={`${node.depth * 18 + 8}px`}>
            <span class="t-dot {node.dot?.state ?? ''}"></span>
            <span class="descendant-close-name">
              <span>{node.label}</span>
              {#if node.starred}<span class="bulk-close-star">★ starred</span>{/if}
            </span>
            <span class="descendant-close-activity" class:working={node.working}>{node.activity}</span>
          </div>
        {/each}
      </div>

      {#if workingCount > 0 && failures.length === 0}
        <p class="descendant-close-warning">{workingCount} thread{workingCount === 1 ? ' is' : 's are'} working and will be interrupted.</p>
      {/if}
      {#if starredCount > 0 && failures.length === 0}
        <p class="descendant-close-warning">{starredCount} thread{starredCount === 1 ? ' is' : 's are'} starred. Closing removes the star{starredCount === 1 ? '' : 's'}.</p>
      {/if}
      {#if failures.length > 0}
        <div class="descendant-close-results" role="alert">
          {#each failures as result}
            <div>{result.error || 'A thread did not close.'}</div>
          {/each}
        </div>
      {/if}

      <div class="descendant-close-actions bulk-close-actions">
        {#if failures.length > 0}
          <button type="button" onclick={acknowledgeBulkCloseFailure}>Dismiss</button>
        {:else}
          <button type="button" disabled={request.busy} onclick={cancelBulkClose}>Cancel</button>
          <button type="button" class="descendant-close-primary" disabled={request.busy} onclick={() => void submitBulkClose()}>{request.busy ? 'Closing…' : 'Close tree'}</button>
        {/if}
      </div>
    </div>
  </div>
{/if}
