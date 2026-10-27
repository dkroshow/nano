<script>
  import { onMount, tick } from 'svelte'
  import { TRANSCRIPT_AUTO_PAGE_THRESHOLD } from '../transcript_view.js'

  let { session } = $props()
  const store = $derived(session.store)

  onMount(() => {
    session.connectFeed?.()
    return () => session.disconnectFeed?.()
  })

  let scroller = $state(null)
  let follow = $state(true)

  $effect(() => {
    void store.events.length
    if (scroller && follow) scroller.scrollTop = scroller.scrollHeight
  })

  function onScroll() {
    follow = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 60
    if (scroller.scrollTop >= TRANSCRIPT_AUTO_PAGE_THRESHOLD) return
    if (!store.eventsHasOlder || store.eventsLoadingOlder || store.eventsOlderError) return
    void olderEvents()
  }

  // Older events prepend, which pushes the feed down; compensate scrollTop by
  // the height the page added so the reader keeps their place. Same model as
  // the thread panes.
  async function olderEvents() {
    const previousHeight = scroller?.scrollHeight ?? 0
    await session.loadOlderEvents()
    await tick()
    if (scroller) scroller.scrollTop += scroller.scrollHeight - previousHeight
  }

  function retryOlderEvents() {
    store.eventsOlderError = ''
    void olderEvents()
  }

  function timeOf(ts) {
    return ts.slice(11, 23) // HH:MM:SS.mmm from RFC3339
  }

  function pretty(msg) {
    return JSON.stringify(msg, null, 2)
  }
</script>

<div class="feed-controls">
  <span class="count">{store.events.length} messages</span>
  <span class="spacer"></span>
  <label><input type="checkbox" bind:checked={follow} /> follow</label>
</div>
<div class="feed" bind:this={scroller} onscroll={onScroll}>
  <!-- Older events arrive by scrolling to the top; the note is a progress
       signal, not an affordance. Only a failed page needs a manual retry. -->
  {#if store.eventsLoadingOlder}
    <div class="feed-older-note" role="status">loading older…</div>
  {:else if store.eventsOlderError}
    <div class="feed-older-note" role="status">
      older events failed: {store.eventsOlderError}
      <button class="load-older" type="button" onclick={retryOlderEvents}>Retry</button>
    </div>
  {/if}
  {#each store.events as evt (evt.seq)}
    <details class="evt">
      <summary>
        <span class="t">{timeOf(evt.ts)}</span>
        <span class="dir {evt.dir}">{evt.dir === 'send' ? '→' : '←'}</span>
        <span class="m" class:reply={evt.kind === 'reply'} class:err={evt.kind === 'error'}>
          {#if evt.kind === 'reply' || evt.kind === 'error'}↩{/if}
          {evt.label}
        </span>
        <span class="preview">{evt.preview}</span>
      </summary>
      <pre>{pretty(evt.msg)}</pre>
    </details>
  {/each}
</div>
