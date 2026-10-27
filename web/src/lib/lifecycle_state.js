// Compatibility fields from the server's lifecycle snapshot. Work is evidence
// only while ready; unknown connections must not inherit stale activity.
export function runtimeReady(runtime) {
  return runtime?.status === 'ready'
}

// The server's one turn state for a thread: working, needs_you or idle. A row's
// live overlay carries its session's label and every runtime delta refreshes
// it, so it wins; a thread Nano does not hold has only the catalog's label,
// which the server stamps from that same overlay whenever one exists.
export function turnState(item) {
  return item?.runtime?.threadStatus || item?.fleet?.label || 'idle'
}

// A live runtime's label: its connection while not ready, else its turn state.
// Takes a runtime overlay or a session summary; both carry threadStatus.
export function runtimeLabel(runtime) {
  if (!runtimeReady(runtime)) {
    if (runtime?.status === 'reconnect_parked') return 'parked'
    if (runtime?.status === 'reconnect_failed') return 'unreachable'
    return runtime?.status || 'unknown'
  }
  if (runtime.threadStatus === 'working') return 'working'
  return runtime.threadStatus === 'needs_you' ? 'waiting' : 'idle'
}

// The turn state a surface last heard, for when it cannot hear the server.
export function offlineTurnLabel(state) {
  const seen = { working: 'working', needs_you: 'waiting for you', idle: 'idle' }[state]
  return seen ? `Offline — last seen ${seen}` : 'Offline'
}

export function promptAllowed(status) {
  return ['ready', 'starting', 'reconnecting', 'reconnect_parked'].includes(status)
}

export function promptQueues(status, turnActive) {
  return promptAllowed(status) && (status !== 'ready' || Boolean(turnActive))
}
