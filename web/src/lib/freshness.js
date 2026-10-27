// Whether a surface may show the turn state it holds as live. The pane hears
// its turn state on its own stream; while that stream is down, a replacement
// has not spoken yet, or Nano does not answer this browser
// (connection.svelte.js), it shows what it last heard as last seen. The rail
// shows a lost connection once, as the page's reconnecting banner, and its
// rows keep what they last showed. Only transport facts move these states,
// never a clock on the turn.

// The pane keeps one state across the streams it opens (a view switch, a
// reveal and a repair each open a new one):
//
//   event \ state                     live      awaiting  dropped
//   stream opened                     awaiting  awaiting  dropped
//   status on the current stream      live      live      live
//   stream error or server error      dropped   dropped   dropped
//
// A deliberate close is not an event: the next open follows it.
export function paneLink(state, event) {
  switch (event) {
    case 'opened': return state === 'dropped' ? 'dropped' : 'awaiting'
    case 'status': return 'live'
    case 'dropped': return 'dropped'
    default: return state
  }
}

// The rail's catalog stream. A drop and the reopen after it each start a new
// epoch, so the epoch names one subscription. A whole read is current when it
// began during this subscription (commitActiveSidebar): one begun before a
// drop or during the outage could not see the events lost meanwhile, so a
// newer live read wins over it. After a reopen the page reads the active
// sidebar, every open history scope, the window and the session list again.
//
//   event \ state     up                down
//   stream error      epoch+1, down     no change
//   stream open       no change         epoch+1, up
export function railLink(link, event) {
  if (event === 'dropped' && link.up) return { epoch: link.epoch + 1, up: false }
  if (event === 'opened' && !link.up) return { epoch: link.epoch + 1, up: true }
  return link
}
