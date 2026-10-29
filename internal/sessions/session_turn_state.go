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
func (attachmentState *State) setTurnActive(active bool, turnID string, startedAtMs int64, live bool) {
	attachmentState.setTurnActiveState(active, turnID, startedAtMs, live, true)
}

func (attachmentState *State) beginTurnRestore() {
	attachmentState.stateMu.Lock()
	if !attachmentState.turnRestoreInProgress {
		attachmentState.turnRestoreInProgress = true
		attachmentState.turnRestoreSequence = 0
		attachmentState.turnRestoreActive = attachmentState.workActiveLocked()
		attachmentState.turnRestoreAccepted = attachmentState.lifecycleState.State.Work == lifecycle.Accepted
		attachmentState.turnRestoreID = attachmentState.turnID
		attachmentState.turnRestoreStartedAtMs = attachmentState.turnStartedAtMs
	}
	attachmentState.stateMu.Unlock()
}

func (attachmentState *State) cancelTurnRestore() {
	attachmentState.stateMu.Lock()
	attachmentState.clearTurnRestoreLocked()
	attachmentState.stateMu.Unlock()
}

func (attachmentState *State) clearTurnRestoreLocked() {
	attachmentState.turnRestoreInProgress = false
	attachmentState.turnRestoreSequence = 0
	attachmentState.turnRestoreActive = false
	attachmentState.turnRestoreAccepted = false
	attachmentState.turnRestoreID = ""
	attachmentState.turnRestoreStartedAtMs = 0
}

// restoreTurnActive reconstructs one historical turn boundary without
// publishing it as present-tense runtime. A Claude shim replay can contain
// hundreds of completed turns; announcing each prompt/result pair makes every
// browser repeatedly promote and demote the restored thread while Nano boots.
// The attach path calls settleRestoredTurnState once after the complete replay
// and the shim's current-state snapshot have both landed.
func (attachmentState *State) restoreTurnActive(active bool, turnID string, startedAtMs int64) {
	attachmentState.setTurnActiveState(active, turnID, startedAtMs, false, false)
}

