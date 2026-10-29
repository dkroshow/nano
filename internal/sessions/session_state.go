package sessions

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/observability"
)

// Close tears down the session's provider attachment.
func (sess *Session) Close() {
	if err := sess.closeProvider(false); err != nil {
		log.Printf("session %s teardown unconfirmed: %v", sess.id, err)
		return
	}
	sess.ConfirmClaudeClose()
}

// ConfirmClaudeClose settles Claude prompts after the shim confirms an explicit close.
// Internal teardown and explicit close both settle only after a successful
// stop. Explicit close additionally checks attachment removal before calling.
func (sess *Session) ConfirmClaudeClose() {
	if sess.provider != "claude" {
		return
	}
	sess.stateMu.Lock()
	unconfirmed := sess.closeUnconfirmed
	sess.closeUnconfirmed = false
	sess.stateMu.Unlock()
	if unconfirmed {
		sess.settleAgentTurnsOnExit(true)
		sess.updateHostedThread(StatusExited, "closed")
	}
}

// CloseWithResult closes the attachment and returns any provider teardown failure.
// Explicit HTTP close callers must use closeWithResult and preserve failures.
func (sess *Session) CloseWithResult() error { return sess.closeProvider(true) }

func (sess *Session) closeProvider(requireReady bool) error {
	sess.closeMu.Lock()
	defer sess.closeMu.Unlock()
	// Mark exited before detaching: a background handshake still in flight must
	// not resurrect the session or its thread routing after teardown
	// (runCodexHandshake and markTransportReady both re-check this under
	// stateMu, and a claude transport that lands afterwards closes itself).
	sess.stateMu.Lock()
	if sess.closed {
		sess.stateMu.Unlock()
		return sess.closeErr
	}
	if requireReady && sess.provider == "claude" && sess.claude == nil {
		sess.stateMu.Unlock()
		return errors.New("Claude transport is not ready; stop unconfirmed, retry when attachment settles")
	}
	sess.closed = true
	sess.closeUnconfirmed = sess.provider == "claude" && sess.claude != nil
	sess.durablyTerminalTurnID = ""
	sess.durablyTerminalTurnThreadID = ""
	sess.stateMu.Unlock()
	sess.releaseCatalogSource()
	sess.observeConnection(lifecycle.EventClose)
	claude := sess.claudeHandle()
	grok := sess.grokHandle()
	switch {
	case sess.codex != nil:
		sess.codex.closeSession(sess)
	case claude != nil:
		// Same rule as codex (closeSession): answer everything still parked
		// before the transport goes away. A shim-hosted child outlives nano, so
		// an unanswered can_use_tool would hold its turn open with no operator
		// anywhere able to reach it.
		sess.closeInteractions()
		sess.closeErr = claude.Close()
	case grok != nil:
		// Explicit close rejects parked permissions before disconnecting the
		// proxy. A Nano process restart does not call close, so the leader keeps
		// both the turn and any parked request alive for session/load.
		sess.closeInteractions()
		_ = grok.Close()
	}
	if sess.spool != nil {
		if err := sess.spool.close(true); err != nil {
			log.Printf("close wire spool %s: %v", sess.id, err)
		}
	}
	if core := sess.coreSession(); core != nil {
		core.Close()
	}
	return sess.closeErr
}

// Summary is one session's full summary: its canonical view (three reads),
// its durable name (one read) and its attention flag from the one-pass
// operation map.
func (sess *Session) Summary() Summary {
	view, err := hostedMetadataView{}, error(sql.ErrNoRows)
	if sess.metadata != nil {
		view, err = sess.metadata.View(sess.hostedThreadID)
	}
	needsAttention := false
	if sess.agentExecutions != nil {
		needsAttention = sess.agentExecutions.SessionNeedsAttention(sess.hostedThreadID)
	}
	summary := sess.SummaryWith(view, err, needsAttention)
	if summary.Metadata != nil {
		summary.Metadata.Name, _ = sess.hostedMetadataName(summary.Provider, summary.ThreadID, summary.Name)
	}
	return summary
}

