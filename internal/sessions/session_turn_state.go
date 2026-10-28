package sessions

import (
	"time"

	"github.com/dkroshow/nano/internal/lifecycle"
)

// setTurnActive tracks whether a turn is in flight, which turn, and when it
// began; only real flips reach the runtime SSE stream. A completing turn drops
// the id and the anchor with the flag, so nothing can later interrupt a turn
// that already ended and no surface can keep timing one.
//
// startedAtMs is evidence, not a default: it is honored only on the false→true
// edge (D7/D10 — a same-turn event must never restart the clock), and 0 is a
// legitimate value meaning "no evidence", which renders as working-without-a-
// timer rather than as a fabricated start (D2/D8).
//
// live gates the completion stamp the same way (D2/D8, mirrored on the close
// edge): a shim-replayed turn close reconstructs state, but the turn did not
// end NOW — stamping arrival time rewrote every reattached thread's "last
// turn ended" to the deploy moment on each nano restart (observed live
// 2026-08-06: 14 threads sharing one restart minute). A replayed close leaves
// the stamp absent, which is the documented post-restart state of this
// deliberately unpersisted field.
func (sess *Session) setTurnActive(active bool, turnID string, startedAtMs int64, live bool) {
	sess.setTurnActiveState(active, turnID, startedAtMs, live, true)
}

func (sess *Session) beginTurnRestore() {
	sess.stateMu.Lock()
	if !sess.turnRestoreInProgress {
		sess.turnRestoreInProgress = true
		sess.turnRestoreSequence = 0
		sess.turnRestoreActive = sess.workActiveLocked()
		sess.turnRestoreAccepted = sess.lifecycleState.State.Work == lifecycle.Accepted
		sess.turnRestoreID = sess.turnID
		sess.turnRestoreStartedAtMs = sess.turnStartedAtMs
	}
	sess.stateMu.Unlock()
}

func (sess *Session) cancelTurnRestore() {
	sess.stateMu.Lock()
	sess.clearTurnRestoreLocked()
	sess.stateMu.Unlock()
}

func (sess *Session) clearTurnRestoreLocked() {
	sess.turnRestoreInProgress = false
	sess.turnRestoreSequence = 0
	sess.turnRestoreActive = false
	sess.turnRestoreAccepted = false
	sess.turnRestoreID = ""
	sess.turnRestoreStartedAtMs = 0
}

// restoreTurnActive reconstructs one historical turn boundary without
// publishing it as present-tense runtime. A Claude shim replay can contain
// hundreds of completed turns; announcing each prompt/result pair makes every
// browser repeatedly promote and demote the restored thread while Nano boots.
// The attach path calls settleRestoredTurnState once after the complete replay
// and the shim's current-state snapshot have both landed.
func (sess *Session) restoreTurnActive(active bool, turnID string, startedAtMs int64) {
	sess.setTurnActiveState(active, turnID, startedAtMs, false, false)
}

