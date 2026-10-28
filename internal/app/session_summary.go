package app

import (
	"time"
)

// Fleet and point reads share the same volatile fields without metadata reads.
func (sess *session) volatileSummary(needsAttention bool) (sessionSummary, bool) {
	sess.stateMu.Lock()
	presentationMode := sess.presentationMode
	if presentationMode == "" {
		presentationMode = presentationShared
	}
	summary := sessionSummary{
		ID: sess.id, Provider: sess.provider, ThreadID: sess.threadID, Name: sess.displayName,
		Model: sess.model, Cwd: sess.cwd, Status: sess.connectionStatusLocked(), HostedThreadID: sess.hostedThreadID,
		InvokerHostedThreadID: sess.invokerHostedThreadID, InvokerNativeThreadID: sess.invokerNativeThreadID,
		InvokerProvider: sess.invokerProvider, PresentationMode: presentationMode, SidebarPlacement: sess.sidebarPlacement, SpawnMode: sess.spawnMode,
		Error: sess.spawnError, TurnActive: sess.workActiveLocked(), ThreadStatus: sess.threadStatusLocked(), TurnStartedAtMs: sess.turnStartedAtMs,
		CompactingSinceMs: sess.compactingSinceMsLocked(), LastTurnEndedAt: sess.lastTurnEndedAt, Waiting: sess.waitingLocked(), MetadataDegraded: sess.metadataDegraded,
		CreatedAt: sess.createdAt.Format(time.RFC3339),
		Posture:   sessionPosture{Model: sess.model, Effort: sess.turnEffort, PermissionMode: sess.permissionMode, ServiceTier: sess.turnServiceTier},
	}
	sess.stateMu.Unlock()
	if summary.Status == sessionStatusExited {
		return sessionSummary{}, false
	}
	if sess.projection != nil {
		summary.ThreadDiagnostics = sess.projection.threadDiagnosticsSnapshot()
	}
	summary.NeedsAttention = needsAttention
	return summary, true
}
