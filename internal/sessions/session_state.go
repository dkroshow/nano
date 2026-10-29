package sessions

import (
	"context"
	"maps"

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
func (teardown *Teardown) Close() {
	if err := teardown.closeProvider(false); err != nil {
		log.Printf("session %s teardown unconfirmed: %v", teardown.identity.id, err)
		return
	}
	teardown.state.ConfirmClaudeClose()
}

// ConfirmClaudeClose settles Claude prompts after the shim confirms an explicit close.
// Internal teardown and explicit close both settle only after a successful
// stop. Explicit close additionally checks attachment removal before calling.
func (attachmentState *State) ConfirmClaudeClose() {
	if attachmentState.identity.provider != "claude" {
		return
	}
	attachmentState.stateMu.Lock()
	unconfirmed := attachmentState.closeUnconfirmed
	attachmentState.closeUnconfirmed = false
	attachmentState.stateMu.Unlock()
	if unconfirmed {
		attachmentState.settleAgentTurnsOnExit(true)
		attachmentState.updateHostedThread(StatusExited, "closed")
	}
}

// CloseWithResult closes the attachment and returns any provider teardown failure.
// Explicit HTTP close callers must use closeWithResult and preserve failures.
func (teardown *Teardown) CloseWithResult() error { return teardown.closeProvider(true) }

func (teardown *Teardown) closeProvider(requireReady bool) error {
	teardown.closeMu.Lock()
	defer teardown.closeMu.Unlock()
	// Mark exited before detaching: a background handshake still in flight must
	// not resurrect the session or its thread routing after teardown
	// (runCodexHandshake and markTransportReady both re-check this under
	// stateMu, and a claude transport that lands afterwards closes itself).
	alreadyClosed, err := teardown.state.beginClose(requireReady)
	if alreadyClosed {
		return teardown.closeErr
	}
	if err != nil {
		return err
	}
	teardown.state.releaseCatalogSource()
	teardown.state.observeConnection(lifecycle.EventClose)
	claude := teardown.state.claudeHandle()
	grok := teardown.state.grokHandle()
	switch {
	case teardown.runtime.daemon() != nil:
		teardown.runtime.daemon().closeSession(teardown.runtime)
	case claude != nil:
		// Same rule as codex (closeSession): answer everything still parked
		// before the transport goes away. A shim-hosted child outlives nano, so
		// an unanswered can_use_tool would hold its turn open with no operator
		// anywhere able to reach it.
		teardown.delivery.closeInteractions()
		teardown.closeErr = claude.Close()
	case grok != nil:
		// Explicit close rejects parked permissions before disconnecting the
		// proxy. A Nano process restart does not call close, so the leader keeps
		// both the turn and any parked request alive for session/load.
		teardown.delivery.closeInteractions()
		_ = grok.Close()
	}
	teardown.wire.closeSpool()
	if core := teardown.runtime.coreSession(); core != nil {
		core.Close()
	}
	return teardown.closeErr
}

// Summary is one session's full summary: its canonical view (three reads),
// its durable name (one read) and its attention flag from the one-pass
// operation map.
func (views *View) Summary() Summary {
	view, err := hostedstore.MetadataView{}, error(sql.ErrNoRows)
	if views.identity.metadata != nil {
		view, err = views.identity.metadata.View(views.identity.hostedThreadID)
	}
	needsAttention := false
	if views.identity.agentExecutions != nil {
		needsAttention = views.identity.agentExecutions.SessionNeedsAttention(views.identity.hostedThreadID)
	}
	summary := views.state.SummaryWith(view, err, needsAttention)
	if summary.Metadata != nil {
		summary.Metadata.Name, _ = views.hostedMetadataName(summary.Provider, summary.ThreadID, summary.Name)
	}
	return summary
}

// SummaryWith builds a summary from a canonical view already read (viewErr
// set when there is none) with no store read of its own, so a list reads all
// views in one batch. The view's name is the display name; summary replaces
// it with the durable name for one session. The sessions list carries no name
// in its metadata.
func (attachmentState *State) SummaryWith(view hostedstore.MetadataView, viewErr error, needsAttention bool) Summary {
	attachmentState.stateMu.Lock()
	presentationMode := attachmentState.presentationMode
	if presentationMode == "" {
		presentationMode = presentationShared
	}
	summary := Summary{
		ID:                    attachmentState.identity.id,
		Provider:              attachmentState.identity.provider,
		ThreadID:              attachmentState.threadID,
		Name:                  attachmentState.displayName,
		Model:                 attachmentState.model,
		Cwd:                   attachmentState.cwd,
		Status:                attachmentState.connectionStatusLocked(),
		HostedThreadID:        attachmentState.identity.hostedThreadID,
		InvokerHostedThreadID: attachmentState.identity.invokerHostedThreadID,
		InvokerNativeThreadID: attachmentState.identity.invokerNativeThreadID,
		InvokerProvider:       attachmentState.identity.invokerProvider,
		PresentationMode:      presentationMode,
		SidebarPlacement:      attachmentState.sidebarPlacement,
		SpawnMode:             attachmentState.identity.spawnMode,
		Error:                 attachmentState.spawnError,
		TurnActive:            attachmentState.workActiveLocked(),
		ThreadStatus:          attachmentState.threadStatusLocked(),
		TurnStartedAtMs:       attachmentState.turnStartedAtMs,
		CompactingSinceMs:     attachmentState.compactingSinceMsLocked(),
		LastTurnEndedAt:       attachmentState.lastTurnEndedAt,
		Waiting:               attachmentState.lifecycleState.State.Connection == lifecycle.Ready && attachmentState.lifecycleState.State.Work == lifecycle.WaitingOnYou,
		MetadataDegraded:      attachmentState.metadataDegraded,
		CreatedAt:             attachmentState.identity.createdAt.Format(time.RFC3339),
	}
	if !summary.TurnActive {
		summary.TurnStartedAtMs = 0
	}
	attachmentState.stateMu.Unlock()
	summary.RunningTurnInput = attachmentState.runtime.acceptsRunningTurnInput()
	if attachmentState.view.liveProjection() != nil {
		summary.ThreadDiagnostics = attachmentState.view.liveProjection().ThreadDiagnosticsSnapshot()
	}
	summary.NeedsAttention = needsAttention
	summary.FileTranscript = true
	// A hosted session's facts are served exclusively from the canonical view.
	// Local session fields are transport state, not a fallback metadata source.
	summary.Posture, summary.Model, summary.Cwd = hostedstore.Posture{}, "", ""
	if attachmentState.identity.metadata == nil || viewErr != nil {
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

func (views *View) hostedMetadataName(provider, nativeThreadID, fallback string) (hostedstore.MetadataName, bool) {
	if views.metadataName != nil {
		if name, ok := views.metadataName(provider, nativeThreadID); ok {
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
func (attachmentState *State) failSpawn(message string) {
	attachmentState.recordRestartFailure("handshake_failed")
	attachmentState.stateMu.Lock()
	wasActive := attachmentState.workActiveLocked()
	attachmentState.spawnError = message
	attachmentState.transitionLifecycleLocked(lifecycle.EventRuntimeExited)
	closed := attachmentState.closed
	if wasActive {
		attachmentState.lastTurnEndedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	attachmentState.turnStartedAtMs = 0 // clearing site 3 of 3 (D14)
	attachmentState.clearTurnRestoreLocked()
	attachmentState.stateMu.Unlock()
	attachmentState.settleAgentTurnsOnExit(closed)
	attachmentState.releaseCatalogSource()
	attachmentState.PublishProjectionRuntime()
	attachmentState.NotifyRuntime()
	attachmentState.updateHostedThread(StatusExited, message)
}

// claudeHandle snapshots the claude provider handle, which a fresh start now
// publishes from a background goroutine (markTransportReady, claude.go). Nil
// means the transport is not up yet: readiness and the handle are published
// under one lock, but the child's own init line can promote a session a moment
// earlier, so every caller checks rather than trusting "ready".
func (attachmentState *State) claudeHandle() *claudeSession {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	return attachmentState.claude
}

func (attachmentState *State) grokHandle() *grokSession {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	return attachmentState.grok
}

func (attachmentState *State) setState(key string, v json.RawMessage) {
	attachmentState.stateMu.Lock()
	attachmentState.state[key] = v
	attachmentState.stateMu.Unlock()
}

func (attachmentState *State) settleAgentTurnsOnExit(closed bool) {
	attachmentState.stateMu.Lock()
	unconfirmed := attachmentState.closeUnconfirmed
	attachmentState.stateMu.Unlock()
	if unconfirmed {
		return
	}
	if attachmentState.identity.agentExecutions == nil || attachmentState.identity.hostedThreadID == "" {
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
	if err := attachmentState.identity.agentExecutions.MarkSessionTurns(attachmentState.identity.hostedThreadID, turnStatus, executionErr); err != nil {
		log.Printf("settle turns for exited hosted thread %s: %v", attachmentState.identity.hostedThreadID, err)
	}
}

func (attachmentState *State) updateHostedThread(status, message string) {
	attachmentState.stateMu.Lock()
	unconfirmed := attachmentState.closeUnconfirmed
	attachmentState.stateMu.Unlock()
	if unconfirmed {
		return
	}
	if attachmentState.identity.hostedThreadID == "" {
		return
	}
	if attachmentState.identity.metadata != nil {
		if err := hostedstore.SkipOwnedNativeClaim(attachmentState.identity.metadata.ClaimNativeID(attachmentState.identity.hostedThreadID, attachmentState.identity.provider, attachmentState.ThreadID(), NativeThreadIDProvisional(attachmentState.identity.provider, attachmentState.ThreadID()))); err != nil {
			log.Printf("claim canonical native id for %s: %v", attachmentState.identity.hostedThreadID, err)
		}
	}
	if attachmentState.identity.agentExecutions == nil {
		return
	}
	if err := attachmentState.identity.agentExecutions.UpdateSession(
		attachmentState.identity.hostedThreadID, attachmentState.identity.id, attachmentState.ThreadID(), status, message,
	); err != nil {
		log.Printf("update hosted thread %s: %v", attachmentState.identity.hostedThreadID, err)
	}
}

func (attachmentState *State) rememberDurableAttachment(rec hostedstore.Attachment) {
	if attachmentState.rememberAttachment == nil {
		return
	}
	// A never-prompted Claude child has only its shim handle. Keep the New
	// Window title beside that provisional identity until system.init supplies
	// the real native id and applyPendingName can write the ordinary name
	// overlay. Real attachment records must not retain this staging value: a
	// later manual rename is authoritative and must never be overwritten at boot.
	if rec.Provisional() {
		attachmentState.stateMu.Lock()
		rec.PendingName = attachmentState.pendingName
		attachmentState.stateMu.Unlock()
	}
	rec.HostedThreadID = attachmentState.identity.hostedThreadID
	// Cwd/model/tier are canonical control.sqlite facts. The attachment JSON is
	// retained solely as a read-only migration/transport bridge and must never
	// regain posture authority from a live session.
	rec.Cwd, rec.Model, rec.TurnServiceTier = "", "", ""
	rec.InvokerHostedThreadID = attachmentState.identity.invokerHostedThreadID
	rec.InvokerNativeThreadID = attachmentState.identity.invokerNativeThreadID
	rec.InvokerProvider = attachmentState.identity.invokerProvider
	// presentationMode is mutable under stateMu (the presentation endpoint
	// flips it mid-life), unlike the invoker fields above, so its read locks.
	attachmentState.stateMu.Lock()
	rec.PresentationMode = attachmentState.presentationMode
	attachmentState.stateMu.Unlock()
	attachmentState.rememberAttachment(rec)
}

// recordRestartStep reports progress without owning the restart record.
func (attachmentState *State) recordRestartStep(step observability.RestartStep) {
	if attachmentState.restartProgress != nil {
		attachmentState.restartProgress(step)
	}
}

// recordRestartFailure reports the provider outcome to the restart owner.
func (attachmentState *State) recordRestartFailure(reason string) {
	if attachmentState.restartFailure != nil {
		attachmentState.restartFailure(reason)
	}
}

// holdHandshakeRelease records the slot held by this attachment until catch-up.
func (attachmentState *State) holdHandshakeRelease(release func()) {
	attachmentState.stateMu.Lock()
	attachmentState.releaseHandshakeSlot = release
	attachmentState.stateMu.Unlock()
}

// SetPresentation updates the attachment presentation mode.
func (attachmentState *State) SetPresentation(mode string) {
	attachmentState.stateMu.Lock()
	attachmentState.presentationMode = mode
	attachmentState.stateMu.Unlock()
}

// SetSidebarPlacement updates the attachment sidebar placement.
func (attachmentState *State) SetSidebarPlacement(placement string) {
	attachmentState.stateMu.Lock()
	attachmentState.sidebarPlacement = placement
	attachmentState.stateMu.Unlock()
}

// IsClosed reports whether attachment teardown has begun.
func (attachmentState *State) IsClosed() bool {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	return attachmentState.closed
}

// ReadyAndWorking reports whether the connected attachment is executing a turn.
func (attachmentState *State) ReadyAndWorking() (ready, working bool) {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	ready = attachmentState.lifecycleState.State.Connection == lifecycle.Ready
	return ready, ready && attachmentState.lifecycleState.State.Work == lifecycle.Working
}

// State owns the atomic lifecycle, native identity, turn and replay facts.
type State struct {
	identity         *attachmentIdentity
	wire             *Wire
	delivery         *Delivery
	runtime          *Runtime
	view             *View
	presentationMode string
	sidebarPlacement string
	cwd              string
	// permissionMode is the resolved creation/resume posture retained on the
	// live attachment. Canonical metadata remains the durable authority; this
	// is the immediate fallback used when a provider asks before that read is
	// available.
	permissionMode string
	claude         *claudeSession // claude provider
	grok           *grokSession   // grok provider: stdio proxy to the durable native leader
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

// connectionStatus reads the lifecycle without lending its lock to a caller.
func (attachmentState *State) connectionStatus() string {
	attachmentState.stateMu.Lock()
	status := attachmentState.connectionStatusLocked()
	attachmentState.stateMu.Unlock()
	return status
}
func (attachmentState *State) deliveryPermission() (string, lifecycle.Permission) {
	attachmentState.stateMu.Lock()
	status, permission := attachmentState.connectionStatusLocked(), attachmentState.lifecyclePermissionLocked()
	attachmentState.stateMu.Unlock()
	return status, permission
}
func (attachmentState *State) maintenanceActive() bool {
	attachmentState.stateMu.Lock()
	active := attachmentState.workActiveLocked() || attachmentState.turnRestoreInProgress
	attachmentState.stateMu.Unlock()
	return active
}
func (attachmentState *State) lifecycleEpoch() uint64 {
	attachmentState.stateMu.Lock()
	epoch := attachmentState.lifecycleState.Epoch
	attachmentState.stateMu.Unlock()
	return epoch
}
func (attachmentState *State) settlementWaiting() map[string]bool {
	attachmentState.stateMu.Lock()
	waiting := maps.Clone(attachmentState.settleWaiting)
	attachmentState.stateMu.Unlock()
	return waiting
}
func (attachmentState *State) turnPermission() lifecycle.Permission {
	attachmentState.stateMu.Lock()
	permission := attachmentState.lifecyclePermissionLocked()
	attachmentState.stateMu.Unlock()
	return permission
}
func (attachmentState *State) lifecycleSnapshot() lifecycle.Snapshot {
	attachmentState.stateMu.Lock()
	initial := attachmentState.lifecycleState
	attachmentState.stateMu.Unlock()
	return initial
}
func (attachmentState *State) markReconnectFailed(message string) (closed, changed bool) {
	attachmentState.stateMu.Lock()
	closed = attachmentState.closed || attachmentState.connectionStatusLocked() == StatusExited
	if !closed {
		changed = true
		attachmentState.transitionLifecycleLocked(lifecycle.EventNativeMissing)
		attachmentState.spawnError = message
	}
	attachmentState.stateMu.Unlock()
	return closed, changed
}
func (attachmentState *State) beginClose(requireReady bool) (bool, error) {
	attachmentState.stateMu.Lock()
	if attachmentState.closed {
		attachmentState.stateMu.Unlock()
		return true, nil
	}
	if requireReady && attachmentState.identity.provider == "claude" && attachmentState.claude == nil {
		attachmentState.stateMu.Unlock()
		return false, errors.New("Claude transport is not ready; stop unconfirmed, retry when attachment settles")
	}
	attachmentState.closed = true
	attachmentState.closeUnconfirmed = attachmentState.identity.provider == "claude" && attachmentState.claude != nil
	attachmentState.durablyTerminalTurnID = ""
	attachmentState.durablyTerminalTurnThreadID = ""
	attachmentState.stateMu.Unlock()
	return false, nil
}
func (attachmentState *State) repairResultStale(cached TranscriptRepairResponse) bool {
	attachmentState.stateMu.Lock()
	stale := attachmentState.threadID != cached.threadID || attachmentState.attachmentGeneration != cached.attachmentGen
	attachmentState.stateMu.Unlock()
	return stale
}
func (attachmentState *State) repairIdentity() (string, int64) {
	attachmentState.stateMu.Lock()
	threadID := attachmentState.threadID
	attachmentGeneration := attachmentState.attachmentGeneration
	attachmentState.stateMu.Unlock()
	return threadID, attachmentGeneration
}
func (attachmentState *State) repairability(threadID string, attachmentGeneration int64) string {
	attachmentState.stateMu.Lock()
	detail := attachmentState.repairableLocked(threadID, attachmentGeneration)
	attachmentState.stateMu.Unlock()
	return detail
}

func (attachmentState *State) catalogAvailable() bool { return attachmentState.catalogSources != nil }
func (attachmentState *State) awaitAttachedCurrent(id string, freshness bool, progress func() bool, done <-chan struct{}) bool {
	return attachmentState.catalogSources.AwaitAttachedCurrent(id, freshness, progress, done)
}
func (attachmentState *State) savedTurnReader() func(context.Context, string) (liveview.DurableTurnEvidence, bool) {
	return attachmentState.savedTurn
}

func (attachmentState *State) exited() bool {
	attachmentState.stateMu.Lock()
	exited := attachmentState.connectionStatusLocked() == StatusExited
	attachmentState.stateMu.Unlock()
	return exited
}
func (attachmentState *State) closedOrExited() bool {
	attachmentState.stateMu.Lock()
	closed := attachmentState.closed || attachmentState.connectionStatusLocked() == StatusExited
	attachmentState.stateMu.Unlock()
	return closed
}
func (attachmentState *State) setNativeThreadID(id string) {
	attachmentState.stateMu.Lock()
	attachmentState.threadID = id
	attachmentState.stateMu.Unlock()
}
func (attachmentState *State) adoptNativeThreadID(id string) {
	attachmentState.stateMu.Lock()
	if attachmentState.threadID == "" {
		attachmentState.threadID = id
	}
	attachmentState.stateMu.Unlock()
}
func (attachmentState *State) reconnectCandidate() (bool, bool, string) {
	attachmentState.stateMu.Lock()
	closed := attachmentState.closed || attachmentState.connectionStatusLocked() == StatusExited
	eligible := attachmentState.connectionStatusLocked() == "ready" || attachmentState.connectionStatusLocked() == "reconnecting" || attachmentState.connectionStatusLocked() == "reconnect_parked" || attachmentState.connectionStatusLocked() == "reconnect_failed"
	threadID := attachmentState.threadID
	attachmentState.stateMu.Unlock()
	return closed, eligible, threadID
}
func (attachmentState *State) recoveryRequested() bool {
	attachmentState.stateMu.Lock()
	eligible := !attachmentState.closed && (attachmentState.connectionStatusLocked() == "reconnecting" || attachmentState.connectionStatusLocked() == "reconnect_parked") && attachmentState.threadID != ""
	attachmentState.stateMu.Unlock()
	return eligible
}
func (attachmentState *State) reconnectParked() bool {
	attachmentState.stateMu.Lock()
	parked := attachmentState.connectionStatusLocked() == "reconnect_parked"
	attachmentState.stateMu.Unlock()
	return parked
}
func (attachmentState *State) reconnectModel() (string, bool) {
	attachmentState.stateMu.Lock()
	model := attachmentState.model
	closed := attachmentState.closed || attachmentState.connectionStatusLocked() == StatusExited
	attachmentState.stateMu.Unlock()
	return model, closed
}
func (attachmentState *State) recoveryRetainsFailure() bool {
	attachmentState.stateMu.Lock()
	recovery := !attachmentState.closed && (attachmentState.connectionStatusLocked() == "reconnecting" || attachmentState.connectionStatusLocked() == "ready")
	attachmentState.stateMu.Unlock()
	return recovery
}
func (attachmentState *State) captureCodexIdentity(nativeID, model, cwd, nativeCwd string) bool {
	attachmentState.stateMu.Lock()
	if attachmentState.connectionStatusLocked() == "exited" {
		attachmentState.stateMu.Unlock()
		return false
	}
	attachmentState.threadID = nativeID
	attachmentState.model = model
	if cwd != "" {
		attachmentState.cwd = cwd
	} else if nativeCwd != "" {
		attachmentState.cwd = nativeCwd
	}
	attachmentState.stateMu.Unlock()
	return true
}
func (attachmentState *State) prepareResumeReady() bool {
	attachmentState.stateMu.Lock()
	if attachmentState.closed || attachmentState.connectionStatusLocked() == StatusExited {
		attachmentState.stateMu.Unlock()
		return false
	}
	attachmentState.spawnError = ""
	attachmentState.stateMu.Unlock()
	return true
}
func (attachmentState *State) claudeReadyEvidence(exited <-chan struct{}) (uint64, uint64, bool, string, int64, bool) {
	attachmentState.stateMu.Lock()
	epoch, sequence := attachmentState.lifecycleState.Epoch, attachmentState.nextEvidenceSequenceLocked()
	active, turnID, anchor := attachmentState.turnRestoreActive, attachmentState.turnRestoreID, attachmentState.turnRestoreStartedAtMs
	ended := false
	select {
	case <-exited:
		ended = true
	default:
	}
	attachmentState.stateMu.Unlock()
	return epoch, sequence, active, turnID, anchor, ended
}
func (attachmentState *State) restoreEvidence() (uint64, uint64, bool, string, int64) {
	attachmentState.stateMu.Lock()
	epoch, sequence := attachmentState.lifecycleState.Epoch, attachmentState.nextEvidenceSequenceLocked()
	active, turnID, anchor := attachmentState.turnRestoreActive, attachmentState.turnRestoreID, attachmentState.turnRestoreStartedAtMs
	attachmentState.stateMu.Unlock()
	return epoch, sequence, active, turnID, anchor
}
func (attachmentState *State) publishGrokHandle(grok *grokSession, req ProviderRequest) bool {
	attachmentState.stateMu.Lock()
	if attachmentState.closed {
		attachmentState.stateMu.Unlock()
		return false
	}
	attachmentState.grok = grok
	if req.Mode == "resume" {
		attachmentState.threadID = req.ThreadID
		attachmentState.model = req.GrokResumeModel
	}
	attachmentState.stateMu.Unlock()
	return true
}
func (attachmentState *State) publishGrokIdentity(threadID, model string) bool {
	attachmentState.stateMu.Lock()
	if attachmentState.closed {
		attachmentState.stateMu.Unlock()
		return false
	}
	attachmentState.threadID = threadID
	attachmentState.model = model
	attachmentState.turnModel = ""
	attachmentState.stateMu.Unlock()
	return true
}
