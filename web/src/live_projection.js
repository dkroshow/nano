// Each pane's file stream supplies runtime and pending operator dialogs.
export function applyRuntimeStatus(state, runtime, interactions) {
  replaceProjectionRuntime(state, runtime)
  state.pendingInteractions = interactions ?? []
}

function replaceProjectionRuntime(state, runtime = {}) {
  state.sessionStatus = runtime.sessionStatus ?? state.sessionStatus
  state.turnStartedAtMs = Number(runtime.turnStartedAtMs) || 0
  state.compactingSinceMs = Number(runtime.compactingSinceMs) || 0
  state.threadStatus = runtime.threadStatus ?? ''
  state.turnActive = Boolean(runtime.turnActive)
  state.tokenUsage = runtime.tokenUsage ?? null
  state.lastError = runtime.lastError ?? ''
  state.deliveryExpiries = Number(runtime.deliveryExpiries) || 0
}