func (sess *Session) setTurnActiveState(active bool, turnID string, startedAtMs int64, live, publish bool) {
	sess.stateMu.Lock()
	restoring := sess.turnRestoreInProgress
	currentActive, currentID := sess.workActiveLocked(), sess.turnID
	if restoring {
		currentActive, currentID = sess.turnRestoreActive, sess.turnRestoreID
	}
	if active && turnID != "" && (turnID == sess.durablyTerminalTurnID || turnID == sess.lastProviderTerminalTurnID) {
		sess.stateMu.Unlock()
		return
	}
	// A late terminal belongs to its own turn, never a newer one. Anonymous
	// boundaries are qualified by the provider adapter before reaching here.
	// Claude runs one execution at a time: its result ends it whichever
	// prompt it answered, and a prompt written mid-turn is often the newer
	// turn here.
	if !active && turnID != "" && currentID != "" && turnID != currentID && sess.provider != "claude" {
		sess.stateMu.Unlock()
		return
	}
	if !restoring && sess.lifecycleState.State.Connection != lifecycle.Ready {
		sess.stateMu.Unlock()
		return
	}
	changed, drain := false, false
	if restoring {
		sess.turnRestoreSequence = sess.nextEvidenceSequenceLocked()
		changed = currentActive != active
		if active {
			if !currentActive || sess.turnRestoreAccepted || currentID != turnID {
				sess.turnRestoreStartedAtMs = startedAtMs
			}
			sess.turnRestoreActive, sess.turnRestoreAccepted = true, false
			sess.turnRestoreID = turnID
		} else {
			sess.turnRestoreActive, sess.turnRestoreAccepted = false, false
			sess.turnRestoreID, sess.turnRestoreStartedAtMs = "", 0
		}
	} else if active {
		wasAccepted := sess.lifecycleState.State.Work == lifecycle.Accepted
		changed, drain = sess.transitionLifecycleLocked(lifecycle.EventStarted)
		if !currentActive || wasAccepted || currentID != turnID {
			sess.turnStartedAtMs = startedAtMs
		}
		sess.turnID = turnID
	} else {
		changed, drain = sess.transitionLifecycleLocked(lifecycle.EventFinished)
		sess.turnID, sess.turnStartedAtMs = "", 0
	}
	if !active {
		// A replayed turn end too: a compaction it held ends with it.
		sess.compactingSinceMs = 0
		if live {
			sess.lastProviderTerminalTurnID = turnID
		}
		if changed && live {
			sess.lastTurnEndedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
	}
	sess.stateMu.Unlock()
	if changed && publish && !restoring {
		sess.PublishProjectionRuntime()
		sess.NotifyRuntime()
		if drain {
			sess.kickQueuedTurnDrain()
		}
	}
}

// settleRestoredTurnState exposes exactly one runtime answer after a Claude
// replay. It also releases a prompt queued while the replay was draining when
// the settled state is idle.
func (sess *Session) settleRestoredTurnState() {
	sess.stateMu.Lock()
	if !sess.turnRestoreInProgress {
		sess.stateMu.Unlock()
		return
	}
	drain, status := sess.settleRestoredTurnStateLocked()
	sess.stateMu.Unlock()
	sess.publishRestoredTurnState(drain, status)
}

func (sess *Session) settleRestoredTurnStateLocked() (drain bool, status string) {
	_, drain = sess.transitionLifecycleLocked(sess.readyLifecycleEventLocked())
	if sess.workActiveLocked() {
		sess.turnID, sess.turnStartedAtMs = sess.turnRestoreID, sess.turnRestoreStartedAtMs
	} else {
		sess.turnID, sess.turnStartedAtMs = "", 0
	}
	sess.clearTurnRestoreLocked()
	return drain, sess.connectionStatusLocked()
}

func (sess *Session) publishRestoredTurnState(drain bool, status string) {
	sess.PublishProjectionRuntime()
	sess.NotifyRuntime()
	sess.updateHostedThread(status, "")
	if drain {
		sess.kickQueuedTurnDrain()
	}
}

// restoreTurnAnchor supplies the start instant of a turn that is ALREADY
// active but has no anchor — the claude re-attach path, where the replay
// re-derives the turn state but deliberately refuses to date it from arrival
// time. Two guards make this a correction rather than a second source of
// truth: it never fires on an idle session, and it may only move the anchor
// BACKWARD (D9/D11), so a recovered start can lengthen the reported elapsed
// but never shorten it.
func (sess *Session) restoreTurnAnchor(startedAtMs int64) bool {
	return sess.restoreTurnAnchorState(startedAtMs, true)
}

func (sess *Session) restoreTurnAnchorQuietly(startedAtMs int64) bool {
	return sess.restoreTurnAnchorState(startedAtMs, false)
}

func (sess *Session) restoreTurnAnchorState(startedAtMs int64, publish bool) bool {
	if startedAtMs <= 0 {
		return false
	}
	sess.stateMu.Lock()
	restoring := sess.turnRestoreInProgress
	active := sess.workActiveLocked()
	anchor := sess.turnStartedAtMs
	if restoring {
		active = sess.turnRestoreActive
		anchor = sess.turnRestoreStartedAtMs
	}
	restore := active && (anchor == 0 || startedAtMs < anchor)
	if restore {
		if restoring {
			sess.turnRestoreStartedAtMs = startedAtMs
		} else {
			sess.turnStartedAtMs = startedAtMs
		}
	}
	sess.stateMu.Unlock()
	if restore && publish && !restoring {
		sess.PublishProjectionRuntime()
		sess.NotifyRuntime()
	}
	return restore
}

// turnAnchor snapshots the live turn's anchor, 0 when there is none.
func (sess *Session) turnAnchor() int64 {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	if sess.turnRestoreInProgress {
		return sess.turnRestoreStartedAtMs
	}
	return sess.turnStartedAtMs
}

// compactingSinceMsLocked is the compaction start the session shows: only a
// turn in flight can be compacting, whichever path ended the last one.
func (sess *Session) compactingSinceMsLocked() int64 {
	if !sess.workActiveLocked() {
		return 0
	}
	return sess.compactingSinceMs
}

// markCompacting applies a provider's own compaction signal: a start keeps
// the first start it saw (startedAtMs), an end clears it. A change reaches
// every pane's status and the rail.
func (sess *Session) markCompacting(compacting bool, startedAtMs int64) {
	sess.stateMu.Lock()
	since := sess.compactingSinceMs
	switch {
	case !compacting:
		since = 0
	case since == 0:
		since = startedAtMs
	}
	changed := since != sess.compactingSinceMs
	sess.compactingSinceMs = since
	sess.stateMu.Unlock()
	if changed {
		sess.PublishProjectionRuntime()
		sess.NotifyRuntime()
	}
}

// activeTurn snapshots the (thread, turn) pair an interrupt would address.
// An empty turn id means there is nothing to interrupt.
func (sess *Session) activeTurn() (threadID, turnID string) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	if !sess.workActiveLocked() {
		return sess.threadID, ""
	}
	return sess.threadID, sess.turnID
}

// turnOptions snapshots the per-turn overrides the next turn/start will carry.
func (sess *Session) turnOptions() (model, effort, serviceTier string) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.turnModel, sess.turnEffort, sess.turnServiceTier
}

// NotifyRuntime publishes a change to the attachment runtime view.
func (sess *Session) NotifyRuntime() {
	if sess.runtimeNotify != nil {
		sess.runtimeNotify()
	}
}

// ThreadID reads the attachment’s current native provider identity.
func (sess *Session) ThreadID() string {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.threadID
}
