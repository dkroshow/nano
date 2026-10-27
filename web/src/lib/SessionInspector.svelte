<script>
  import EventFeed from './EventFeed.svelte'
  import FileView from './FileView.svelte'
  import ResponsesInput from './ResponsesInput.svelte'
  import { fileTabKey } from '../state.svelte.js'

  let { session, onClose = null } = $props()
  const store = $derived(session.store)
  const primaryTabLabel = $derived(store.provider === 'codex' ? 'Responses Input' : 'Under the Hood')

  function closeFile(file) {
    if (file.dirty && !window.confirm('Close file with unsaved changes?')) return
    session.closeInstructionFile(file.path)
  }
</script>

<section class="pane-right" aria-label="Session inspector">
  <nav class="tabs" aria-label="Session inspector views">
    <button
      class:on={store.rightTab === 'responses'}
      aria-pressed={store.rightTab === 'responses'}
      onclick={() => (store.rightTab = 'responses')}
    >{primaryTabLabel}</button>
    <button
      class:on={store.rightTab === 'feed'}
      aria-pressed={store.rightTab === 'feed'}
      onclick={() => (store.rightTab = 'feed')}
    >Event Feed</button>
    {#each store.openFiles as file (file.path)}
      <button
        class="file-tab"
        class:on={store.rightTab === fileTabKey(file.path)}
        aria-pressed={store.rightTab === fileTabKey(file.path)}
        title={file.path}
        onclick={() => (store.rightTab = fileTabKey(file.path))}
      >
        {#if file.dirty}<span class="file-dirty-dot"></span>{/if}
        <span class="file-tab-name">{file.basename}</span>
        <span
          class="file-tab-close"
          role="button"
          tabindex="-1"
          title="close file"
          onclick={(event) => { event.stopPropagation(); closeFile(file) }}
          onkeydown={(event) => event.stopPropagation()}
        >×</span>
      </button>
    {/each}
    {#if onClose}
      <button
        class="session-inspector-close"
        aria-label="Close Thread Details"
        title="Close Thread Details"
        onclick={onClose}
      >×</button>
    {/if}
  </nav>
  {#if store.rightTab === 'responses'}
    <ResponsesInput {session} />
  {:else if store.rightTab === 'feed'}
    <EventFeed {session} />
  {/if}
  {#each store.openFiles as file (file.path)}
    <FileView {session} {file} visible={store.rightTab === fileTabKey(file.path)} />
  {/each}
</section>
