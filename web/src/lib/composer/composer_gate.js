import { promptAllowed, promptQueues } from '../shared/lifecycle_state.js'

// The browser follows lifecycle admission. Starting and reconnecting accept
// queued text; unavailable or unknown connections retain the draft locally.
export function composerLocked(sessionStatus) {
  return !promptAllowed(sessionStatus)
}

// "starting" covers two different waits, and the copy has to say which. Both
// spawn kinds now answer their POST before the provider handshake finishes, so
// the operator sees this state whether they created a session or reattached to
// a thread — and being told "attaching to thread" while creating one is a
// wrong answer to "what is it doing". The summary's spawnMode is the server's
// own record of which was asked for; an absent one reads as a start, since
// nothing else ever had to be distinguished from an attach.
export function startingPhase(sessionStatus, spawnMode) {
  if (sessionStatus !== 'starting') return ''
  return spawnMode === 'resume' ? 'attaching' : 'creating'
}

// joinsTurn is true when Send delivers into the running turn: the server says
// the provider takes running-turn input, and the turn is working rather than
// waiting on a dialog. Queue then remains the explicit next-turn choice.
export function composerPlaceholder(sessionStatus, turnActive, spawnMode, joinsTurn = false) {
  const phase = startingPhase(sessionStatus, spawnMode)
  if (phase) return phase === 'attaching' ? 'attaching to thread…' : 'starting session…'
  if (!promptAllowed(sessionStatus)) return 'Connection unavailable — message retained here'
  if (!promptQueues(sessionStatus, turnActive)) return 'Message the agent'
  return joinsTurn ? 'Add to the running turn' : 'Queue the next message'
}

// Enter sends on a desktop keyboard, where Shift+Enter is the newline everyone
// already knows. On a touch keyboard it must not: Return is the only line break
// a phone offers, there is no Shift chord to reach for, and a message sent by
// accident cannot be taken back. The button is the submit path there — which is
// also the one that reads "Queue" mid-turn, so nothing about send-vs-queue
// changes with the keyboard.
export function enterSubmits(key, { shiftKey = false, coarse = false } = {}) {
  return key === 'Enter' && !shiftKey && !coarse
}
