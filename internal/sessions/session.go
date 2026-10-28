package sessions

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	coresession "github.com/dkroshow/nano/internal/core/session"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/inspection"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/observability"
	"github.com/dkroshow/nano/internal/providers"
	"github.com/dkroshow/nano/internal/setup"
)

// Session owns one live provider attachment and its ordered runtime state.
type Session struct {
	id                    string
	provider              string
	hostedThreadID        string
	invokerHostedThreadID string
	invokerNativeThreadID string
	invokerProvider       string
	presentationMode      string
	sidebarPlacement      string
	cwd                   string
	// permissionMode is the resolved creation/resume posture retained on the
	// live attachment. Canonical metadata remains the durable authority; this
	// is the immediate fallback used when a provider asks before that read is
	// available.
	permissionMode string
	createdAt      time.Time
	// processStarted is the immutable application start supplied at construction.
	processStarted time.Time
	// spawnMode is how this session came into being — "start" (a thread that
	// did not exist until now) or "resume" (attaching to one that does). Written
	// once at spawn and never mutated, because it describes the request, not the
	// state: it rides the summary so a surface showing "starting" can say which
	// kind of starting it is (creating vs attaching) instead of guessing from an
	// absent thread id.
	spawnMode string

	codex            *CodexDaemon   // codex provider: shared daemon client
	claude           *claudeSession // claude provider
	grok             *grokSession   // grok provider: stdio proxy to the durable native leader
	reasoningSummary string         // codex: turn/start summary level
	projection       Projection     // normalized live transcript, one per session
	completedTurns   func(context.Context) (*coreturns.Store, error)
	// savedTurn reads the saved status of one turn by exact native turn id,
	// to settle a live turn whose terminal event was lost.
	savedTurn    func(ctx context.Context, turnID string) (durableTurnEvidence, bool)
	savedPrompts SavedPromptReader
	// savedPromptsIn reads saved user rows in a named native thread, for a
	// Claude prompt whose exact native is not the current one; savedTurnFinal
	// reads a decided saved turn's own final answer from the catalog.
	savedPromptsIn savedPromptNativeReader
	savedTurnFinal savedTurnFinalReader
	// owedHint says this hosted thread may have Claude prompts that owe their
	// member completion: set when settlement records one or an owed read finds
	// one, cleared only when a member step reads none. It only gates the pass;
	// the owed rows are the fact.
	owedHint atomic.Bool
	// memberDirty records a member step a trigger asked for while settleMu
	// was held (requestMemberStep, releaseSettle).
	memberDirty atomic.Bool
	// memberCursor is the delivery sequence the next member step starts its
	// owed read from. Guarded by settleMu.
	memberCursor uint64
	// settleWaiting names the delivered prompts a later saved commit may
	// settle: each one this attachment delivered, and each whose saved turn
	// was still open when settlement last ran. While it is empty, a saved
	// commit reads nothing to settle prompts. Guarded by stateMu.
	settleWaiting map[string]bool
	// stoppedTurns names the native turns the operator stopped. A prompt such
	// a turn ended without taking is held for the person, never sent again by
	// itself: Stop means stop. Guarded by stateMu.
	stoppedTurns map[string]bool
	// discardedUnknown names the actions the person discarded before Nano had
	// them, so an attempt still on its way is refused. Guarded by sendMu.
	discardedUnknown map[string]bool
	// settleMu runs one settlement pass at a time, so an older pass never
	// restores a prompt a newer one settled.
	settleMu sync.Mutex
	// releaseHandshakeSlot gives back the boot handshake slot this session's
	// provider attach holds (boot_reattach.go); nil when it holds none.
	// savedCaughtUp: an attach of this session has waited for its saved
	// catch-up, so a later one needs a fresh pass. Both stateMu.
	releaseHandshakeSlot func()
	savedCaughtUp        bool
	// transcriptRepairMu serializes the operator's stop/discard/rebuild action
	// and retains its last idempotent response until the attachment changes.
	transcriptRepairMu      sync.Mutex
	transcriptRepairResults map[string]TranscriptRepairResponse
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
	rememberAttachment func(rec attachmentRecord)
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
	nameThread  func(provider, nativeThreadID, name string)
	// publishCreationName refreshes the derived name projection from the
	// canonical creation fact without writing it into the manual names file.
	publishCreationName func(provider, nativeThreadID, name string)
	// creationName reloads the immutable creation request from the canonical
	// threads store. Every bind site consults it, so a restart cannot lose the
	// name merely because the first in-memory pendingName was consumed.
	creationName func() string
	// metadataName reads the names-store overlay with its provenance. It is
	// separate from displayName, which is only the immediate pre-catalog UI
	// value while a new native id has not yet been indexed.
	metadataName func(provider, nativeThreadID string) (hostedMetadataName, bool)

	// Submitted prompts are idempotent operations keyed by the browser's
	// clientActionId. sendAttempts carries their small hash-only journal across
	// Nano restarts; sendOps lets duplicate requests join an in-flight send.
	sendAttempts    Journal
	queuedTurns     Queue
	turns           *prompts.Store
	core            *coresession.Session
	coreMu          sync.RWMutex
	coreOnce        sync.Once
	coreErr         error
	providerAdapter providers.Adapter
	agentExecutions Executions
	metadata        Metadata
	appendUnread    func(string, string) string
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
	sendMu               sync.Mutex
	sendOps              map[string]*turnSendOperation
	// nanoStopping is the server's stop flag. Once it is set, no delivery
	// starts; see claimAndStartTransactionalTurnLocked.
	nanoStopping *atomic.Bool
	// deliveryExpiries is the server's count of recent delivery deadline
	// expiries per provider, shown in this session's runtime status. Nil-safe.
	deliveryExpiries *DeliveryExpiries
	// lastHumanPromptAt is the newest browser-path (human-origin) prompt on
	// this attachment, a hard availability fact. Durable copy:
	// hosted_threads.last_human_prompt_at via agentExecutions.noteHumanPrompt.
	lastHumanPromptAt time.Time
	turnSender        func(context.Context, string) (json.RawMessage, error) // test seam
	// A daemon reconnect may begin while the previous thread handshake is still
	// unwinding its response. Serialize the whole bind transaction per session so
	// its identity, attachment record, and ready transition cannot interleave.
	codexHandshakeMu sync.Mutex

	mu     sync.Mutex
	events []wireEvent
	seq    int64
	subs   map[chan wireEvent]struct{}
	spool  *wireSpool

	// mcp caches the projected MCP tool registry served by
	// /api/sessions/{id}/mcp, invalidated from the wire (mcp_inventory.go).
	mcp inspection.MCPCache
	// skills is shared across every session because app-server scopes its
	// skills/list answer to cwd, not to one provider thread.
	skills *setup.SkillsListCache

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
	closeMu          sync.Mutex // serialize teardown and its cached outcome
	closeErr         error      // guarded by closeMu
	closeUnconfirmed bool       // stateMu: suppress settlement until stop acknowledgement
	spawnError       string     // background handshake failure; surfaced as "error" (Slice R3)
	tokenUsage       json.RawMessage
	turnTokenUsage   map[string]*tokenUsage
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
	interactions map[string]pendingInteraction
	// Status classifiers reject every provider interaction before it enters the
	// visible pending-card state. rejectedInteractionSeq tells the dispatcher
	// that the otherwise-hidden abnormal attempt must fail closed.
	autoRejectInteractions bool
	rejectedInteractionSeq int64
	state                  map[string]json.RawMessage

	// liveText is set for a provider switched to file-only transcripts.
	liveText LiveText
}

