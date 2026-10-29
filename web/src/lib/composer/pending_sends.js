// Nano owns every prompt it has acknowledged: its state, label and allowed
// actions reach the pane on the thread's stream. The one prompt the browser
// holds is a send Nano never acknowledged: it exists nowhere but here, so its
// text is kept in storage (one key per action, so two tabs never overwrite
// each other) from just before its POST until Nano acknowledges it, the
// stream lists it, or the person discards it.

const LEGACY_PREFIX = 'nano-pending:'
const UNSENT_PREFIX = 'nano-unsent:'

export function newClientActionId() {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `nano-${Date.now()}-${Math.random().toString(16).slice(2)}`
}

function unsentRecord(send) {
  return JSON.stringify({
    clientActionId: send.clientActionId,
    provider: send.provider,
    threadId: send.threadId,
    sessionId: send.sessionId,
    text: send.text,
    createdAt: send.createdAt,
    // Part of the request: a Resend of this action must ask the same.
    ...(send.nextTurn ? { nextTurn: true } : {}),
    // The person asked to discard it: nothing posts it again by itself.
    ...(send.stopped ? { stopped: true } : {}),
    // No POST for it has started, so Nano cannot have it. A record without
    // the mark may have been posted (older pages stored only posted sends);
    // the mark is gone from the record before any POST for it leaves.
    ...(send.held ? { held: true } : {}),
  })
}

// Writes the send's record and reads it back. A Send or Resend posts only when
// this succeeds: the composer has given up its draft, so the record is the
// only copy a reload can find.
export function persistUnsent(send) {
  const record = unsentRecord(send)
  try {
    localStorage.setItem(UNSENT_PREFIX + send.clientActionId, record)
    return localStorage.getItem(UNSENT_PREFIX + send.clientActionId) === record
  } catch {
    return false
  }
}

export function forgetUnsent(clientActionId) {
  try { localStorage.removeItem(UNSENT_PREFIX + clientActionId) } catch { /* unavailable */ }
}

// The stored record of one send, or null: another tab may have changed it.
export function readUnsent(clientActionId) {
  try {
    const stored = JSON.parse(localStorage.getItem(UNSENT_PREFIX + clientActionId))
    return stored?.clientActionId === clientActionId ? stored : null
  } catch {
    return null
  }
}

// A thread's sends Nano never acknowledged, oldest first.
export function loadUnsent({ provider, threadId, sessionId } = {}) {
  if (typeof localStorage === 'undefined') return []
  const sends = []
  try {
    for (let i = 0; i < localStorage.length; i += 1) {
      const key = localStorage.key(i)
      if (!key?.startsWith(UNSENT_PREFIX)) continue
      let stored = null
      try { stored = JSON.parse(localStorage.getItem(key)) } catch { stored = null }
      if (!stored?.clientActionId || !String(stored.text ?? '').trim()) continue
      // A send made before the thread id landed is keyed by its session.
      const mine = stored.threadId
        ? Boolean(threadId) && stored.provider === provider && stored.threadId === threadId
        : Boolean(sessionId) && stored.sessionId === sessionId
      if (mine) sends.push(stored)
    }
  } catch {
    return []
  }
  return sends.sort((a, b) => (a.createdAt || 0) - (b.createdAt || 0))
}

