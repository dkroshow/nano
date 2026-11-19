<script>
  import { explorer } from '../../explorer.svelte.js'
  import { openSidebarRow } from '../../app.svelte.js'
  import { displayTitle, sidebarAge, shortNativeId } from './explorer_format.js'

  // How many agent processes Nano is holding right now, in the brand row so it
  // is always in view: a leak of idle attached threads once filled memory and
  // froze the machine. "Attached" is the catalog's runtime overlay, the same
  // fact that draws a row's live dot, so the count moves on the attach and
  // detach events the catalog stream already carries.
  const PROVIDER_LABEL = { claude: 'Claude', codex: 'Codex', grok: 'Grok' }

  let open = $state(false)
  // Idle times are read once when the list opens; nothing here ticks.
  let openedAt = $state(0)

  const attached = $derived(explorer.dataset.filter((item) => item.runtime?.attached))
  const byProvider = $derived((() => {
    const counts = {}
    for (const item of attached) counts[item.provider] = (counts[item.provider] ?? 0) + 1
    return Object.entries(counts).sort((a, b) => b[1] - a[1])
  })())
  const breakdown = $derived(byProvider.map(([p, n]) => `${PROVIDER_LABEL[p] ?? p} ${n}`).join(', '))

  // Longest idle first: those are the ones worth a look.
  const rows = $derived((() => {
    if (!open) return []
    const byNativeId = new Map(explorer.dataset.map((item) => [item.nativeId, item]))
    return attached
      .map((item) => ({
        item,
        title: displayTitle(item).text,
        idle: item.runtime.turnActive ? 'working' : sidebarAge(item.updatedAt ?? item.createdAt, openedAt),
        startedBy: starterLabel(item, byNativeId),
        lastAt: new Date(item.updatedAt ?? item.createdAt ?? 0).getTime() || 0,
      }))
      .sort((a, b) => a.lastAt - b.lastAt)
  })())

  function starterLabel(item, byNativeId) {
    if (!item.invokerNativeThreadId) return 'you'
    const invoker = byNativeId.get(item.invokerNativeThreadId)
    return invoker ? displayTitle(invoker).text : shortNativeId(item.invokerNativeThreadId)
  }

  function toggle() {
    open = !open
    if (open) openedAt = Date.now()
  }

  async function openRow(item) {
    open = false
    await openSidebarRow({ action: 'open', sessionId: item.runtime.sessionId, catalogId: item.transient ? null : item.id })
  }

  function onWindowKeydown(event) {
    if (event.key === 'Escape' && open) open = false
  }
</script>

<svelte:window onkeydown={onWindowKeydown} />

<button
  class="attached-btn"
  type="button"
  data-testid="attached-threads"
  aria-haspopup="dialog"
  aria-expanded={open}
  title="Threads Nano holds a live agent process for"
  onclick={toggle}
>
  Attached {attached.length}{#if breakdown}:&nbsp;<span class="attached-breakdown">{breakdown}</span>{/if}
</button>

{#if open}
  <div class="quick-detail-overlay" role="presentation" onclick={(event) => { if (event.target === event.currentTarget) open = false }}>
    <div class="quick-detail attached-list" role="dialog" aria-modal="true" aria-label="Attached threads">
      <button class="quick-detail-close" aria-label="Close attached threads" onclick={() => (open = false)}>×</button>
      <h3>Attached: {attached.length}{breakdown ? ` (${breakdown})` : ''}</h3>
      {#if rows.length === 0}
        <div class="mail-empty">Nano holds no agent processes.</div>
      {:else}
        <ul class="mail-list">
          {#each rows as row (row.item.id)}
            <li class="mail-item">
              <button class="mail-item-row attached-row" onclick={() => openRow(row.item)} title="Open this thread">
                <span class="mail-item-head">
                  <span class="t-provider {row.item.provider}">{row.item.provider}</span>
                  <span class="mail-subject">{row.title}</span>
                  <span class="mail-time">{row.idle === 'working' ? 'working' : `idle ${row.idle}`}</span>
                </span>
                <span class="mail-sender">started by {row.startedBy}</span>
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  </div>
{/if}
