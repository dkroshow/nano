package sidebar

import (
	"time"

	"github.com/dkroshow/nano/internal/catalog"
)

const (
	fleetWorking         = catalog.FleetWorking
	fleetNeedsYou        = catalog.FleetNeedsYou
	fleetIdle            = catalog.FleetIdle
	fleetCourtUserOwes   = catalog.CourtUserOwes
	fleetCourtAgentOwes  = catalog.CourtAgentOwes
	fleetUserMessageType = catalog.FleetUserMessageType
)

// threadFleetState is the one place a thread's turn state is decided. A thread
// Nano holds takes its session's lifecycle label; only a thread Nano does not
// hold falls back to its saved file, where an unanswered prompt is working for
// at most catalog.WorkingSilenceBound.
func threadFleetState(thread Thread, court catalog.FleetCourtMessage, now time.Time) *catalog.FleetState {
	if nanoHolds(thread) {
		return liveFleetState(thread)
	}
	return court.State(thread.ID, now)
}

// nanoHolds reports whether Nano's own lifecycle owns a row's turn state: a
// hosted thread, a live attachment, or a memory row. A hosted thread with no
// live session is idle; its saved file never overrides Nano's record.
func nanoHolds(thread Thread) bool {
	return thread.HostedThreadID != "" || thread.Transient || thread.Runtime != nil && thread.Runtime.Attached
}

// liveFleetState copies the label the session published on its runtime
// overlay, the same value the pane reads.
func liveFleetState(thread Thread) *catalog.FleetState {
	label := fleetIdle
	if thread.Runtime != nil && thread.Runtime.Attached {
		label = thread.Runtime.ThreadStatus
	}
	return catalog.LiveFleetState(thread.ID, label)
}