func (attachmentState *State) setTurnActiveState(active bool, turnID string, startedAtMs int64, live, publish bool) {
	attachmentState.stateMu.Lock()
	restoring := attachmentState.turnRestoreInProgress
	currentActive, currentID := attachmentState.workActiveLocked(), attachmentState.turnID
	if restoring {
		currentActive, currentID = attachmentState.turnRestoreActive, attachmentState.turnRestoreID
	}
	if active && turnID != "" && (turnID == attachmentState.durablyTerminalTurnID || turnID == attachmentState.lastProviderTerminalTurnID) {
		attachmentState.stateMu.Unlock()
		return
	}
	// A late terminal belongs to its own turn, never a newer one. Anonymous
	// boundaries are qualified by the provider adapter before reaching here.
	// Claude runs one execution at a time: its result ends it whichever
	// prompt it answered, and a prompt written mid-turn is often the newer
	// turn here.
	if !active && turnID != "" && currentID != "" && turnID != currentID && attachmentState.identity.provider != "claude" {
		attachmentState.stateMu.Unlock()
		return
	}
	if !restoring && attachmentState.lifecycleState.State.Connection != lifecycle.Ready {
		attachmentState.stateMu.Unlock()
		return
	}
	changed, drain := false, false
	if restoring {
		attachmentState.turnRestoreSequence = attachmentState.nextEvidenceSequenceLocked()
		changed = currentActive != active
		if active {
			if !currentActive || attachmentState.turnRestoreAccepted || currentID != turnID {
				attachmentState.turnRestoreStartedAtMs = startedAtMs
			}
			attachmentState.turnRestoreActive, attachmentState.turnRestoreAccepted = true, false
			attachmentState.turnRestoreID = turnID
		} else {
			attachmentState.turnRestoreActive, attachmentState.turnRestoreAccepted = false, false
			attachmentState.turnRestoreID, attachmentState.turnRestoreStartedAtMs = "", 0
		}
	} else if active {
		wasAccepted := attachmentState.lifecycleState.State.Work == lifecycle.Accepted
		changed, drain = attachmentState.transitionLifecycleLocked(lifecycle.EventStarted)
		if !currentActive || wasAccepted || currentID != turnID {
			attachmentState.turnStartedAtMs = startedAtMs
		}
		attachmentState.turnID = turnID
	} else {
		changed, drain = attachmentState.transitionLifecycleLocked(lifecycle.EventFinished)
		attachmentState.turnID, attachmentState.turnStartedAtMs = "", 0
	}
	if !active {
		// A replayed turn end too: a compaction it held ends with it.
		attachmentState.compactingSinceMs = 0
		if live {
			attachmentState.lastProviderTerminalTurnID = turnID
		}
		if changed && live {
			attachmentState.lastTurnEndedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
	}
	attachmentState.stateMu.Unlock()
	if changed && publish && !restoring {
		attachmentState.PublishProjectionRuntime()
		attachmentState.NotifyRuntime()
		if drain {
			attachmentState.delivery.kickQueuedTurnDrain()
		}
	}
}

// settleRestoredTurnState exposes exactly one runtime answer after a Claude
// replay. It also releases a prompt queued while the replay was draining when
// the settled state is idle.
func (attachmentState *State) settleRestoredTurnState() {
	attachmentState.stateMu.Lock()
	if !attachmentState.turnRestoreInProgress {
		attachmentState.stateMu.Unlock()
		return
	}
	drain, status := attachmentState.settleRestoredTurnStateLocked()
	attachmentState.stateMu.Unlock()
	attachmentState.publishRestoredTurnState(drain, status)
}

func (attachmentState *State) settleRestoredTurnStateLocked() (drain bool, status string) {
	_, drain = attachmentState.transitionLifecycleLocked(attachmentState.readyLifecycleEventLocked())
	if attachmentState.workActiveLocked() {
		attachmentState.turnID, attachmentState.turnStartedAtMs = attachmentState.turnRestoreID, attachmentState.turnRestoreStartedAtMs
	} else {
		attachmentState.turnID, attachmentState.turnStartedAtMs = "", 0
	}
	attachmentState.clearTurnRestoreLocked()
	return drain, attachmentState.connectionStatusLocked()
}

func (attachmentState *State) publishRestoredTurnState(drain bool, status string) {
	attachmentState.PublishProjectionRuntime()
	attachmentState.NotifyRuntime()
	attachmentState.updateHostedThread(status, "")
	if drain {
		attachmentState.delivery.kickQueuedTurnDrain()
	}
}

// restoreTurnAnchor supplies the start instant of a turn that is ALREADY
// active but has no anchor — the claude re-attach path, where the replay
// re-derives the turn state but deliberately refuses to date it from arrival
// time. Two guards make this a correction rather than a second source of
// truth: it never fires on an idle session, and it may only move the anchor
// BACKWARD (D9/D11), so a recovered start can lengthen the reported elapsed
// but never shorten it.
func (attachmentState *State) restoreTurnAnchor(startedAtMs int64) bool {
	return attachmentState.restoreTurnAnchorState(startedAtMs, true)
}

func (attachmentState *State) restoreTurnAnchorQuietly(startedAtMs int64) bool {
	return attachmentState.restoreTurnAnchorState(startedAtMs, false)
}

func (attachmentState *State) restoreTurnAnchorState(startedAtMs int64, publish bool) bool {
	if startedAtMs <= 0 {
		return false
	}
	attachmentState.stateMu.Lock()
	restoring := attachmentState.turnRestoreInProgress
	active := attachmentState.workActiveLocked()
	anchor := attachmentState.turnStartedAtMs
	if restoring {
		active = attachmentState.turnRestoreActive
		anchor = attachmentState.turnRestoreStartedAtMs
	}
	restore := active && (anchor == 0 || startedAtMs < anchor)
	if restore {
		if restoring {
			attachmentState.turnRestoreStartedAtMs = startedAtMs
		} else {
			attachmentState.turnStartedAtMs = startedAtMs
		}
	}
	attachmentState.stateMu.Unlock()
	if restore && publish && !restoring {
		attachmentState.PublishProjectionRuntime()
		attachmentState.NotifyRuntime()
	}
	return restore
}

// turnAnchor snapshots the live turn's anchor, 0 when there is none.
func (attachmentState *State) turnAnchor() int64 {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	if attachmentState.turnRestoreInProgress {
		return attachmentState.turnRestoreStartedAtMs
	}
	return attachmentState.turnStartedAtMs
}

// compactingSinceMsLocked is the compaction start the session shows: only a
// turn in flight can be compacting, whichever path ended the last one.
func (attachmentState *State) compactingSinceMsLocked() int64 {
	if !attachmentState.workActiveLocked() {
		return 0
	}
	return attachmentState.compactingSinceMs
}

// markCompacting applies a provider's own compaction signal: a start keeps
// the first start it saw (startedAtMs), an end clears it. A change reaches
// every pane's status and the rail.
func (attachmentState *State) markCompacting(compacting bool, startedAtMs int64) {
	attachmentState.stateMu.Lock()
	since := attachmentState.compactingSinceMs
	switch {
	case !compacting:
		since = 0
	case since == 0:
		since = startedAtMs
	}
	changed := since != attachmentState.compactingSinceMs
	attachmentState.compactingSinceMs = since
	attachmentState.stateMu.Unlock()
	if changed {
		attachmentState.PublishProjectionRuntime()
		attachmentState.NotifyRuntime()
	}
}

// activeTurn snapshots the (thread, turn) pair an interrupt would address.
// An empty turn id means there is nothing to interrupt.
func (attachmentState *State) activeTurn() (threadID, turnID string) {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	if !attachmentState.workActiveLocked() {
		return attachmentState.threadID, ""
	}
	return attachmentState.threadID, attachmentState.turnID
}

// turnOptions snapshots the per-turn overrides the next turn/start will carry.
func (attachmentState *State) turnOptions() (model, effort, serviceTier string) {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	return attachmentState.turnModel, attachmentState.turnEffort, attachmentState.turnServiceTier
}

// NotifyRuntime publishes a change to the attachment runtime view.
func (attachmentState *State) NotifyRuntime() {
	if attachmentState.runtimeNotify != nil {
		attachmentState.runtimeNotify()
	}
}

// ThreadID reads the attachment’s current native provider identity.
func (attachmentState *State) ThreadID() string {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	return attachmentState.threadID
}
