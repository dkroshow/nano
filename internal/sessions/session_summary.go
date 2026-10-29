package sessions

import (
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// volatileSummary snapshots attachment runtime fields without reading canonical stores.
// Fleet and point reads share the same volatile fields without metadata reads.
func (attachmentState *State) volatileSummary(needsAttention bool) (Summary, bool) {
	attachmentState.stateMu.Lock()
	presentationMode := attachmentState.presentationMode
	if presentationMode == "" {
		presentationMode = presentationShared
	}
	summary := Summary{
		ID: attachmentState.identity.id, Provider: attachmentState.identity.provider, ThreadID: attachmentState.threadID, Name: attachmentState.displayName,
		Model: attachmentState.model, Cwd: attachmentState.cwd, Status: attachmentState.connectionStatusLocked(), HostedThreadID: attachmentState.identity.hostedThreadID,
		InvokerHostedThreadID: attachmentState.identity.invokerHostedThreadID, InvokerNativeThreadID: attachmentState.identity.invokerNativeThreadID,
		InvokerProvider: attachmentState.identity.invokerProvider, PresentationMode: presentationMode, SidebarPlacement: attachmentState.sidebarPlacement, SpawnMode: attachmentState.identity.spawnMode,
		Error: attachmentState.spawnError, TurnActive: attachmentState.workActiveLocked(), ThreadStatus: attachmentState.threadStatusLocked(), TurnStartedAtMs: attachmentState.turnStartedAtMs,
		CompactingSinceMs: attachmentState.compactingSinceMsLocked(), LastTurnEndedAt: attachmentState.lastTurnEndedAt, Waiting: attachmentState.waitingLocked(), MetadataDegraded: attachmentState.metadataDegraded,
		CreatedAt: attachmentState.identity.createdAt.Format(time.RFC3339),
		Posture:   hostedstore.Posture{Model: attachmentState.model, Effort: attachmentState.turnEffort, PermissionMode: attachmentState.permissionMode, ServiceTier: attachmentState.turnServiceTier},
	}
	attachmentState.stateMu.Unlock()
	if summary.Status == StatusExited {
		return Summary{}, false
	}
	if attachmentState.view.liveProjection() != nil {
		summary.ThreadDiagnostics = attachmentState.view.liveProjection().ThreadDiagnosticsSnapshot()
	}
	summary.NeedsAttention = needsAttention
	return summary, true
}
