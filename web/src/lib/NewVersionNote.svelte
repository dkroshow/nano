<script>
  // The note a tab shows while a new Nano build waits for it; see
  // build_reload.svelte.js. App.svelte places it with the page's other
  // notices, above the Explorer overlay.
  let { buildReload } = $props()
</script>

{#if buildReload.state.pending}
  <div class="new-version-note" role="status">
    <span>Nano was updated —</span>
    <button onclick={() => buildReload.reloadNow()} disabled={buildReload.sending}>Reload</button>
    {#if buildReload.sending}<span class="waiting">Waiting for your message</span>{/if}
  </div>
{/if}

<style>
  .new-version-note {
    pointer-events: auto;
    display: flex;
    gap: 8px;
    align-items: center;
    max-width: calc(100vw - 32px);
    padding: 6px 10px;
    border-radius: 6px;
    background: var(--bg);
    color: var(--text);
    border: 1px solid var(--accent);
    box-shadow: 0 2px 8px rgba(0, 0, 0, 0.2);
  }
  .waiting {
    color: var(--muted);
  }
</style>
