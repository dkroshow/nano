<script>
  import { runtimeLabel } from './lifecycle_state.js'
  import Conversation from './Conversation.svelte'
  import SessionInspector from './SessionInspector.svelte'
  import TokenGauge from './TokenGauge.svelte'
  import { openThreadExplorerAt } from '../app.svelte.js'
  import { startingPhase } from './composer_gate.js'

  let { info, session } = $props()
  const store = $derived(session.store)

  const shortThread = $derived(store.meta?.threadId ? store.meta.threadId.slice(-12) : '—')
  // A hosted session's header is standing metadata, so it follows the same
  // canonical projection as the footer and controls. Provider snapshots are
  // evidence that updates that projection, never UI fallback precedence.
  const model = $derived(
    store.metadataUnavailable
      ? ''
      : (store.posture?.model ?? info.posture?.model ?? '')
  )
  // The store's status (fed by /state and the list poll) beats the summary
  // prop: it picks up the resume attach flip from the wire without waiting
  // for the next poll.
  const runtimeStatus = $derived(store.sessionStatus ?? info.status)
  // Creating a session and attaching to a thread are both "starting"; only the
  // spawn mode tells them apart (lib/composer_gate.js).
  const startingAs = $derived(startingPhase(runtimeStatus, info.spawnMode))
  const dotClass = $derived(
    runtimeStatus !== 'ready' || !store.connected ? 'disconnected' : store.threadStatus === 'working' ? 'active' : 'idle'
  )
</script>

<div class="session">
  <header class="topbar">
    <div class="brand">{info.provider}-<span>nano</span></div>
    <div
      class="status-dot {dotClass}"
      title={store.connected ? runtimeLabel({ status: runtimeStatus, threadStatus: store.threadStatus }) : 'disconnected'}
    ></div>
    {#if model}<div class="chip">{model}</div>{/if}
    <div class="chip" title={store.meta?.threadId}>{shortThread}</div>
    <div class="chip" title={info.cwd}>{info.cwd.split('/').filter(Boolean).at(-1) ?? info.cwd}</div>
    {#if info.provider === 'codex' && store.meta?.threadId}
      <!-- Persisted reconstruction omits command executions (Probe P0) — the
           Explorer projection is the complete record, one click away. -->
      <button
        class="chip chip-action"
        title="Resumed history omits command executions — open the complete catalog history in the workspace"
        onclick={() => openThreadExplorerAt(store.meta.threadId)}
      >Open full history</button>
    {/if}
    {#if startingAs}<div class="chip chip-attaching">{startingAs === 'attaching' ? 'attaching…' : 'starting…'}</div>{/if}
    {#if info.status === 'exited'}<div class="chip" style="color: var(--red)">exited</div>{/if}
    {#if store.lastError}<div class="chip" style="color: var(--red)">{store.lastError}</div>{/if}
    <div class="spacer"></div>
    <TokenGauge usage={store.tokenUsage} />
  </header>

  <div class="split">
    <section class="pane-left">
      <Conversation {session} {info} />
    </section>
    <SessionInspector {session} />
  </div>
</div>