// unsentStep is the whole life of this browser's copy of one send, as one
// reducer. The copy may be stored (its record above), shown (an unsent row),
// both, or neither; its text goes back to the composer at most once, and once
// Nano is known to have it (acked) or to have ended it, no later event shows
// it again. entry is
// the previous result (undefined at first). Events:
//   held       kept unposted, already stored: Nano cannot be reached, or an
//              earlier send to this thread has no answer yet
//   posting    a POST for it starts; event.send is the send, already stored
//   answered   Nano has it: a 2xx/202, or Nano's own read says it took it
//   listed     the thread's stream lists it: a shown copy is Nano's from
//              then on; a send still posting only loses its echo, since it
//              may yet be refused
//   refused    Nano definitely refused the send being posted, whose text is
//              event.text; event.quiet keeps it out of the composer (a legacy
//              queue entry holds its own copy)
//   lost       a POST ended with no answer, and Nano's read could not say more
//   loaded     found in storage when the page loads; event.send is the record
//   ending     this tab asked Nano to end it (Discard)
//   endFailed  Nano did not confirm that end
//   ended      Nano confirmed the end; the text goes back to the composer
//              (event.text is the row's, for a prompt this tab never held).
//              A refusal ends the action too: no later answer shows it again
//   resent     Nano resent it as a new prompt, which now owns the text: it
//              ends here for good, and nothing goes back to the composer
//   gone       its stored record is missing or stopped: another tab sent,
//              discarded or stopped it, so this tab stands down
//   saved      the stream names it saved, or Nano's record says it settled:
//              its saved row is the pane's now
//   missed     a stream tail neither listed it nor named it saved: once
//              Nano has the send, a read of its record says whether it
//              settled (check)
//   checked    that read answered; event.settled says the record is
//              confirmed, refused or expired
//   slow       its POST has been out long enough to say it is still sending
// It returns the next entry, whether to forget the stored record, and the
// text to hand back ('' for none). An entry keeps the text only while this
// browser may still need it.
//
// A shown copy also retries by itself: retry says the store re-posts it under
// its own action id (Nano answers an id once however often it arrives), and
// attempts counts the posts that got no answer, for the backoff. lost and
// loaded turn it on; it is off whenever the copy is hidden. ending marks the
// send stopped (the store has already stored the mark: a reload must not
// retry it either), so a late lost answer does not turn it on again: after a
// Discard the person asked for, only their own Resend posts it (a fresh
// posting, which clears the mark).
//
// posted says a POST for it ever started, so Nano may have it; inFlight that
// one is out now. A copy never posted is held, not retried: the store posts
// a thread's copies one at a time, oldest first, when Nano can be reached.
//
// echo is the transcript row for a send the person just made: from its POST
// until the stream lists it, the pane shows it as delivering, so the prompt
// never leaves the screen while the network carries it. It outlives Nano's
// answer (the stream may list the prompt after the POST answers) and ends on
// the server's word alone: the stream lists it or names it saved. It also
// ends when the copy ends or moves to the not-delivered list. A shown copy
// has no echo. slow says its POST is still out after a while.
//
// check runs those reads one at a time, newest trigger last: idle, reading
// (the store reads when a copy enters it), or again (a newer silent tail or
// answer came while a read was out, so its answer cannot decide and one more
// read follows it).
export function unsentStep(entry, event) {
  const { entry: stepped, forget, handBack } = step(entry, event)
  const next = { ...stepped, retry: stepped.shown && stepped.retry && !stepped.send?.stopped }
  const done = (next.acked || next.ended || next.handedBack) && !next.echo
  return { entry: done && next.send ? { ...next, send: { clientActionId: next.send.clientActionId } } : next, forget, handBack }
}

// retryDelayMs is how long a shown copy waits before its next automatic post:
// 2, 4, 8 and 16 seconds, then every 30 seconds until Nano answers.
export function retryDelayMs(attempts) {
  return Math.min(30_000, 1_000 * 2 ** Math.max(1, attempts))
}

