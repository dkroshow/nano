package sessions

import (
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// volatileSummary snapshots attachment runtime fields without reading canonical stores.
// Fleet and point reads share the same volatile fields without metadata reads.
func (sess *View) volatileSummary(needsAttention bool) (Summary, bool) {
	sess.session.stateOwner.stateMu.Lock()
	presentationMode := sess.session.stateOwner.presentationMode
	if presentationMode == "" {
		presentationMode = presentationShared
	}
	summary := Summary{
		ID: sess.session.id, Provider: sess.session.provider, ThreadID: sess.session.stateOwner.threadID, Name: sess.session.stateOwner.displayName,
		Model: sess.session.stateOwner.model, Cwd: sess.session.stateOwner.cwd, Status: sess.session.stateOwner.connectionStatusLocked(), HostedThreadID: sess.session.hostedThreadID,
		InvokerHostedThreadID: sess.session.invokerHostedThreadID, InvokerNativeThreadID: sess.session.invokerNativeThreadID,
		InvokerProvider: sess.session.invokerProvider, PresentationMode: presentationMode, SidebarPlacement: sess.session.stateOwner.sidebarPlacement, SpawnMode: sess.session.spawnMode,
		Error: sess.session.stateOwner.spawnError, TurnActive: sess.session.stateOwner.workActiveLocked(), ThreadStatus: sess.session.stateOwner.threadStatusLocked(), TurnStartedAtMs: sess.session.stateOwner.turnStartedAtMs,
		CompactingSinceMs: sess.session.stateOwner.compactingSinceMsLocked(), LastTurnEndedAt: sess.session.stateOwner.lastTurnEndedAt, Waiting: sess.session.stateOwner.waitingLocked(), MetadataDegraded: sess.session.stateOwner.metadataDegraded,
		CreatedAt: sess.session.createdAt.Format(time.RFC3339),
		Posture:   hostedstore.Posture{Model: sess.session.stateOwner.model, Effort: sess.session.stateOwner.turnEffort, PermissionMode: sess.session.stateOwner.permissionMode, ServiceTier: sess.session.stateOwner.turnServiceTier},
	}
	sess.session.stateOwner.stateMu.Unlock()
	if summary.Status == StatusExited {
		return Summary{}, false
	}
	if sess.projection != nil {
		summary.ThreadDiagnostics = sess.projection.ThreadDiagnosticsSnapshot()
	}
	summary.NeedsAttention = needsAttention
	return summary, true
}
