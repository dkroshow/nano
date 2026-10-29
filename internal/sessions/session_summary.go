package sessions

import (
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// volatileSummary snapshots attachment runtime fields without reading canonical stores.
// Fleet and point reads share the same volatile fields without metadata reads.
func (sess *State) volatileSummary(needsAttention bool) (Summary, bool) {
	sess.stateMu.Lock()
	presentationMode := sess.presentationMode
	if presentationMode == "" {
		presentationMode = presentationShared
	}
	summary := Summary{
		ID: sess.identity.id, Provider: sess.identity.provider, ThreadID: sess.threadID, Name: sess.displayName,
		Model: sess.model, Cwd: sess.cwd, Status: sess.connectionStatusLocked(), HostedThreadID: sess.identity.hostedThreadID,
		InvokerHostedThreadID: sess.identity.invokerHostedThreadID, InvokerNativeThreadID: sess.identity.invokerNativeThreadID,
		InvokerProvider: sess.identity.invokerProvider, PresentationMode: presentationMode, SidebarPlacement: sess.sidebarPlacement, SpawnMode: sess.identity.spawnMode,
		Error: sess.spawnError, TurnActive: sess.workActiveLocked(), ThreadStatus: sess.threadStatusLocked(), TurnStartedAtMs: sess.turnStartedAtMs,
		CompactingSinceMs: sess.compactingSinceMsLocked(), LastTurnEndedAt: sess.lastTurnEndedAt, Waiting: sess.waitingLocked(), MetadataDegraded: sess.metadataDegraded,
		CreatedAt: sess.identity.createdAt.Format(time.RFC3339),
		Posture:   hostedstore.Posture{Model: sess.model, Effort: sess.turnEffort, PermissionMode: sess.permissionMode, ServiceTier: sess.turnServiceTier},
	}
	sess.stateMu.Unlock()
	if summary.Status == StatusExited {
		return Summary{}, false
	}
	if sess.view.projection != nil {
		summary.ThreadDiagnostics = sess.view.projection.ThreadDiagnosticsSnapshot()
	}
	summary.NeedsAttention = needsAttention
	return summary, true
}