// SummaryWith builds a summary from a canonical view already read (viewErr
// set when there is none) with no store read of its own, so a list reads all
// views in one batch. The view's name is the display name; summary replaces
// it with the durable name for one session. The sessions list carries no name
// in its metadata.
func (sess *Session) SummaryWith(view hostedMetadataView, viewErr error, needsAttention bool) Summary {
	sess.stateMu.Lock()
	presentationMode := sess.presentationMode
	if presentationMode == "" {
		presentationMode = presentationShared
	}
	summary := Summary{
		ID:                    sess.id,
		Provider:              sess.provider,
		ThreadID:              sess.threadID,
		Name:                  sess.displayName,
		Model:                 sess.model,
		Cwd:                   sess.cwd,
		Status:                sess.connectionStatusLocked(),
		HostedThreadID:        sess.hostedThreadID,
		InvokerHostedThreadID: sess.invokerHostedThreadID,
		InvokerNativeThreadID: sess.invokerNativeThreadID,
		InvokerProvider:       sess.invokerProvider,
		PresentationMode:      presentationMode,
		SidebarPlacement:      sess.sidebarPlacement,
		SpawnMode:             sess.spawnMode,
		Error:                 sess.spawnError,
		TurnActive:            sess.workActiveLocked(),
		ThreadStatus:          sess.threadStatusLocked(),
		TurnStartedAtMs:       sess.turnStartedAtMs,
		CompactingSinceMs:     sess.compactingSinceMsLocked(),
		LastTurnEndedAt:       sess.lastTurnEndedAt,
		Waiting:               sess.lifecycleState.State.Connection == lifecycle.Ready && sess.lifecycleState.State.Work == lifecycle.WaitingOnYou,
		MetadataDegraded:      sess.metadataDegraded,
		CreatedAt:             sess.createdAt.Format(time.RFC3339),
	}
	if !summary.TurnActive {
		summary.TurnStartedAtMs = 0
	}
	sess.stateMu.Unlock()
	summary.RunningTurnInput = sess.acceptsRunningTurnInput()
	if sess.projection != nil {
		summary.ThreadDiagnostics = sess.projection.ThreadDiagnosticsSnapshot()
	}
	summary.NeedsAttention = needsAttention
	summary.FileTranscript = true
	// A hosted session's facts are served exclusively from the canonical view.
	// Local session fields are transport state, not a fallback metadata source.
	summary.Posture, summary.Model, summary.Cwd = sessionPosture{}, "", ""
	if sess.metadata == nil || viewErr != nil {
		summary.MetadataUnavailable = true
		return summary
	}
	view.Attachment = hostedAttachmentView{State: summary.Status, Provenance: "live_session", Error: summary.Error}
	view.Name = displayMetadataName(summary.Name)
	summary.Posture = hostedstore.PostureFromFields(view.Fields)
	summary.Metadata = &view
	summary.Model = summary.Posture.Model
	if cwd, ok := hostedstore.MetadataString(view.Fields["cwd"]); ok {
		summary.Cwd = cwd
	}
	return summary
}

func (sess *Session) hostedMetadataName(provider, nativeThreadID, fallback string) (hostedMetadataName, bool) {
	if sess.metadataName != nil {
		if name, ok := sess.metadataName(provider, nativeThreadID); ok {
			return name, true
		}
	}
	name := displayMetadataName(fallback)
	return name, name.Value != ""
}

// displayMetadataName is a hosted view's name when the names store holds
// none: the session's display name, as a manual name, or no name.
func displayMetadataName(displayName string) hostedMetadataName {
	if displayName == "" {
		return hostedMetadataName{}
	}
	return hostedMetadataName{Value: displayName, Provenance: "manual"}
}

