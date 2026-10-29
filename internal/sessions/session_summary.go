package sessions

import (
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// volatileSummary snapshots attachment runtime fields without reading canonical stores.
// Fleet and point reads share the same volatile fields without metadata reads.
func (sess *Session) volatileSummary(needsAttention bool) (Summary, bool) {
	sess.stateOwner.stateMu.Lock()
	presentationMode := sess.stateOwner.presentationMode
	if presentationMode == "" {
		presentationMode = presentationShared
	}
	summary := Summary{
		ID: sess.id, Provider: sess.provider, ThreadID: sess.stateOwner.threadID, Name: sess.stateOwner.displayName,
		Model: sess.stateOwner.model, Cwd: sess.stateOwner.cwd, Status: sess.stateOwner.connectionStatusLocked(), HostedThreadID: sess.hostedThreadID,
		InvokerHostedThreadID: sess.invokerHostedThreadID, InvokerNativeThreadID: sess.invokerNativeThreadID,
		InvokerProvider: sess.invokerProvider, PresentationMode: presentationMode, SidebarPlacement: sess.stateOwner.sidebarPlacement, SpawnMode: sess.spawnMode,
		Error: sess.stateOwner.spawnError, TurnActive: sess.stateOwner.workActiveLocked(), ThreadStatus: sess.stateOwner.threadStatusLocked(), TurnStartedAtMs: sess.stateOwner.turnStartedAtMs,
		CompactingSinceMs: sess.stateOwner.compactingSinceMsLocked(), LastTurnEndedAt: sess.stateOwner.lastTurnEndedAt, Waiting: sess.stateOwner.waitingLocked(), MetadataDegraded: sess.stateOwner.metadataDegraded,
		CreatedAt: sess.createdAt.Format(time.RFC3339),
		Posture:   hostedstore.Posture{Model: sess.stateOwner.model, Effort: sess.stateOwner.turnEffort, PermissionMode: sess.stateOwner.permissionMode, ServiceTier: sess.stateOwner.turnServiceTier},
	}
	sess.stateOwner.stateMu.Unlock()
	if summary.Status == StatusExited {
		return Summary{}, false
	}
	if sess.projection != nil {
		summary.ThreadDiagnostics = sess.projection.ThreadDiagnosticsSnapshot()
	}
	summary.NeedsAttention = needsAttention
	return summary, true
}