function step(entry, event) {
  const copy = {
    send: null, shown: false, acked: false, ending: false, ended: false, handedBack: false,
    retry: false, attempts: 0, posted: false, inFlight: false, echo: false, slow: false, missed: false, check: 'idle', ...entry,
  }
  const settled = copy.acked || copy.ended
  const handBack = (allowed, text) => (allowed && !copy.handedBack && text ? text : '')
  const arm = (check) => (check === 'idle' ? 'reading' : 'again')
  switch (event.type) {
    case 'held':
      return { entry: { ...copy, send: event.send, shown: true }, forget: false, handBack: '' }
    case 'posting':
      return { entry: { ...copy, send: event.send, posted: true, inFlight: true, echo: !copy.shown, slow: false, missed: false, check: 'idle' }, forget: false, handBack: '' }
    case 'answered':
      return {
        entry: { ...copy, shown: false, acked: true, inFlight: false, slow: false, check: copy.echo && copy.missed ? arm(copy.check) : copy.check },
        forget: true, handBack: '',
      }
    case 'listed':
      // The stream's row takes an echo's place; a send still posting is not
      // Nano's on that alone, since it may yet be refused or lost.
      if (copy.shown) return { entry: { ...copy, shown: false, echo: false, acked: true, inFlight: false }, forget: true, handBack: '' }
      return { entry: { ...copy, echo: false }, forget: false, handBack: '' }
    case 'saved':
      return { entry: { ...copy, echo: false }, forget: false, handBack: '' }
    case 'missed':
      return { entry: { ...copy, missed: copy.echo, check: copy.echo && copy.acked ? arm(copy.check) : copy.check }, forget: false, handBack: '' }
    case 'checked':
      if (event.settled || !copy.echo) return { entry: { ...copy, echo: false, check: 'idle' }, forget: false, handBack: '' }
      return { entry: { ...copy, check: copy.check === 'again' ? 'reading' : 'idle' }, forget: false, handBack: '' }
    case 'slow':
      return { entry: { ...copy, slow: copy.inFlight }, forget: false, handBack: '' }
    case 'refused': {
      const text = handBack(!event.quiet && !copy.ended, event.text)
      return {
        entry: { ...copy, shown: false, echo: false, ended: true, inFlight: false, handedBack: copy.handedBack || Boolean(text) },
        forget: true, handBack: text,
      }
    }
    case 'lost':
      return settled
        ? { entry: { ...copy, inFlight: false, echo: false }, forget: true, handBack: '' }
        : { entry: { ...copy, shown: true, echo: false, inFlight: false, retry: !copy.ending, attempts: copy.attempts + 1 }, forget: false, handBack: '' }
    case 'loaded':
      return settled
        ? { entry: copy, forget: true, handBack: '' }
        : { entry: { ...copy, send: event.send, shown: true, echo: false, posted: !event.send.held, retry: !event.send.held }, forget: false, handBack: '' }
    case 'gone':
      return { entry: { ...copy, shown: false, echo: false, ended: true }, forget: false, handBack: '' }
    case 'ending':
      // Every copy this tab may still post stops, shown or with its POST out.
      return settled || !copy.send?.text
        ? { entry: { ...copy, ending: true, retry: false }, forget: false, handBack: '' }
        : { entry: { ...copy, ending: true, retry: false, send: { ...copy.send, stopped: true } }, forget: false, handBack: '' }
    case 'endFailed':
      return { entry: { ...copy, ending: false }, forget: false, handBack: '' }
    case 'resent':
      return { entry: { ...copy, ending: false, ended: true, shown: false, echo: false }, forget: true, handBack: '' }
    case 'ended': {
      const text = handBack(true, copy.send?.text || event.text)
      return {
        entry: { ...copy, ending: false, ended: true, shown: false, echo: false, handedBack: copy.handedBack || Boolean(text) },
        forget: true, handBack: text,
      }
    }
  }
  throw new Error(`unsentStep: unknown event ${event.type}`)
}

// One-time migration guard: old pending operation keys are deliberately
// deleted, but composer draft keys remain untouched.
export function purgeLegacyPendingStorage() {
  if (typeof localStorage === 'undefined') return
  try {
    for (let i = localStorage.length - 1; i >= 0; i -= 1) {
      const key = localStorage.key(i)
      if (key?.startsWith(LEGACY_PREFIX)) localStorage.removeItem(key)
    }
  } catch {
    // Storage may be unavailable; nothing old is left to read either.
  }
}
