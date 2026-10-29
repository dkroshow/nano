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
	"github.com/dkroshow/nano/internal/threads"
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
	sess.stateOwner.stateMu.Lock()
	unconfirmed := sess.stateOwner.closeUnconfirmed
	sess.stateOwner.closeUnconfirmed = false
	sess.stateOwner.stateMu.Unlock()
	if unconfirmed {
		sess.delivery.settleAgentTurnsOnExit(true)
		sess.stateOwner.updateHostedThread(StatusExited, "closed")
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
	sess.stateOwner.stateMu.Lock()
	if sess.stateOwner.closed {
		sess.stateOwner.stateMu.Unlock()
		return sess.closeErr
	}
	if requireReady && sess.provider == "claude" && sess.runtime.claude == nil {
		sess.stateOwner.stateMu.Unlock()
		return errors.New("Claude transport is not ready; stop unconfirmed, retry when attachment settles")
	}
	sess.stateOwner.closed = true
	sess.stateOwner.closeUnconfirmed = sess.provider == "claude" && sess.runtime.claude != nil
	sess.stateOwner.durablyTerminalTurnID = ""
	sess.stateOwner.durablyTerminalTurnThreadID = ""
	sess.stateOwner.stateMu.Unlock()
	sess.stateOwner.releaseCatalogSource()
	sess.stateOwner.observeConnection(lifecycle.EventClose)
	claude := sess.runtime.claudeHandle()
	grok := sess.runtime.grokHandle()
	switch {
	case sess.runtime.codex != nil:
		sess.runtime.codex.closeSession(sess)
	case claude != nil:
		// Same rule as codex (closeSession): answer everything still parked
		// before the transport goes away. A shim-hosted child outlives nano, so
		// an unanswered can_use_tool would hold its turn open with no operator
		// anywhere able to reach it.
		sess.delivery.closeInteractions()
		sess.closeErr = claude.Close()
	case grok != nil:
		// Explicit close rejects parked permissions before disconnecting the
		// proxy. A Nano process restart does not call close, so the leader keeps
		// both the turn and any parked request alive for session/load.
		sess.delivery.closeInteractions()
		_ = grok.Close()
	}
	if sess.wire.spool != nil {
		if err := sess.wire.spool.close(true); err != nil {
			log.Printf("close wire spool %s: %v", sess.id, err)
		}
	}
	if core := sess.runtime.coreSession(); core != nil {
		core.Close()
	}
	return sess.closeErr
}

