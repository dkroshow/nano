const listeners = new Set()

export function addTranscriptAuditListener(listener) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function dispatchTranscriptAuditRequest(request) {
  for (const listener of listeners) {
    try {
      listener(request)
    } catch {
      // One pane cannot prevent another exact target from claiming its audit.
    }
  }
}

export function __resetTranscriptAuditListenersForTest() {
  listeners.clear()
}