// Summary is the public snapshot of an attachment identity, lifecycle, and provider settings.
type Summary struct {
	ID                    string `json:"id"`
	Provider              string `json:"provider"`
	ThreadID              string `json:"threadId"`
	Name                  string `json:"name,omitempty"`
	Model                 string `json:"model"`
	Cwd                   string `json:"cwd"`
	Status                string `json:"status"`
	HostedThreadID        string `json:"hostedThreadId"`
	InvokerHostedThreadID string `json:"invokerHostedThreadId,omitempty"`
	InvokerNativeThreadID string `json:"invokerNativeThreadId,omitempty"`
	InvokerProvider       string `json:"invokerProvider,omitempty"`
	PresentationMode      string `json:"presentationMode"`
	SidebarPlacement      string `json:"sidebarPlacement"`
	// SpawnMode is "start" or "resume". Both spawn kinds now hand back a
	// "starting" session while their provider handshake runs in the background,
	// and the two mean different things to the operator — one is creating a
	// session, the other is attaching to a thread — so the intent travels with
	// the status rather than being inferred from ThreadID being empty.
	SpawnMode  string `json:"spawnMode,omitempty"`
	Error      string `json:"error,omitempty"` // set only on spawn failure (Slice R3)
	TurnActive bool   `json:"turnActive"`
	// ThreadStatus is the thread's turn state (working, needs_you or idle),
	// the one value the pane and the sidebar render.
	ThreadStatus string `json:"threadStatus"`
	// RunningTurnInput says Send joins a working turn instead of queueing it.
	RunningTurnInput bool `json:"runningTurnInput"`
	// TurnStartedAtMs is the immutable anchor of the turn in flight; absent (0)
	// when there is no turn or no evidence of when it began.
	TurnStartedAtMs int64 `json:"turnStartedAtMs,omitempty"`
	// CompactingSinceMs is when the provider began compacting the turn's
	// context; absent while it is not compacting.
	CompactingSinceMs int64 `json:"compactingSinceMs,omitempty"`
	// Waiting reports a parked interaction: the turn is open but the agent is
	// blocked on the operator, which is not the same as working (D15). Display
	// state layered on an active turn — both providers leave turnActive true
	// while a dialog is parked — so surfaces show waiting INSTEAD of a ticking
	// timer rather than dropping the row's liveness.
	Waiting bool `json:"waiting,omitempty"`
	// NeedsAttention promotes a failed background agent turn into the compact
	// sidebar. It is derived from the durable execution store, not wire text.
	NeedsAttention bool `json:"needsAttention,omitempty"`
	// ThreadDiagnostics is the thread's bounded list of delivery and
	// session facts (thread_diagnostics.go).
	ThreadDiagnostics []threadDiagnostic `json:"threadDiagnostics,omitempty"`
	// LastTurnEndedAt is the server wall-clock instant at which the most recent
	// hosted turn ended. It seeds the client's completed stack on reload.
	LastTurnEndedAt string `json:"lastTurnEndedAt,omitempty"`
	CreatedAt       string `json:"createdAt"`
	// Starred is the operator pin overlay, stamped at list time so a
	// pending sidebar row can join the Starred section before its catalog row.
	Starred bool `json:"starred,omitempty"`
	// Posture and Metadata both come from control.sqlite for hosted threads.
	Posture             sessionPosture      `json:"posture"`
	Metadata            *hostedMetadataView `json:"metadata,omitempty"`
	MetadataUnavailable bool                `json:"metadataUnavailable,omitempty"`
	MetadataDegraded    bool                `json:"metadataDegraded,omitempty"`
	// FileTranscript is always true: every provider serves pane rows and live
	// text through its file transcript stream (live_text.go).
	FileTranscript bool `json:"fileTranscript"`
}

// applyPendingName publishes the highest available not-yet-durable name once
// a native id is known: a staged operator rename first, then the immutable
// creation fallback. The derived projection ranks manual and imported truth
// above creation, so an existing durable name remains authoritative.
func (sess *Session) applyPendingName(nativeThreadID string) {
	if nativeThreadID == "" {
		return // no id yet — keep the name for a later bind site
	}
	sess.stateMu.Lock()
	name := sess.pendingName
	sess.pendingName = ""
	sess.stateMu.Unlock()
	if name != "" {
		if sess.nameThread != nil {
			sess.nameThread(sess.provider, nativeThreadID, name)
		}
		return
	}
	if sess.creationName == nil {
		return
	}
	name = sess.creationName()
	if name == "" {
		return
	}
	if sess.publishCreationName != nil {
		sess.publishCreationName(sess.provider, nativeThreadID, name)
	} else if sess.nameThread != nil {
		// A narrow fallback for legacy embedders without a derived projection.
		sess.nameThread(sess.provider, nativeThreadID, name)
	}
}