// failSpawn records a background spawn failure: the session stays listed so
// the UI can show what went wrong, with status exited and the message
// surfaced as "error" on the summary and /state (frozen R3 contract).
func (sess *Session) failSpawn(message string) {
	sess.recordRestartFailure("handshake_failed")
	sess.stateMu.Lock()
	wasActive := sess.workActiveLocked()
	sess.spawnError = message
	sess.transitionLifecycleLocked(lifecycle.EventRuntimeExited)
	closed := sess.closed
	if wasActive {
		sess.lastTurnEndedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	sess.turnStartedAtMs = 0 // clearing site 3 of 3 (D14)
	sess.clearTurnRestoreLocked()
	sess.stateMu.Unlock()
	sess.settleAgentTurnsOnExit(closed)
	sess.releaseCatalogSource()
	sess.PublishProjectionRuntime()
	sess.NotifyRuntime()
	sess.updateHostedThread(StatusExited, message)
}

// claudeHandle snapshots the claude provider handle, which a fresh start now
// publishes from a background goroutine (markTransportReady, claude.go). Nil
// means the transport is not up yet: readiness and the handle are published
// under one lock, but the child's own init line can promote a session a moment
// earlier, so every caller checks rather than trusting "ready".
func (sess *Session) claudeHandle() *claudeSession {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.claude
}

func (sess *Session) grokHandle() *grokSession {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.grok
}

func (sess *Session) setState(key string, v json.RawMessage) {
	sess.stateMu.Lock()
	sess.state[key] = v
	sess.stateMu.Unlock()
}

func (sess *Session) settleAgentTurnsOnExit(closed bool) {
	sess.stateMu.Lock()
	unconfirmed := sess.closeUnconfirmed
	sess.stateMu.Unlock()
	if unconfirmed {
		return
	}
	if sess.agentExecutions == nil || sess.hostedThreadID == "" {
		return
	}
	turnStatus := agentTurnDeliveryUnknown
	executionErr := &agentExecutionError{
		Source: "transport", Code: "session_exited",
		Message: "the Nano session exited without provider terminal evidence",
	}
	if closed {
		turnStatus = agentTurnInterrupted
		executionErr = &agentExecutionError{
			Source: "client", Code: "session_closed", Message: "the caller closed the hosted thread",
		}
	}
	if err := sess.agentExecutions.MarkSessionTurns(sess.hostedThreadID, turnStatus, executionErr); err != nil {
		log.Printf("settle turns for exited hosted thread %s: %v", sess.hostedThreadID, err)
	}
}

func (sess *Session) updateHostedThread(status, message string) {
	sess.stateMu.Lock()
	unconfirmed := sess.closeUnconfirmed
	sess.stateMu.Unlock()
	if unconfirmed {
		return
	}
	if sess.hostedThreadID == "" {
		return
	}
	if sess.metadata != nil {
		if err := hostedstore.SkipOwnedNativeClaim(sess.metadata.ClaimNativeID(sess.hostedThreadID, sess.provider, sess.ThreadID(), NativeThreadIDProvisional(sess.provider, sess.ThreadID()))); err != nil {
			log.Printf("claim canonical native id for %s: %v", sess.hostedThreadID, err)
		}
	}
	if sess.agentExecutions == nil {
		return
	}
	if err := sess.agentExecutions.UpdateSession(
		sess.hostedThreadID, sess.id, sess.ThreadID(), status, message,
	); err != nil {
		log.Printf("update hosted thread %s: %v", sess.hostedThreadID, err)
	}
}

func (sess *Session) rememberDurableAttachment(rec attachmentRecord) {
	if sess.rememberAttachment == nil {
		return
	}
	// A never-prompted Claude child has only its shim handle. Keep the New
	// Window title beside that provisional identity until system.init supplies
	// the real native id and applyPendingName can write the ordinary name
	// overlay. Real attachment records must not retain this staging value: a
	// later manual rename is authoritative and must never be overwritten at boot.
	if rec.Provisional() {
		sess.stateMu.Lock()
		rec.PendingName = sess.pendingName
		sess.stateMu.Unlock()
	}
	rec.HostedThreadID = sess.hostedThreadID
	// Cwd/model/tier are canonical control.sqlite facts. The attachment JSON is
	// retained solely as a read-only migration/transport bridge and must never
	// regain posture authority from a live session.
	rec.Cwd, rec.Model, rec.TurnServiceTier = "", "", ""
	rec.InvokerHostedThreadID = sess.invokerHostedThreadID
	rec.InvokerNativeThreadID = sess.invokerNativeThreadID
	rec.InvokerProvider = sess.invokerProvider
	// presentationMode is mutable under stateMu (the presentation endpoint
	// flips it mid-life), unlike the invoker fields above, so its read locks.
	sess.stateMu.Lock()
	rec.PresentationMode = sess.presentationMode
	sess.stateMu.Unlock()
	sess.rememberAttachment(rec)
}

// recordRestartStep reports progress without owning the restart record.
func (sess *Session) recordRestartStep(step observability.RestartStep) {
	if sess.restartProgress != nil {
		sess.restartProgress(step)
	}
}

// recordRestartFailure reports the provider outcome to the restart owner.
func (sess *Session) recordRestartFailure(reason string) {
	if sess.restartFailure != nil {
		sess.restartFailure(reason)
	}
}

// holdHandshakeRelease records the slot held by this attachment until catch-up.
func (sess *Session) holdHandshakeRelease(release func()) {
	sess.stateMu.Lock()
	sess.releaseHandshakeSlot = release
	sess.stateMu.Unlock()
}

// SetPresentation updates the attachment presentation mode.
func (sess *Session) SetPresentation(mode string) {
	sess.stateMu.Lock()
	sess.presentationMode = mode
	sess.stateMu.Unlock()
}

// SetSidebarPlacement updates the attachment sidebar placement.
func (sess *Session) SetSidebarPlacement(placement string) {
	sess.stateMu.Lock()
	sess.sidebarPlacement = placement
	sess.stateMu.Unlock()
}

// IsClosed reports whether attachment teardown has begun.
func (sess *Session) IsClosed() bool {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.closed
}

// ReadyAndWorking reports whether the connected attachment is executing a turn.
func (sess *Session) ReadyAndWorking() (ready, working bool) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	ready = sess.lifecycleState.State.Connection == lifecycle.Ready
	return ready, ready && sess.lifecycleState.State.Work == lifecycle.Working
}