// Summary is one session's full summary: its canonical view (three reads),
// its durable name (one read) and its attention flag from the one-pass
// operation map.
func (sess *Session) Summary() Summary {
	view, err := hostedstore.MetadataView{}, error(sql.ErrNoRows)
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
func (sess *Session) SummaryWith(view hostedstore.MetadataView, viewErr error, needsAttention bool) Summary {
	sess.stateOwner.stateMu.Lock()
	presentationMode := sess.stateOwner.presentationMode
	if presentationMode == "" {
		presentationMode = presentationShared
	}
	summary := Summary{
		ID:                    sess.id,
		Provider:              sess.provider,
		ThreadID:              sess.stateOwner.threadID,
		Name:                  sess.stateOwner.displayName,
		Model:                 sess.stateOwner.model,
		Cwd:                   sess.stateOwner.cwd,
		Status:                sess.stateOwner.connectionStatusLocked(),
		HostedThreadID:        sess.hostedThreadID,
		InvokerHostedThreadID: sess.invokerHostedThreadID,
		InvokerNativeThreadID: sess.invokerNativeThreadID,
		InvokerProvider:       sess.invokerProvider,
		PresentationMode:      presentationMode,
		SidebarPlacement:      sess.stateOwner.sidebarPlacement,
		SpawnMode:             sess.spawnMode,
		Error:                 sess.stateOwner.spawnError,
		TurnActive:            sess.stateOwner.workActiveLocked(),
		ThreadStatus:          sess.stateOwner.threadStatusLocked(),
		TurnStartedAtMs:       sess.stateOwner.turnStartedAtMs,
		CompactingSinceMs:     sess.stateOwner.compactingSinceMsLocked(),
		LastTurnEndedAt:       sess.stateOwner.lastTurnEndedAt,
		Waiting:               sess.stateOwner.lifecycleState.State.Connection == lifecycle.Ready && sess.stateOwner.lifecycleState.State.Work == lifecycle.WaitingOnYou,
		MetadataDegraded:      sess.stateOwner.metadataDegraded,
		CreatedAt:             sess.createdAt.Format(time.RFC3339),
	}
	if !summary.TurnActive {
		summary.TurnStartedAtMs = 0
	}
	sess.stateOwner.stateMu.Unlock()
	summary.RunningTurnInput = sess.runtime.acceptsRunningTurnInput()
	if sess.projection != nil {
		summary.ThreadDiagnostics = sess.projection.ThreadDiagnosticsSnapshot()
	}
	summary.NeedsAttention = needsAttention
	summary.FileTranscript = true
	// A hosted session's facts are served exclusively from the canonical view.
	// Local session fields are transport state, not a fallback metadata source.
	summary.Posture, summary.Model, summary.Cwd = hostedstore.Posture{}, "", ""
	if sess.metadata == nil || viewErr != nil {
		summary.MetadataUnavailable = true
		return summary
	}
	view.Attachment = hostedstore.AttachmentView{State: summary.Status, Provenance: "live_session", Error: summary.Error}
	view.Name = displayMetadataName(summary.Name)
	summary.Posture = hostedstore.PostureFromFields(view.Fields)
	summary.Metadata = &view
	summary.Model = summary.Posture.Model
	if cwd, ok := hostedstore.MetadataString(view.Fields["cwd"]); ok {
		summary.Cwd = cwd
	}
	return summary
}

func (sess *Session) hostedMetadataName(provider, nativeThreadID, fallback string) (hostedstore.MetadataName, bool) {
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
func displayMetadataName(displayName string) hostedstore.MetadataName {
	if displayName == "" {
		return hostedstore.MetadataName{}
	}
	return hostedstore.MetadataName{Value: displayName, Provenance: "manual"}
}

// failSpawn records a background spawn failure: the session stays listed so
// the UI can show what went wrong, with status exited and the message
// surfaced as "error" on the summary and /state (frozen R3 contract).
func (sess *State) failSpawn(message string) {
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
	sess.session.delivery.settleAgentTurnsOnExit(closed)
	sess.releaseCatalogSource()
	sess.session.PublishProjectionRuntime()
	sess.NotifyRuntime()
	sess.updateHostedThread(StatusExited, message)
}

// claudeHandle snapshots the claude provider handle, which a fresh start now
// publishes from a background goroutine (markTransportReady, claude.go). Nil
// means the transport is not up yet: readiness and the handle are published
// under one lock, but the child's own init line can promote a session a moment
// earlier, so every caller checks rather than trusting "ready".
func (sess *Runtime) claudeHandle() *claudeSession {
	sess.session.stateOwner.stateMu.Lock()
	defer sess.session.stateOwner.stateMu.Unlock()
	return sess.claude
}

func (sess *Runtime) grokHandle() *grokSession {
	sess.session.stateOwner.stateMu.Lock()
	defer sess.session.stateOwner.stateMu.Unlock()
	return sess.grok
}

func (sess *State) setState(key string, v json.RawMessage) {
	sess.stateMu.Lock()
	sess.state[key] = v
	sess.stateMu.Unlock()
}

func (sess *Delivery) settleAgentTurnsOnExit(closed bool) {
	sess.session.stateOwner.stateMu.Lock()
	unconfirmed := sess.session.stateOwner.closeUnconfirmed
	sess.session.stateOwner.stateMu.Unlock()
	if unconfirmed {
		return
	}
	if sess.session.agentExecutions == nil || sess.session.hostedThreadID == "" {
		return
	}
	turnStatus := agentTurnDeliveryUnknown
	executionErr := &threads.ExecutionError{
		Source: "transport", Code: "session_exited",
		Message: "the Nano session exited without provider terminal evidence",
	}
	if closed {
		turnStatus = agentTurnInterrupted
		executionErr = &threads.ExecutionError{
			Source: "client", Code: "session_closed", Message: "the caller closed the hosted thread",
		}
	}
	if err := sess.session.agentExecutions.MarkSessionTurns(sess.session.hostedThreadID, turnStatus, executionErr); err != nil {
		log.Printf("settle turns for exited hosted thread %s: %v", sess.session.hostedThreadID, err)
	}
}

func (sess *State) updateHostedThread(status, message string) {
	sess.stateMu.Lock()
	unconfirmed := sess.closeUnconfirmed
	sess.stateMu.Unlock()
	if unconfirmed {
		return
	}
	if sess.session.hostedThreadID == "" {
		return
	}
	if sess.session.metadata != nil {
		if err := hostedstore.SkipOwnedNativeClaim(sess.session.metadata.ClaimNativeID(sess.session.hostedThreadID, sess.session.provider, sess.ThreadID(), NativeThreadIDProvisional(sess.session.provider, sess.ThreadID()))); err != nil {
			log.Printf("claim canonical native id for %s: %v", sess.session.hostedThreadID, err)
		}
	}
	if sess.session.agentExecutions == nil {
		return
	}
	if err := sess.session.agentExecutions.UpdateSession(
		sess.session.hostedThreadID, sess.session.id, sess.ThreadID(), status, message,
	); err != nil {
		log.Printf("update hosted thread %s: %v", sess.session.hostedThreadID, err)
	}
}

func (sess *State) rememberDurableAttachment(rec hostedstore.Attachment) {
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
	rec.HostedThreadID = sess.session.hostedThreadID
	// Cwd/model/tier are canonical control.sqlite facts. The attachment JSON is
	// retained solely as a read-only migration/transport bridge and must never
	// regain posture authority from a live session.
	rec.Cwd, rec.Model, rec.TurnServiceTier = "", "", ""
	rec.InvokerHostedThreadID = sess.session.invokerHostedThreadID
	rec.InvokerNativeThreadID = sess.session.invokerNativeThreadID
	rec.InvokerProvider = sess.session.invokerProvider
	// presentationMode is mutable under stateMu (the presentation endpoint
	// flips it mid-life), unlike the invoker fields above, so its read locks.
	sess.stateMu.Lock()
	rec.PresentationMode = sess.presentationMode
	sess.stateMu.Unlock()
	sess.rememberAttachment(rec)
}

// recordRestartStep reports progress without owning the restart record.
func (sess *State) recordRestartStep(step observability.RestartStep) {
	if sess.restartProgress != nil {
		sess.restartProgress(step)
	}
}

// recordRestartFailure reports the provider outcome to the restart owner.
func (sess *State) recordRestartFailure(reason string) {
	if sess.restartFailure != nil {
		sess.restartFailure(reason)
	}
}

// holdHandshakeRelease records the slot held by this attachment until catch-up.
func (sess *State) holdHandshakeRelease(release func()) {
	sess.stateMu.Lock()
	sess.releaseHandshakeSlot = release
	sess.stateMu.Unlock()
}

// SetPresentation updates the attachment presentation mode.
func (sess *State) SetPresentation(mode string) {
	sess.stateMu.Lock()
	sess.presentationMode = mode
	sess.stateMu.Unlock()
}

// SetSidebarPlacement updates the attachment sidebar placement.
func (sess *State) SetSidebarPlacement(placement string) {
	sess.stateMu.Lock()
	sess.sidebarPlacement = placement
	sess.stateMu.Unlock()
}

// IsClosed reports whether attachment teardown has begun.
func (sess *State) IsClosed() bool {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.closed
}

// ReadyAndWorking reports whether the connected attachment is executing a turn.
func (sess *State) ReadyAndWorking() (ready, working bool) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	ready = sess.lifecycleState.State.Connection == lifecycle.Ready
	return ready, ready && sess.lifecycleState.State.Work == lifecycle.Working
}
