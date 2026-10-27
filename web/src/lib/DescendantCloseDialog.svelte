<script>
  import { parentCloseLabel } from './descendant_close.js'
  import {
    acknowledgeDescendantCloseFailures,
    cancelDescendantClose,
    descendantClose,
    submitDescendantClose,
    toggleAllCloseDescendants,
    toggleCloseDescendant,
  } from './descendant_close.svelte.js'
  import { closeSelection } from './descendant_close.js'
  import { trapTab } from './modal_focus.js'
  import { tick } from 'svelte'

  const request = $derived(descendantClose.request)
  const selection = $derived(request ? closeSelection(request.nodes, request.selected) : null)
  let selectAll = $state(null)
  let dialogEl = $state(null)
  let openRequest = null
  let opener = null

  $effect(() => {
    if (selectAll && selection) selectAll.indeterminate = selection.indeterminate
  })

  // This is a blocking choice, so the trigger must not retain keyboard focus:
  // Space on it could otherwise issue a second singleton request underneath
  // the dialog. Restore it after Cancel, acknowledgement, or a clean close.
  $effect(() => {
    const current = request
    if (current === openRequest) return
    if (current) {
      openRequest = current
      opener = document.activeElement
      void tick().then(() => dialogEl?.querySelector('input:not(:disabled), button:not(:disabled)')?.focus())
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
      cancelDescendantClose()
    }
  }
</script>

<svelte:window onkeydown={onKeydown} />

{#if request}
  <div class="descendant-close-overlay" role="presentation">
    <div bind:this={dialogEl} class="descendant-close-dialog" role="dialog" aria-modal="true" aria-label="Close descendant agents">
      <h3>{request.provider === 'claude' ? 'End parent session?' : 'Detach parent?'}</h3>
      <p class="descendant-close-copy">Choose which descendant agents to close. Unchecked agents stay open.</p>

      {#if request.nodes.length}
        <label class="descendant-select-all">
          <input
            bind:this={selectAll}
            type="checkbox"
            checked={selection.checked}
            disabled={request.busy || selection.eligible.length === 0}
            aria-label="Select all closable agents"
            onchange={(event) => toggleAllCloseDescendants(event.currentTarget.checked)}
          />
          Select all
        </label>
      {/if}

      {#if request.nodes.length}
        <div class="descendant-close-tree" aria-label="Descendant agents">
          {#each request.nodes as node (node.key)}
            <label class="descendant-close-row" class:inactive={!node.closable} style:padding-left={`${node.depth * 18 + 8}px`}>
              <input
                type="checkbox"
                checked={request.selected.includes(node.sessionId)}
                disabled={!node.closable || request.busy}
                aria-label={`Close ${node.label}`}
                onchange={(event) => toggleCloseDescendant(node.sessionId, event.currentTarget.checked)}
              />
              <span class="t-dot {node.dot?.state ?? ''}"></span>
              <span class="descendant-close-name">
                <span>{node.label}</span>
              </span>
              <span class="descendant-close-activity" class:working={node.working}>{node.activity}</span>
            </label>
          {/each}
        </div>
      {/if}

      {#if selection.workingCount > 0 && request.results.length === 0}
        <p class="descendant-close-warning">{selection.workingCount} selected agent{selection.workingCount === 1 ? ' is' : 's are'} working.</p>
      {/if}
      {#if request.results.length > 0}
        <div class="descendant-close-results" role="alert">
          {#each request.results.filter((result) => !result.ok) as result}
            <div>Could not close {result.label}: {result.error}</div>
          {/each}
        </div>
      {/if}

      <div class="descendant-close-actions">
        {#if request.results.length > 0}
          <button type="button" onclick={acknowledgeDescendantCloseFailures}>Keep remaining agents open</button>
        {:else}
          <button type="button" disabled={request.busy} onclick={cancelDescendantClose}>Cancel</button>
          <button type="button" class="descendant-close-primary" disabled={request.busy} onclick={() => void submitDescendantClose()}>
            {request.busy ? 'Closing…' : parentCloseLabel(request.provider, selection.selectedIds.length)}
          </button>
        {/if}
      </div>
    </div>
  </div>
{/if}
