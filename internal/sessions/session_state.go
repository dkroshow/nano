package sessions

import (
	"context"

	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/liveview"

	"sync"

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
func (sess *Lifecycle) Close() {
	if err := sess.closeProvider(false); err != nil {
		log.Printf("session %s teardown unconfirmed: %v", sess.session.id, err)
		return
	}
	sess.ConfirmClaudeClose()
}

// ConfirmClaudeClose settles Claude prompts after the shim confirms an explicit close.
// Internal teardown and explicit close both settle only after a successful
// stop. Explicit close additionally checks attachment removal before calling.
func (sess *Lifecycle) ConfirmClaudeClose() {
	if sess.session.provider != "claude" {
		return
	}
	sess.session.stateOwner.stateMu.Lock()
	unconfirmed := sess.session.stateOwner.closeUnconfirmed
	sess.session.stateOwner.closeUnconfirmed = false
	sess.session.stateOwner.stateMu.Unlock()
	if unconfirmed {
		sess.session.delivery.settleAgentTurnsOnExit(true)
		sess.session.stateOwner.updateHostedThread(StatusExited, "closed")
	}
}

// CloseWithResult closes the attachment and returns any provider teardown failure.
// Explicit HTTP close callers must use closeWithResult and preserve failures.
func (sess *Lifecycle) CloseWithResult() error { return sess.closeProvider(true) }

func (sess *Lifecycle) closeProvider(requireReady bool) error {
	sess.closeMu.Lock()
	defer sess.closeMu.Unlock()
	// Mark exited before detaching: a background handshake still in flight must
	// not resurrect the session or its thread routing after teardown
	// (runCodexHandshake and markTransportReady both re-check this under
	// stateMu, and a claude transport that lands afterwards closes itself).
	sess.session.stateOwner.stateMu.Lock()
	if sess.session.stateOwner.closed {
		sess.session.stateOwner.stateMu.Unlock()
		return sess.closeErr
	}
	if requireReady && sess.session.provider == "claude" && sess.session.runtime.claude == nil {
		sess.session.stateOwner.stateMu.Unlock()
		return errors.New("Claude transport is not ready; stop unconfirmed, retry when attachment settles")
	}
	sess.session.stateOwner.closed = true
	sess.session.stateOwner.closeUnconfirmed = sess.session.provider == "claude" && sess.session.runtime.claude != nil
	sess.session.stateOwner.durablyTerminalTurnID = ""
	sess.session.stateOwner.durablyTerminalTurnThreadID = ""
	sess.session.stateOwner.stateMu.Unlock()
	sess.session.stateOwner.releaseCatalogSource()
	sess.session.stateOwner.observeConnection(lifecycle.EventClose)
	claude := sess.session.runtime.claudeHandle()
	grok := sess.session.runtime.grokHandle()
	switch {
	case sess.session.runtime.codex != nil:
		sess.session.runtime.codex.closeSession(sess.session)
	case claude != nil:
		// Same rule as codex (closeSession): answer everything still parked
		// before the transport goes away. A shim-hosted child outlives nano, so
		// an unanswered can_use_tool would hold its turn open with no operator
		// anywhere able to reach it.
		sess.session.delivery.closeInteractions()
		sess.closeErr = claude.Close()
	case grok != nil:
		// Explicit close rejects parked permissions before disconnecting the
		// proxy. A Nano process restart does not call close, so the leader keeps
		// both the turn and any parked request alive for session/load.
		sess.session.delivery.closeInteractions()
		_ = grok.Close()
	}
	if sess.session.wire.spool != nil {
		if err := sess.session.wire.spool.close(true); err != nil {
			log.Printf("close wire spool %s: %v", sess.session.id, err)
		}
	}
	if core := sess.session.runtime.coreSession(); core != nil {
		core.Close()
	}
	return sess.closeErr
}

// Summary is one session's full summary: its canonical view (three reads),
// its durable name (one read) and its attention flag from the one-pass
// operation map.
func (sess *View) Summary() Summary {
	view, err := hostedstore.MetadataView{}, error(sql.ErrNoRows)
	if sess.session.metadata != nil {
		view, err = sess.session.metadata.View(sess.session.hostedThreadID)
	}
	needsAttention := false
	if sess.session.agentExecutions != nil {
		needsAttention = sess.session.agentExecutions.SessionNeedsAttention(sess.session.hostedThreadID)
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
func (sess *View) SummaryWith(view hostedstore.MetadataView, viewErr error, needsAttention bool) Summary {
	sess.session.stateOwner.stateMu.Lock()
	presentationMode := sess.session.stateOwner.presentationMode
	if presentationMode == "" {
		presentationMode = presentationShared
	}
	summary := Summary{
		ID:                    sess.session.id,
		Provider:              sess.session.provider,
		ThreadID:              sess.session.stateOwner.threadID,
		Name:                  sess.session.stateOwner.displayName,
		Model:                 sess.session.stateOwner.model,
		Cwd:                   sess.session.stateOwner.cwd,
		Status:                sess.session.stateOwner.connectionStatusLocked(),
		HostedThreadID:        sess.session.hostedThreadID,
		InvokerHostedThreadID: sess.session.invokerHostedThreadID,
		InvokerNativeThreadID: sess.session.invokerNativeThreadID,
		InvokerProvider:       sess.session.invokerProvider,
		PresentationMode:      presentationMode,
		SidebarPlacement:      sess.session.stateOwner.sidebarPlacement,
		SpawnMode:             sess.session.spawnMode,
		Error:                 sess.session.stateOwner.spawnError,
		TurnActive:            sess.session.stateOwner.workActiveLocked(),
		ThreadStatus:          sess.session.stateOwner.threadStatusLocked(),
		TurnStartedAtMs:       sess.session.stateOwner.turnStartedAtMs,
		CompactingSinceMs:     sess.session.stateOwner.compactingSinceMsLocked(),
		LastTurnEndedAt:       sess.session.stateOwner.lastTurnEndedAt,
		Waiting:               sess.session.stateOwner.lifecycleState.State.Connection == lifecycle.Ready && sess.session.stateOwner.lifecycleState.State.Work == lifecycle.WaitingOnYou,
		MetadataDegraded:      sess.session.stateOwner.metadataDegraded,
		CreatedAt:             sess.session.createdAt.Format(time.RFC3339),
	}
	if !summary.TurnActive {
		summary.TurnStartedAtMs = 0
	}
	sess.session.stateOwner.stateMu.Unlock()
	summary.RunningTurnInput = sess.session.runtime.acceptsRunningTurnInput()
	if sess.projection != nil {
		summary.ThreadDiagnostics = sess.projection.ThreadDiagnosticsSnapshot()
	}
	summary.NeedsAttention = needsAttention
	summary.FileTranscript = true
	// A hosted session's facts are served exclusively from the canonical view.
	// Local session fields are transport state, not a fallback metadata source.
	summary.Posture, summary.Model, summary.Cwd = hostedstore.Posture{}, "", ""
	if sess.session.metadata == nil || viewErr != nil {
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

func (sess *View) hostedMetadataName(provider, nativeThreadID, fallback string) (hostedstore.MetadataName, bool) {
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
	sess.session.view.PublishProjectionRuntime()
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

// State owns the atomic lifecycle, native identity, turn and replay facts.
type State struct {
	session          *Session
	presentationMode string
	sidebarPlacement string
	cwd              string
	// permissionMode is the resolved creation/resume posture retained on the
	// live attachment. Canonical metadata remains the durable authority; this
	// is the immediate fallback used when a provider asks before that read is
	// available.
	permissionMode string
	// savedTurn reads the saved status of one turn by exact native turn id,
	// to settle a live turn whose terminal event was lost.
	savedTurn func(ctx context.Context, turnID string) (liveview.DurableTurnEvidence, bool)
	// settleWaiting names the delivered prompts a later saved commit may
	// settle: each one this attachment delivered, and each whose saved turn
	// was still open when settlement last ran. While it is empty, a saved
	// commit reads nothing to settle prompts. Guarded by stateMu.
	settleWaiting map[string]bool
	// stoppedTurns names the native turns the operator stopped. A prompt such
	// a turn ended without taking is held for the person, never sent again by
	// itself: Stop means stop. Guarded by stateMu.
	stoppedTurns map[string]bool
	// releaseHandshakeSlot gives back the boot handshake slot this session's
	// provider attach holds (boot_reattach.go); nil when it holds none.
	// savedCaughtUp: an attach of this session has waited for its saved
	// catch-up, so a later one needs a fresh pass. Both stateMu.
	releaseHandshakeSlot func()
	savedCaughtUp        bool
	// catalogRefresh hints the catalog that a known native file may have
	// changed (catalog_nudge.go); an empty path is ignored. Nil when the
	// catalog is unavailable; set once before wire activity, never mutated.
	catalogRefresh func(string)
	// catalogSources holds this attachment's native files in the catalog's
	// watch set from registration until close (catalog_attached.go). Set at
	// registration, so an attachment that never registers holds nothing.
	catalogSources AttachedCatalog
	// catalogSourcePath is learned from codex thread/started and reused on later
	// turn edges. stateMu guards it with the rest of native identity.
	catalogSourcePath string
	// runtimeNotify nudges the thread-index runtime SSE stream on
	// runtime-visible transitions (status changes, turnActive flips). Nil when
	// the runtime hub is unwired; set once at spawn like catalogRefresh.
	runtimeNotify func()
	// rememberAttachment persists this session as a durable attachment once its
	// native thread id is known (attachments.go). The caller builds the record,
	// including its provider — only a handshake knows whether the session is
	// actually durable. Nil when the store is unavailable; set once at spawn
	// like the nudges above.
	rememberAttachment func(rec hostedstore.Attachment)
	// forgetAttachment retires a durable attachment record by identity. Used by
	// the claude `--resume` fallback, whose successor child mints a fresh native
	// id and so leaves the superseded record pointing at nothing. Nil when the
	// store is unavailable; set once at spawn like rememberAttachment.
	forgetAttachment func(provider, nativeThreadID string)
	// updateTurnAnchor persists the live-turn anchor onto this session's
	// attachment record, or clears it (0, 0). Claude-only in practice: codex
	// re-reads its in-flight turn from the thread/resume response, so only
	// claude needs the anchor written down. Nil when the store is unavailable.
	updateTurnAnchor func(provider, nativeThreadID string, startedAtMs, anchorSeq int64)
	// updateShimSession ties this session's attachment record to the shim child
	// now hosting it — the `--resume` respawn window, where the record still
	// names the predecessor's native id but a restart must re-attach to the new
	// child rather than spawn another. Nil when the store is unavailable.
	updateShimSession func(provider, nativeThreadID, shimSessionID string)
	// pendingName is an operator rename made after creation but before the real
	// native id bound. It is not the immutable creation name. applyPendingName
	// gives it precedence over the creation fallback, while the names store keeps
	// any already-durable manual name above both. displayName keeps the winning
	// value available before the catalog exposes its overlay.
	pendingName string
	displayName string
	// metadataDegraded means a newer observation failed to enter the canonical
	// log. It never makes the last committed canonical record unreadable; API
	// reads derive unavailable directly from control.sqlite instead.
	metadataDegraded bool
	// pendingTurnCausation is appended immediately before each Codex turn/start.
	// thread/settings/updated has no request id, so provider ordering is the
	// only correlation supplied by the protocol. Each entry has an id so a
	// failed RPC removes exactly its own snapshot.
	pendingTurnCausation []turnCausationEntry
	turnCausationSeq     int64
	lastSettingsEvidence map[string]turnCausationEntry
	// attachmentGeneration is assigned by the canonical repository before a
	// provider transport binds. Provider seams stamp their evidence with it.
	attachmentGeneration int64
	subscriptionSweep    func(string)
	restartProgress      func(observability.RestartStep)
	restartFailure       func(string)
	catalogBackground    bool // hidden at boot: its catalog holds wait behind foreground ones, bounded
	// lastHumanPromptAt is the newest browser-path (human-origin) prompt on
	// this attachment, a hard availability fact. Durable copy:
	// hosted_threads.last_human_prompt_at via agentExecutions.noteHumanPrompt.
	lastHumanPromptAt time.Time

	stateMu                    sync.Mutex
	threadID                   string
	model                      string
	lifecycleState             lifecycle.Snapshot
	evidenceSequence           uint64
	recoveryActivationPending  bool
	turnRestoreSequence        uint64
	lastProviderTerminalTurnID string
	// A Claude shim replay rebuilds historical turn boundaries before the
	// session can expose one current answer. Keep that work private: unrelated
	// runtime events snapshot every session, so merely suppressing this
	// session's notifications still lets browsers observe a half-replayed turn.
	turnRestoreInProgress  bool
	turnRestoreActive      bool
	turnRestoreAccepted    bool
	turnRestoreID          string
	turnRestoreStartedAtMs int64
	// codexTurnInputs is what this attachment saw of each Codex turn it
	// watched from its start (codex_lifecycle.go). Each handshake empties it.
	// codexTurnEpoch counts link losses and handshakes; codexTurnInputsEpoch
	// is the one whose ordered reset last emptied the map. Proof is usable
	// only while they match.
	codexTurnInputs      map[string]*codexTurnInput
	codexTurnEpoch       uint64
	codexTurnInputsEpoch uint64
	// codexEchoWaits holds, by client id, the Codex deliveries an echo can
	// still bind (expectCodexEcho). Exact positive proof, so a reconnect
	// keeps it.
	codexEchoWaits map[string]*codexEchoWait
	// turnID is the codex turn currently in flight, learned from turn/started
	// and cleared with it. It is what `turn/interrupt` needs alongside threadId,
	// and it is deliberately not a "last turn" memo: an id kept past
	// turn/completed would let the stop button address a finished turn.
	turnID string
	// durablyTerminalTurnID prevents a late resume/handshake in-progress frame
	// from reviving the same provider turn after catalog evidence settled it.
	// Claude ids are ordinal-derived: a resumed shim must reseed that ordinal
	// before it emits another turn, otherwise a reused ordinal is conservatively
	// rejected until a genuinely different provider turn id appears.
	durablyTerminalTurnID       string
	durablyTerminalTurnThreadID string
	// turnStartedAtMs is the immutable epoch-millisecond anchor for the active turn.
	// The browser computes elapsed time. Zero means no start evidence is available.
	turnStartedAtMs int64
	// lastTurnEndedAt is an in-memory wall-clock seed for completion-order
	// navigation. It is deliberately not persisted with attachments: hosted
	// session exactness matters after a browser reload, not after a host restart.
	lastTurnEndedAt string
	// compactingSinceMs is when the provider began compacting the current
	// turn's context (ms epoch), from its own start signal; 0 when it is not
	// compacting. Its end signal, or whatever ends the turn, clears it.
	compactingSinceMs int64
	// turnEffort / turnModel / turnServiceTier are the runtime values merged into
	// every turn/start (codexTurnOverrides). They live under stateMu because the
	// pane changes them between turns; empty means "leave it to the thread".
	turnEffort      string
	turnModel       string
	turnServiceTier string
	// closed records that close() ran — the operator let this session go. It is
	// deliberately narrower than status "exited", which a dying provider also
	// produces: a background handshake landing on a session whose provider died
	// still has state worth capturing, while one landing on a torn-down session
	// has nobody left to hand it to (claude.go markTransportReady).
	closed           bool
	closeUnconfirmed bool   // stateMu: suppress settlement until stop acknowledgement
	spawnError       string // background handshake failure; surfaced as "error" (Slice R3)
	tokenUsage       json.RawMessage
	turnTokenUsage   map[string]*interact.TokenUsage
	lastError        string
	// codexZeroTurnResume marks the boot-only fallback for a daemon-resident
	// thread whose first rollout does not exist yet. Its routing claim stays live
	// until the first accepted turn makes the normal disk-resume path available.
	codexZeroTurnResume          bool
	codexZeroTurnEligible        bool
	codexZeroTurnRecoveryRunning bool
	// interactions holds the server→client requests parked for this session,
	// keyed by JSON-RPC request id. Server-owned truth, not wire inference: the
	// wire buffer is bounded, so a tab opened after the request landed could
	// never rediscover it by replay (interactions.go).
	interactions map[string]interact.PendingInteraction
	// status classifiers reject every provider interaction before it enters the
	// visible pending-card state. rejectedInteractionSeq tells the dispatcher
	// that the otherwise-hidden abnormal attempt must fail closed.
	autoRejectInteractions bool
	rejectedInteractionSeq int64
	state                  map[string]json.RawMessage
}
