// A browser closes an EventSource for good when a reconnect gets an HTTP
// error, as Tailscale Serve's 502 does while Nano restarts behind it; a
// source still CONNECTING is the browser's own retry. reopenIfClosed asks for
// a new stream at about the browser's own retry pace, and returns the timer
// (null when the browser is still retrying).
export const STREAM_REOPEN_MS = 3000

export function reopenIfClosed(source, reopen) {
  if (source?.readyState !== globalThis.EventSource?.CLOSED) return null
  return setTimeout(reopen, STREAM_REOPEN_MS)
}
