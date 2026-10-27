// session.go — one hosted agent session: thread metadata, the wire buffer
// streamed to the UI over SSE, and the HTTP handlers scoped to that session.
// Codex sessions share one app-server daemon; Claude sessions still own one
// child process each.
package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dkroshow/nano/internal/httpapi"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	coresession "github.com/dkroshow/nano/internal/core/session"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/providers"
)

// wireBufferCap bounds the raw events a session keeps in memory: the tail the
// Event Feed shows on connect. Older events page from the spool, which holds
// every event. At 20,000 the rings held about 500 MB across 450 sessions, 60%
// of the live heap, for a panel that is rarely open.
const wireBufferCap = 500

type wireEvent struct {
	Seq int64           `json:"seq"`
	TS  string          `json:"ts"`
	Dir string          `json:"dir"`
	Msg json.RawMessage `json:"msg"`
}

type queuedCodexDelta struct {
	method   string
	threadID string
	turnID   string
	itemID   string
	delta    string
	at       time.Time
	event    wireEvent
	wireSeq  []int64
}

type queuedCodexDeltaBatch struct {
	first  queuedCodexDelta
	delta  strings.Builder
	frames []queuedCodexDelta
}

func parseQueuedCodexDelta(line []byte) (queuedCodexDelta, bool) {
	var envelope struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &envelope) != nil ||
		(envelope.Method != "item/agentMessage/delta" && envelope.Method != "item/commandExecution/outputDelta") ||
		envelope.Params.ThreadID == "" || envelope.Params.TurnID == "" || envelope.Params.ItemID == "" {
		return queuedCodexDelta{}, false
	}
	return queuedCodexDelta{
		method:   envelope.Method,
		threadID: envelope.Params.ThreadID, turnID: envelope.Params.TurnID,
		itemID: envelope.Params.ItemID, delta: envelope.Params.Delta,
	}, true
}

func (batch *queuedCodexDeltaBatch) append(frame queuedCodexDelta) {
	batch.delta.WriteString(frame.delta)
	batch.frames = append(batch.frames, frame)
}

func (batch *queuedCodexDeltaBatch) mergedLine() ([]byte, error) {
	return json.Marshal(struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		} `json:"params"`
	}{
		Method: batch.first.method,
		Params: struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}{ThreadID: batch.first.threadID, TurnID: batch.first.turnID, ItemID: batch.first.itemID, Delta: batch.delta.String()},
	})
}

// spawnRequest is the POST /api/sessions body: everything the New Window
// dialog can pick. Empty fields fall back to server flag defaults (or the
// provider's own defaults).
type spawnRequest struct {
	Provider              string                   `json:"provider"`
	Mode                  string                   `json:"mode"`                       // start | resume
	ThreadID              string                   `json:"threadId"`                   // resume target (native thread/session id)
	ClientSessionID       string                   `json:"clientSessionId,omitempty"`  // optional idempotent creation alias
	InvokerThreadRef      string                   `json:"invokerThreadRef,omitempty"` // agent API only: caller's own hosted/native/current session reference
	Subscriptions         subscriptions.Choices    `json:"subscriptions,omitempty"`    // agent API only: completion delivery channels on the immutable invoker edge
	Assignment            *orchestrationAssignment `json:"assignment,omitempty"`       // optional caller-authored execution assignment
	HostedThreadID        string                   `json:"-"`                          // server-owned durable execution identity
	InvokerHostedThreadID string                   `json:"-"`                          // normalized durable invocation edge
	InvokerNativeThreadID string                   `json:"-"`                          // provider-native snapshot for display/debugging
	InvokerProvider       string                   `json:"-"`                          // provider snapshot; the edge itself is cross-provider
	PresentationMode      string                   `json:"presentationMode,omitempty"` // shared | background
	SidebarPlacement      string                   `json:"sidebarPlacement,omitempty"` // invoker | standalone
	Cwd                   string                   `json:"cwd"`
	Model                 string                   `json:"model"`
	Effort                string                   `json:"effort"`         // codex + claude + grok reasoning effort
	ServiceTier           string                   `json:"serviceTier"`    // codex: priority / service tier id
	Name                  string                   `json:"name"`           // optional user-assigned thread name, written once the native id binds
	PendingName           string                   `json:"-"`              // boot-only operator rename staged before a real native id bound
	Sandbox               string                   `json:"sandbox"`        // codex
	Approval              string                   `json:"approval"`       // codex
	PermissionMode        string                   `json:"permissionMode"` // claude; grok start: auto | bypassPermissions
	AllowedTools          string                   `json:"allowedTools"`   // claude
	ClaudeBin             string                   `json:"-"`              // resolved server-side only
	ClaudeShimWS          string                   `json:"-"`              // resolved server-side only; empty = direct child
	ClaudeShimSpools      string                   `json:"-"`              // resolved server-side only; the shim's spool directory
	ClaudeArgs            []string                 `json:"-"`              // resolved server-side only; claude resume: the original child's argv
	GrokBin               string                   `json:"-"`              // resolved server-side only
	GrokLeaderSocket      string                   `json:"-"`              // empty uses Grok's short default ~/.grok/leader.sock
	// ClaudeAnchor is the attachment record's live-turn anchor, read at resolve
	// time because the attach replay rewrites the record before anything else
	// could read it. Zero-valued for a start, or for a record that names no
	// turn in flight.
	ClaudeAnchor attachmentRecord `json:"-"`
	// retiredClaudeHandle is the provisional handle whose record an
	// interrupted re-key left beside the native's. Only an actual resume
	// retires it; a preview resolves the same request and must not.
	retiredClaudeHandle string `json:"-"`
	// codexZeroTurnResume is boot-only: a supervised daemon can still hold a
	// thread that has not taken its first turn, but thread/resume cannot load it
	// because Codex writes no rollout until that turn. codexResumeModel restores
	// the attachment's display posture without sending it as a resume override.
	codexZeroTurnResume bool   `json:"-"`
	codexResumeModel    string `json:"-"`
	grokResumeModel     string `json:"-"`
	// bootReattach makes provider selection asynchronous after the session shell
	// is registered. Boot has no HTTP caller waiting for a synchronous resume
	// refusal, and one slow daemon must not hide every other remembered session.
	bootReattach *bootProviderControl `json:"-"`
	// Prioritized registrations must appear immediately even during fleet boot.
	immediateRuntimePublish bool `json:"-"`
	// restart is this thread's entry in the boot's restart record, if any.
	restart *restartThread `json:"-"`
	// catalogBackground queues the catalog holds of a thread the sidebar did
	// not show at boot behind foreground ones, for a bounded time
	// (catalog_attached.go).
	catalogBackground bool `json:"-"`
	// apiOrigin is this server's own HTTP origin. Starts use it to mint the
	// provider identity block; Claude fallback resumes use it to rebuild or
	// refresh that same immutable block when transport argv is absent or stale.
	apiOrigin string `json:"-"`
	// externalResume is set only by read-only native evidence verification when
	// this provider conversation has never had a Nano hosted record. Metadata is
	// retained separately because provider resume builders intentionally blank
	// settings that must not be sent as start overrides.
	externalResume         bool              `json:"-"`
	externalResumePosture  bool              `json:"-"`
	externalResumeCatalog  string            `json:"-"`
	externalResumeMetadata map[string]string `json:"-"`

	// Advanced codex thread/start params (docs/thread-start-parameters §1/§7).
	Personality           string          `json:"personality"`           // none | friendly | pragmatic
	ApprovalsReviewer     string          `json:"approvalsReviewer"`     // user | auto_review
	Ephemeral             bool            `json:"ephemeral"`             // true → no rollout on disk, no resume
	BaseInstructions      string          `json:"baseInstructions"`      // replaces built-in system-prompt layer
	DeveloperInstructions string          `json:"developerInstructions"` // injected at developer role
	ModelProvider         string          `json:"modelProvider"`         // key into config [model_providers]
	Config                json.RawMessage `json:"config"`                // per-thread config.toml overrides (object)
	ThreadSource          string          `json:"threadSource"`          // client analytics classification
	SessionStartSource    string          `json:"sessionStartSource"`    // startup | clear
	ServiceName           string          `json:"serviceName"`           // analytics service name
}

// CreateHostedThreadRequest is the agent-facing creation shape. It shares the
// server's spawn decoder because hosted-thread creation is one constrained
// form of session creation.
type CreateHostedThreadRequest = spawnRequest

var errHostedControlUnavailable = errors.New("hosted control unavailable; new hosted sessions and resumes are disabled")

type session struct {
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
	// spawnMode is how this session came into being — "start" (a thread that
	// did not exist until now) or "resume" (attaching to one that does). Written
	// once at spawn and never mutated, because it describes the request, not the
	// state: it rides the summary so a surface showing "starting" can say which
	// kind of starting it is (creating vs attaching) instead of guessing from an
	// absent thread id.
	spawnMode string

	codex            *codexDaemon    // codex provider: shared daemon client
	claude           *claudeSession  // claude provider
	grok             *grokSession    // grok provider: stdio proxy to the durable native leader
	reasoningSummary string          // codex: turn/start summary level
	projection       *liveProjection // normalized live transcript, one per session
	completedTurns   *completedTurnSource
	// savedTurn reads the saved status of one turn by exact native turn id,
	// to settle a live turn whose terminal event was lost.
	savedTurn    func(ctx context.Context, turnID string) (durableTurnEvidence, bool)
	savedPrompts savedPromptReader
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
	transcriptRepairResults map[string]transcriptRepairResponse
	// catalogRefresh hints the catalog that a known native file may have
	// changed (catalog_nudge.go); an empty path is ignored. Nil when the
	// catalog is unavailable; set once before wire activity, never mutated.
	catalogRefresh func(string)
	// catalogSources holds this attachment's native files in the catalog's
	// watch set from registration until close (catalog_attached.go). Set at
	// registration, so an attachment that never registers holds nothing.
	catalogSources attachedSourceCatalog
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
	sendAttempts    *sendAttemptStore
	queuedTurns     *queuedTurnStore
	turns           *prompts.Store
	core            *coresession.Session
	coreMu          sync.RWMutex
	coreOnce        sync.Once
	coreErr         error
	providerAdapter providers.Adapter
	agentExecutions *agentExecutionStore
	metadata        *controlMetadataStore
	notices         *interact.Notices
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
	restart              *restartThread // set while a restart record tracks it; nil-safe
	catalogBackground    bool           // hidden at boot: its catalog holds wait behind foreground ones, bounded
	sendMu               sync.Mutex
	sendOps              map[string]*turnSendOperation
	// nanoStopping is the server's stop flag. Once it is set, no delivery
	// starts; see claimAndStartTransactionalTurnLocked.
	nanoStopping *atomic.Bool
	// deliveryExpiries is the server's count of recent delivery deadline
	// expiries per provider, shown in this session's runtime status. Nil-safe.
	deliveryExpiries *deliveryExpiries
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
	mcp mcpInventoryCache
	// skills is shared across every session because app-server scopes its
	// skills/list answer to cwd, not to one provider thread.
	skills *skillsListCache

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
	liveText *liveTextLoop
}

type sessionSummary struct {
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

// detachedSessionView keeps the legacy attachment shape required to locate a
// resumable provider transport, while exposing hosted facts only through the
// listed metadata live sessions carry; the full view is a thread's own read.
// The embedded record is deliberately scrubbed at response construction; old
// JSON cwd/model/tier values are migration input, not API truth.
type detachedSessionView struct {
	attachmentRecord
	// Status is "starting" while the boot loop has not registered the thread.
	Status              string          `json:"status,omitempty"`
	Metadata            *listedMetadata `json:"metadata,omitempty"`
	MetadataUnavailable bool            `json:"metadataUnavailable,omitempty"`
}

// applyPendingName publishes the highest available not-yet-durable name once
// a native id is known: a staged operator rename first, then the immutable
// creation fallback. The derived projection ranks manual and imported truth
// above creation, so an existing durable name remains authoritative.
func (sess *session) applyPendingName(nativeThreadID string) {
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

func newSessionID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// resolveSpawn fills flag defaults and absolutizes cwd — the single place
// spawn parameters get resolved, shared by spawnSession and the preview
// endpoint so the previewed JSON matches what a real spawn would send. Resume
// intentionally avoids applying start defaults for either provider; otherwise
// nano would silently override the posture the resumed session already has.
func (s *server) resolveSpawn(req spawnRequest) (spawnRequest, error) {
	req.Provider = strings.TrimSpace(req.Provider)
	if req.Mode == "" {
		req.Mode = "start"
	}
	presentationMode, err := normalizePresentationMode(req.PresentationMode)
	if err != nil {
		return req, err
	}
	req.PresentationMode = presentationMode
	sidebarPlacement, err := normalizeSidebarPlacement(req.SidebarPlacement)
	if err != nil {
		return req, err
	}
	req.SidebarPlacement = sidebarPlacement
	switch req.Mode {
	case "start", "resume":
	default:
		return req, fmt.Errorf("unknown spawn mode %q", req.Mode)
	}
	if req.Mode == "resume" {
		// A Claude fallback may have to rebuild the complete child command from
		// canonical hosted state alone. The immutable identity block is part of
		// that transport command, so the resume resolver needs the same server
		// origin a fresh start receives below.
		req.apiOrigin = s.defaults.apiOrigin
		req.ThreadID = strings.TrimSpace(req.ThreadID)
		if req.ThreadID == "" {
			return req, fmt.Errorf("threadId is required to resume a thread")
		}
		// Resume routes only through the canonical current native owner. Neither
		// attachment records nor historical activity may mint or reclaim one.
		if err := s.resolveResumeHostedIdentity(&req); err != nil {
			return req, err
		}
		// A hosted attachment is only a transport locator.  Its old cwd/model/
		// tier fields are migration input, never resume authority: once a durable
		// hosted identity exists, the canonical control row supplies every
		// provider setting that can be replayed.
		if req.HostedThreadID != "" {
			if err := s.inheritResumePosture(req.HostedThreadID, req.Provider, req.ThreadID); err != nil {
				return req, err
			}
			if err := s.applyCanonicalResumeMetadata(&req); err != nil {
				return req, err
			}
		}
		switch req.Provider {
		case "codex":
			return s.resolveCodexResume(req)
		case "claude":
			return s.resolveClaudeResume(req)
		case "grok":
			return s.resolveGrokResume(req)
		}
		return req, fmt.Errorf("resume is not supported for provider %q", req.Provider)
	}

	// The app-server's thread/start rejects anything outside this pair as an
	// unknown enum variant — a caller reaching for a free-text origin tag
	// wants threadSource, not this field. Catching it here means a bad value
	// fails the request instead of minting a session that can never start
	// (observed 2026-08-06: five background workers stuck exited, each
	// carrying rpc error -32600 unknown variant `nano`).
	if req.SessionStartSource != "" && req.SessionStartSource != "startup" && req.SessionStartSource != "clear" {
		return req, fmt.Errorf("sessionStartSource must be %q or %q, got %q", "startup", "clear", req.SessionStartSource)
	}

	req.apiOrigin = s.defaults.apiOrigin
	if req.Cwd == "" {
		req.Cwd = s.defaults.cwd
	}
	absCwd, err := filepath.Abs(req.Cwd)
	if err != nil {
		return req, err
	}
	req.Cwd = absCwd
	if req.Model == "" && req.Provider != "grok" {
		req.Model = s.defaults.model
	}
	switch req.Provider {
	case "claude":
		req.ClaudeBin = s.defaults.claudeBin
		req.ClaudeShimWS = s.defaults.claudeShimWS
		if req.AllowedTools == "" && req.PermissionMode != "bypassPermissions" {
			req.AllowedTools = s.defaults.allowedTools
		}
	case "codex":
		if req.Sandbox == "" {
			req.Sandbox = s.defaults.sandbox
		}
		if req.Approval == "" {
			req.Approval = s.defaults.approval
		}
		// Standard is Nano's deliberate default. A raw app-server omission can
		// inherit an operator config that is already Priority, which makes an
		// unspecified worker's speed posture depend on ambient state. Priority
		// is opt-in: callers that want it must send serviceTier=priority.
		if req.ServiceTier == "" {
			req.ServiceTier = "default"
		}
		if err := s.codex.validateUltrafast(req.Model, req.ServiceTier); err != nil {
			return req, err
		}
	case "grok":
		req.GrokBin = s.defaults.grokBin
		req.GrokLeaderSocket = s.defaults.grokLeaderSocket
		switch req.PermissionMode {
		case "", "auto", "bypassPermissions":
		default:
			return req, fmt.Errorf("Grok permissionMode must be %q or %q", "auto", "bypassPermissions")
		}
	default:
		return req, fmt.Errorf("unknown provider %q", req.Provider)
	}
	return req, nil
}

// applyCanonicalResumeMetadata overlays the recoverable provider settings for
// an already-hosted thread.  The JSON attachment remains useful for transport
// handles (Claude argv, shim ids and replay anchors), but it must never be
// consulted for model, effort, permission, service tier or cwd once a hosted
// identity has been established.
func (s *server) applyCanonicalResumeMetadata(req *spawnRequest) error {
	if req == nil || req.HostedThreadID == "" {
		return nil
	}
	if s.controlMetadata == nil || s.controlMetadataErr != "" {
		return errHostedControlUnavailable
	}
	view, err := s.controlMetadata.view(req.HostedThreadID)
	if err != nil {
		return fmt.Errorf("read canonical metadata for hosted thread %s: %w", req.HostedThreadID, err)
	}
	externalPosture, err := s.controlMetadata.wasExternalResumeRegistration(req.HostedThreadID)
	if err != nil {
		return fmt.Errorf("read external Resume origin for hosted thread %s: %w", req.HostedThreadID, err)
	}
	req.externalResumePosture = externalPosture
	// Do not let an API caller's resume body or a retired attachment value fill
	// a missing canonical fact. Resume deliberately uses desired intent rather
	// than the display projection: an imported historical creation/operator
	// request remains what a new attachment should retry, even when the UI
	// displays a later observed provider value. Unknown is explicit state, not
	// permission to inherit today's server defaults.
	req.Cwd, req.Model, req.Effort, req.PermissionMode, req.ServiceTier = "", "", "", "", ""
	for _, field := range []struct {
		name string
		set  func(string)
	}{
		{postureFieldModel, func(value string) { req.Model = value }},
		{postureFieldEffort, func(value string) { req.Effort = value }},
		{postureFieldPermissionMode, func(value string) { req.PermissionMode = value }},
		{postureFieldServiceTier, func(value string) { req.ServiceTier = value }},
		{"cwd", func(value string) { req.Cwd = value }},
	} {
		if value, ok := canonicalResumeString(view.Fields[field.name]); ok {
			field.set(value)
		}
	}
	return nil
}

// canonicalMetadataString consumes the server-authored display projection.
// It deliberately returns no value for unknown rather than choosing a process
// default. Resume uses canonicalResumeString instead because it intentionally
// preserves historical desired intent.
func canonicalMetadataString(current metadataCurrentView) (string, bool) {
	var value string
	if len(current.DisplayValue) == 0 || json.Unmarshal(current.DisplayValue, &value) != nil || value == "" {
		return "", false
	}
	return value, true
}

// canonicalResumeString preserves an explicit operator/creation choice across
// a new attachment, including a provider-confirmed or rejected attempt. A
// cleared choice deliberately sends no override. Only a field with no desired
// event falls back to the provider's effective value.
func canonicalResumeString(current metadataCurrentView) (string, bool) {
	encoded := current.EffectiveValue
	if current.DesiredEventID != nil {
		if current.DesiredDisposition == metadataDispositionCleared {
			return "", false
		}
		encoded = current.DesiredValue
	}
	var value string
	if len(encoded) == 0 || json.Unmarshal(encoded, &value) != nil || value == "" {
		return "", false
	}
	return value, true
}

// resolveGrokResume restores only the attachment's identity and cwd. The
// native leader owns model and permission posture; session/load is the source
// of truth and Nano does not layer current start defaults over it.
func (s *server) resolveGrokResume(req spawnRequest) (spawnRequest, error) {
	cwd := strings.TrimSpace(req.Cwd)
	if cwd == "" {
		return req, fmt.Errorf("no attachment record for Grok session %s and no cwd supplied", req.ThreadID)
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return req, err
	}
	resumeModel := req.Model
	resumePermissionMode := req.PermissionMode
	req.Cwd = absCwd
	req.Model = ""
	req.Effort = ""
	// session/load, not Nano, restores Grok's native permission posture. Keep
	// the canonical value on the local attachment so Nano can still enforce
	// and audit a thread-scoped bypass request after reconnecting.
	req.PermissionMode = resumePermissionMode
	req.GrokBin = s.defaults.grokBin
	req.GrokLeaderSocket = s.defaults.grokLeaderSocket
	// For a hosted thread this is the canonical desired/effective model loaded
	// by applyCanonicalResumeMetadata.  A non-hosted historical attachment may
	// still supply an explicit request value, but it cannot become authority for
	// a hosted session.
	req.grokResumeModel = resumeModel
	return req, nil
}

// resolveCodexResume keeps a codex resume free of start defaults: the
// app-server already holds the thread's posture, so every start-only field is
// blanked rather than re-sent. The service tier is the exception: Codex resumes
// at its own config's tier, so every resume handshake sends the thread's.
func (s *server) resolveCodexResume(req spawnRequest) (spawnRequest, error) {
	if req.Cwd == "" {
		if req.HostedThreadID != "" {
			return req, fmt.Errorf("canonical cwd is unknown for hosted Codex thread %s", req.HostedThreadID)
		}
		req.Cwd = s.defaults.cwd
	}
	absCwd, err := filepath.Abs(req.Cwd)
	if err != nil {
		return req, err
	}
	req.Cwd = absCwd
	req.Model = ""
	req.Effort = ""
	req.ServiceTier = ""
	req.Sandbox = ""
	req.Approval = ""
	req.Personality = ""
	req.ApprovalsReviewer = ""
	req.Ephemeral = false
	req.BaseInstructions = ""
	req.DeveloperInstructions = ""
	req.ModelProvider = ""
	req.Config = nil
	req.ThreadSource = ""
	req.SessionStartSource = ""
	req.ServiceName = ""
	return req, nil
}

// resolveClaudeResume resolves a claude resume from evidence, not defaults. The
// attachment record left behind by the session being resumed carries recovery
// handles (binary + argv). Cwd and provider settings for hosted threads come
// only from canonical control.sqlite metadata.
//
// Without an attachment, the canonical hosted owner still supplies the cwd
// and posture before this provider builder runs. A missing transport record
// never authorizes public resume to mint a new owner or guess its settings.
func (s *server) resolveClaudeResume(req spawnRequest) (spawnRequest, error) {
	req.ClaudeShimWS = s.defaults.claudeShimWS
	req.ClaudeShimSpools = s.defaults.claudeShimSpools
	if nativeThreadIDProvisional("claude", req.ThreadID) && req.HostedThreadID != "" {
		// An interrupted re-key can leave the handle's record beside the one it
		// wrote for the resolved native. The native's record is the current
		// one, with the anchor of any later turn: resume it. spawnSession
		// retires the handle's record the re-key did not get to forget.
		native, err := s.controlStore().resolvedNative(context.Background(), req.HostedThreadID, "claude", req.ThreadID)
		if err != nil {
			return req, err
		}
		if current, ok := s.attachmentFor("claude", native); native != "" && ok && current.HostedThreadID == req.HostedThreadID {
			req.retiredClaudeHandle, req.ThreadID = req.ThreadID, native
		}
	}
	rec, _ := s.attachmentFor("claude", req.ThreadID)
	cwd := strings.TrimSpace(req.Cwd)
	if cwd == "" {
		if req.HostedThreadID != "" {
			return req, fmt.Errorf("canonical cwd is unknown for hosted Claude thread %s", req.HostedThreadID)
		}
		return req, fmt.Errorf("no attachment record for thread %s and no cwd supplied", req.ThreadID)
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return req, err
	}
	// The cwd must exist for the `--resume` respawn, but not for the shim-attach
	// rung above it, which needs no cwd at all and is the only rung that keeps a
	// mid-flight turn alive. The check therefore lives on that rung
	// (requireResumeCwd, claude.go), not here.
	req.Cwd = absCwd
	req.ClaudeBin = rec.Bin
	req.ClaudeArgs = rec.Args
	req.ClaudeAnchor = rec
	if req.ClaudeBin == "" {
		req.ClaudeBin = resolveClaudeBin(s.defaults.claudeBin)
	}
	if len(req.ClaudeArgs) == 0 {
		// No stored transport command exists, so rebuild one from the request's
		// recorded fields. Existing hosted records retain the legacy allowlist
		// fallback. A first registration must keep an unknown provider allowlist
		// unknown instead of applying a new-session default.
		if !req.externalResumePosture && req.AllowedTools == "" && req.PermissionMode != "bypassPermissions" {
			req.AllowedTools = s.defaults.allowedTools
		}
		req.ClaudeArgs = claudeArgs(req)
	}
	// A stored command supplies the stream transport baseline. A record-less
	// historical resume rebuilds that baseline above. Only after either source
	// exists may canonical hosted posture and identity replace stale values.
	if req.HostedThreadID != "" {
		req.ClaudeArgs = claudeCanonicalResumeArgs(req.ClaudeArgs, req)
	}
	return req, nil
}

// liveSessionForResume finds a live session that already hosts the thread a
// resume names, by native id or, before Codex learns its native id, by
// hosted id. Exited sessions do not count: the operator may resume those.
func (s *server) liveSessionForResume(provider, nativeThreadID, hostedThreadID string) *session {
	if existing := s.sessionForThread(provider, nativeThreadID); existing != nil {
		return existing
	}
	existing := s.sessionForHostedThread(hostedThreadID)
	if existing == nil || hostedThreadID == "" {
		return nil
	}
	existing.stateMu.Lock()
	exited := existing.connectionStatusLocked() == sessionStatusExited
	existing.stateMu.Unlock()
	if exited {
		return nil
	}
	return existing
}

// spawnSession registers a session shell and hands it back as soon as the
// request itself is known to be good — the provider handshake runs behind it.
//
// Validation stays synchronous, so a bad cwd or an already-attached thread is
// still the POST's own answer. The handshake is not: a slow app-server made the
// New Window dialog sit on "Creating…" for up to a minute
// (codexHandshakeTimeout), which the operator reads as a hung app. Both spawn
// kinds now follow the contract resume has had since Slice R3 — status
// "starting", the card visible immediately, failure surfaced through failSpawn
// on the session rather than as an HTTP error nobody is still looking at.
func (s *server) spawnSession(req spawnRequest) (*session, error) {
	if s.agentExecutions == nil || s.controlMetadata == nil || s.executionControlErr != "" || s.controlMetadataErr != "" {
		return nil, errHostedControlUnavailable
	}
	if req.Mode == "resume" {
		// Whichever path registers a still-starting thread (the boot loop, a
		// request, a manual resume) is tracked from here, lock wait included.
		if key := (attachmentKey{provider: req.Provider, nativeThreadID: req.ThreadID}); req.restart == nil && s.boot.waiting(key) {
			req.restart = s.restart.thread(key)
			req.restart.at(restartPicked)
		}
		lock := s.resumeSpawnLock(req.Provider)
		lock.Lock()
		defer lock.Unlock()
		req.restart.at(restartLock)
	}
	req, err := s.resolveSpawn(req)
	if err != nil {
		return nil, err
	}
	if req.externalResume {
		record, err := s.agentExecutions.ensureExternalResumeSession(req)
		if err != nil {
			return nil, err
		}
		failRegistration := func(cause error) error {
			if updateErr := s.agentExecutions.updateSession(record.ID, "", "", sessionStatusExited, cause.Error()); updateErr != nil {
				return fmt.Errorf("%w (also failed to mark hosted thread %s retryable: %v)", cause, record.ID, updateErr)
			}
			return cause
		}
		req.HostedThreadID = record.ID
		// The read-only resolver ran before registration so preview could use the
		// same path. Re-apply the now-canonical metadata and rebuild the provider
		// resume shape so Claude's identity block, in particular, carries the new
		// hosted id rather than an empty pre-registration placeholder.
		if err := s.applyCanonicalResumeMetadata(&req); err != nil {
			return nil, failRegistration(err)
		}
		switch req.Provider {
		case "codex":
			req, err = s.resolveCodexResume(req)
		case "claude":
			req, err = s.resolveClaudeResume(req)
		case "grok":
			req, err = s.resolveGrokResume(req)
		}
		if err != nil {
			return nil, failRegistration(err)
		}
	}
	if req.Mode != "resume" {
		if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("cwd %q is not a directory", req.Cwd)
		}
	}
	// One nano session per thread, mapped to the frozen 409 by
	// handleSessionCreate. Resumes are serialized above, so the first one
	// registers and every later one finds its session here. This runs before
	// any hosted binding is written: Codex also claims a daemon slot, but only
	// after its shell registers, too late to keep a losing resume from
	// rebinding the hosted record. The shim enforces the same rule one layer
	// down for a second nano instance, where it can only surface as a plain
	// spawn error: there is no sibling session id in this process to send the
	// client to.
	if req.Mode == "resume" {
		if req.retiredClaudeHandle != "" {
			s.forgetAttachment("claude", req.retiredClaudeHandle)
		}
		if existing := s.liveSessionForResume(req.Provider, req.ThreadID, req.HostedThreadID); existing != nil {
			return nil, &threadAttachedError{ExistingSessionID: existing.id}
		}
	}
	hostedThreadID := req.HostedThreadID
	creationRequestName := ""
	if req.Mode == "start" {
		// Only the creation request may establish the immutable creation fact.
		// Resume inputs, including boot's staged rename, are never that request.
		creationRequestName = strings.TrimSpace(req.Name)
	}
	record, err := s.agentExecutions.ensureNamedSessionWithIdentity(hostedThreadID, req.Provider, req.PresentationMode, req.InvokerHostedThreadID, req.InvokerNativeThreadID, req.InvokerProvider, creationRequestName)
	if err != nil {
		return nil, err
	}
	hostedThreadID = record.ID
	req.SidebarPlacement = record.SidebarPlacement
	if req.InvokerHostedThreadID == "" {
		req.InvokerHostedThreadID = record.InvokerHostedThreadID
		req.InvokerNativeThreadID = record.InvokerNativeThreadID
		req.InvokerProvider = record.InvokerProvider
	}
	// Provider builders (claudeArgs, codexThreadParams) read the hosted identity
	// from the request; without this write-back the UI path hands them an empty
	// id and only agent-API spawns would carry identity.
	req.HostedThreadID = hostedThreadID
	if req.Mode == "resume" && s.controlMetadata != nil {
		if err := s.controlMetadata.persistMissingResumeCwd(hostedThreadID, req.Cwd); err != nil {
			return nil, fmt.Errorf("persist resume cwd: %w", err)
		}
	}
	creationName := s.creationNameForHosted(hostedThreadID)
	if creationName == "" {
		creationName = creationRequestName
	}
	pendingName := strings.TrimSpace(req.PendingName)
	displayName := firstNonEmpty(pendingName, creationName)
	if req.Mode == "resume" {
		if durableName, ok := s.durableThreadName(req.Provider, req.ThreadID); ok {
			displayName = durableName
		}
	}
	nanoSessionID := newSessionID()
	sess := &session{
		id:                    nanoSessionID,
		provider:              req.Provider,
		hostedThreadID:        hostedThreadID,
		invokerHostedThreadID: req.InvokerHostedThreadID,
		invokerNativeThreadID: req.InvokerNativeThreadID,
		invokerProvider:       req.InvokerProvider,
		presentationMode:      req.PresentationMode,
		sidebarPlacement:      req.SidebarPlacement,
		cwd:                   req.Cwd,
		permissionMode:        firstNonEmpty(req.PermissionMode, req.Approval),
		createdAt:             time.Now(),
		spawnMode:             req.Mode,
		subs:                  map[chan wireEvent]struct{}{},
		state:                 map[string]json.RawMessage{},
		reasoningSummary:      s.defaults.reasoningSummary,
		projection:            s.newSessionProjection(req.Provider, hostedThreadID),
		turnEffort:            req.Effort,
		pendingName:           pendingName,
		displayName:           displayName,
		nameThread:            s.assignSpawnName,
		publishCreationName:   s.publishCreationName,
		creationName: func() string {
			return s.creationNameForHosted(hostedThreadID)
		},
		metadataName:         s.durableThreadNameRef,
		skills:               s.skills,
		catalogRefresh:       s.catalogRefresh,
		runtimeNotify:        s.notifyRuntime,
		rememberAttachment:   s.rememberAttachment,
		forgetAttachment:     s.forgetAttachment,
		updateTurnAnchor:     s.setAttachmentTurnAnchor,
		updateShimSession:    s.setAttachmentShimSession,
		sendAttempts:         s.sendAttempts,
		queuedTurns:          s.queuedTurns,
		turns:                s.turns,
		completedTurns:       s.completedTurns,
		agentExecutions:      s.agentExecutions,
		metadata:             s.controlMetadata,
		notices:              s.notices,
		pendingTurnCausation: []turnCausationEntry{},
		lastSettingsEvidence: map[string]turnCausationEntry{},
		sendOps:              map[string]*turnSendOperation{},
		nanoStopping:         &s.stopping,
		deliveryExpiries:     s.deliveryExpiries,
	}
	// Reserve a resumed native identity before the shell becomes visible or any
	// asynchronous provider work begins. Claude and Grok do not have Codex's
	// daemon claim table, so sessionForThread is their duplicate-attachment
	// guard. A background handshake may confirm or succeed this identity later,
	// but it must never leave a window where a second Resume sees no owner.
	if req.Mode == "resume" && (req.Provider == "claude" || req.Provider == "grok") {
		sess.threadID = req.ThreadID
		sess.setProjectionThreadID(req.ThreadID)
	}
	if s.controlMetadata != nil {
		if err := s.controlMetadata.ensureHostedThread(hostedThreadID, req.Provider, req.PresentationMode, req.InvokerHostedThreadID, req.InvokerNativeThreadID, req.InvokerProvider); err != nil {
			return nil, fmt.Errorf("canonical hosted metadata: %w", err)
		}
		generation, err := s.controlMetadata.beginTransportBinding(hostedThreadID)
		if err != nil {
			return nil, fmt.Errorf("begin hosted transport binding: %w", err)
		}
		sess.attachmentGeneration = generation
		sess.lifecycleState, err = lifecycle.New(uint64(generation))
		if err != nil {
			return nil, err
		}
	}
	s.startLiveText(sess)
	// Runtime posture hydration lives at the provider seams —
	// seedCodexTurnOverrides and the Claude resume overlay — because what
	// "re-applying posture" means is provider mechanics.
	sess.savedTurn = func(ctx context.Context, turnID string) (durableTurnEvidence, bool) {
		return s.savedTurnEvidence(ctx, sess, turnID)
	}
	sess.savedPrompts = s.savedPromptsFor(sess)
	sess.savedPromptsIn = s.savedPromptRows
	sess.savedTurnFinal = s.savedTurnFinal
	sess.subscriptionSweep = s.sweepSubscriptions
	spool, err := newWireSpool(s.spoolDir, sess.id)
	if err != nil {
		return nil, fmt.Errorf("create wire spool: %w", err)
	}
	sess.spool = spool

	// Registered BEFORE any provider work: a close arriving while the handshake
	// is still in flight has to be able to reach this session, and the card has
	// to appear at once rather than when the provider answers. Bind: the session
	// is now visible to the catalog's runtime overlay.
	sess.restart = req.restart
	sess.catalogBackground = req.catalogBackground
	if s.catalogSources != nil {
		sess.catalogSources = s.catalogSources
	}
	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()
	sess.bindCatalogSource(sess.currentThreadID())
	sess.restart.at(restartRegistered)
	if sess.agentExecutions != nil {
		// presentationMode is mutable under stateMu once the session is
		// resolvable by the presentation endpoint, which it is as of the
		// registration above.
		sess.stateMu.Lock()
		presentationMode, status, threadID := sess.presentationMode, sess.connectionStatusLocked(), sess.threadID
		sess.stateMu.Unlock()
		if err := sess.agentExecutions.bindSession(
			sess.hostedThreadID, sess.id, threadID, status, presentationMode,
		); err != nil {
			log.Printf("bind hosted thread %s: %v", sess.hostedThreadID, err)
		}
	}
	// Asynchronous: a prompt notice that prepares this thread holds the
	// dispatcher while it waits for this registration.
	sess.subscriptionSweep(sess.hostedThreadID)
	if req.immediateRuntimePublish {
		s.publishRuntime(false)
	} else {
		s.notifyRuntime()
	}
	if req.Provider == "codex" {
		// Boot can register dozens of panes at once. Start the shared cwd refresh
		// now, without waiting for a browser to ask each session for /state.
		sess.skills.current(req.Cwd)
	}

	if req.Mode != "resume" {
		sess.recordPosture(creationPostureEntries(req), metadataFreshnessLive)
	}
	if err := s.beginProviderSpawn(sess, req); err != nil {
		s.discardSpawn(sess)
		return nil, err
	}
	return sess, nil
}

// beginProviderSpawn hands the session to its provider. What it returns is what
// the POST answers with, so only the failures a caller can act on — an
// already-attached thread, a resume ladder that found nothing to attach to —
// come back as errors here. Handshake failures land on the session itself.
func (s *server) beginProviderSpawn(sess *session, req spawnRequest) error {
	if err := sess.ensureCoreSession(); err != nil {
		return err
	}
	if req.Provider == "claude" && req.Mode == "resume" {
		// Claude attach waits for replay to pass through the session mailbox.
		// Run its orchestration outside that mailbox so it can make progress.
		// Manual resumes still return their synchronous HTTP refusal errors;
		// boot and external resumes retain their existing asynchronous shells.
		return s.beginProviderSpawnLegacy(sess, req)
	}
	operation := func(context.Context) error { return s.beginProviderSpawnLegacy(sess, req) }
	if req.Mode == "resume" {
		return sess.core.ResumeLegacy(context.Background(), operation)
	}
	return sess.core.StartLegacy(context.Background(), operation)
}

// beginProviderSpawnLegacy is the c2: asynchronous attachment shim. Provider
// adapters replace it when their Start and Resume operations return evidence.
func (s *server) beginProviderSpawnLegacy(sess *session, req spawnRequest) error {
	if req.Provider == "codex" {
		return startCodexSession(sess, s.codex, req)
	}
	if req.Provider == "grok" {
		startGrokSession(sess, req)
		return nil
	}
	if req.Mode == "resume" {
		if req.bootReattach != nil {
			go func() {
				defer req.bootReattach.holdHandshakeSlot(sess)()
				sess.stateMu.Lock()
				closed := sess.closed
				sess.stateMu.Unlock()
				if closed {
					sess.restart.fail("closed")
					return
				}
				if err := s.resumeClaudeForSpawn(sess, req); err != nil {
					if errors.Is(err, errSpawnSessionClosed) {
						sess.restart.fail("closed")
						log.Printf("claude boot session %s was closed before its transport came up", sess.id)
						return
					}
					sess.restart.fail("handshake_failed")
					log.Printf("boot re-attach %s/%s failed (left detached): %v", req.Provider, req.ThreadID, err)
					s.discardSpawn(sess)
				}
			}()
			return nil
		}
		if req.externalResumePosture {
			go func() {
				if err := s.resumeClaudeForSpawn(sess, req); err != nil {
					if errors.Is(err, errSpawnSessionClosed) {
						return
					}
					sess.failSpawn(err.Error())
				}
			}()
			return nil
		}
		return s.resumeClaudeForSpawn(sess, req)
	}
	go func() {
		if _, err := startClaudeSession(sess, req); err != nil {
			if errors.Is(err, errSpawnSessionClosed) {
				log.Printf("claude session %s was closed before its transport came up", sess.id)
				return // closed by the operator: reporting a failure would resurrect it
			}
			log.Printf("start claude session %s: %v", sess.id, err)
			sess.failSpawn(err.Error())
		}
	}()
	return nil
}

func (s *server) resumeClaudeForSpawn(sess *session, req spawnRequest) error {
	// The manual path calls this synchronously because its rungs decide WHICH
	// session the operator gets (attach, respawn, or refuse), and the refusal is
	// the frozen 409 the frontend routes on. Boot runs the same work behind its
	// already-registered shell because no HTTP caller is waiting for that choice.
	_, err := resumeClaudeSession(sess, req)
	if err == nil {
		sess.restart.at(restartAttached)
	}
	if errors.Is(err, errClaudeProvisionalGone) {
		// The never-prompted thread's child is gone and it has no transcript
		// to come back from. Retire the record: an empty conversation lost is
		// nothing lost, while a surviving record would sit in the sidebar as
		// a detached row that can never resume.
		s.forgetAttachment("claude", req.ThreadID)
	}
	return err
}

// discardSpawn unwinds a session whose provider refused it outright. The shell
// was registered before the attempt (so a concurrent close could reach it), so
// registration has to be undone with it, and the spool file removed — nothing
// will ever read it.
func (s *server) discardSpawn(sess *session) {
	s.mu.Lock()
	delete(s.sessions, sess.id)
	s.mu.Unlock()
	sess.releaseCatalogSource()
	if sess.spool != nil {
		if err := sess.spool.close(true); err != nil {
			log.Printf("close wire spool %s: %v", sess.id, err)
		}
	}
	s.notifyRuntime()
}

// close tears down the session's provider attachment.
func (sess *session) close() {
	if err := sess.closeProvider(false); err != nil {
		log.Printf("session %s teardown unconfirmed: %v", sess.id, err)
		return
	}
	sess.confirmClaudeClose()
}

// Internal teardown and explicit close both settle only after a successful
// stop. Explicit close additionally checks attachment removal before calling.
func (sess *session) confirmClaudeClose() {
	if sess.provider != "claude" {
		return
	}
	sess.stateMu.Lock()
	unconfirmed := sess.closeUnconfirmed
	sess.closeUnconfirmed = false
	sess.stateMu.Unlock()
	if unconfirmed {
		sess.settleAgentTurnsOnExit(true)
		sess.updateHostedThread(sessionStatusExited, "closed")
	}
}

// Explicit HTTP close callers must use closeWithResult and preserve failures.
func (sess *session) closeWithResult() error { return sess.closeProvider(true) }

func (sess *session) closeProvider(requireReady bool) error {
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

// summary is one session's full summary: its canonical view (three reads),
// its durable name (one read) and its attention flag from the one-pass
// operation map.
func (sess *session) summary() sessionSummary {
	view, err := hostedMetadataView{}, error(sql.ErrNoRows)
	if sess.metadata != nil {
		view, err = sess.metadata.view(sess.hostedThreadID)
	}
	summary := sess.summaryWith(view, err, sess.agentExecutions.sessionNeedsAttention(sess.hostedThreadID))
	if summary.Metadata != nil {
		summary.Metadata.Name, _ = sess.hostedMetadataName(summary.Provider, summary.ThreadID, summary.Name)
	}
	return summary
}

// summaryWith builds a summary from a canonical view already read (viewErr
// set when there is none) with no store read of its own, so a list reads all
// views in one batch. The view's name is the display name; summary replaces
// it with the durable name for one session. The sessions list carries no name
// in its metadata.
func (sess *session) summaryWith(view hostedMetadataView, viewErr error, needsAttention bool) sessionSummary {
	sess.stateMu.Lock()
	presentationMode := sess.presentationMode
	if presentationMode == "" {
		presentationMode = presentationShared
	}
	summary := sessionSummary{
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
	summary.ThreadDiagnostics = sess.projection.threadDiagnosticsSnapshot()
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
	summary.Posture = postureFromFields(view.Fields)
	summary.Metadata = &view
	summary.Model = summary.Posture.Model
	if cwd, ok := canonicalMetadataString(view.Fields["cwd"]); ok {
		summary.Cwd = cwd
	}
	return summary
}

func (sess *session) hostedMetadataName(provider, nativeThreadID, fallback string) (hostedMetadataName, bool) {
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
func (sess *session) failSpawn(message string) {
	sess.restart.fail("handshake_failed")
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
	sess.publishProjectionRuntime()
	sess.notifyRuntime()
	sess.updateHostedThread(sessionStatusExited, message)
}

// claudeHandle snapshots the claude provider handle, which a fresh start now
// publishes from a background goroutine (markTransportReady, claude.go). Nil
// means the transport is not up yet: readiness and the handle are published
// under one lock, but the child's own init line can promote a session a moment
// earlier, so every caller checks rather than trusting "ready".
func (sess *session) claudeHandle() *claudeSession {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.claude
}

func (sess *session) grokHandle() *grokSession {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.grok
}

func (sess *session) setState(key string, v json.RawMessage) {
	sess.stateMu.Lock()
	sess.state[key] = v
	sess.stateMu.Unlock()
}

func (sess *session) settleAgentTurnsOnExit(closed bool) {
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
	if err := sess.agentExecutions.markSessionTurns(sess.hostedThreadID, turnStatus, executionErr); err != nil {
		log.Printf("settle turns for exited hosted thread %s: %v", sess.hostedThreadID, err)
	}
}

func (sess *session) updateHostedThread(status, message string) {
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
		if err := skipOwnedNativeClaim(sess.metadata.claimNativeID(sess.hostedThreadID, sess.provider, sess.currentThreadID(), nativeThreadIDProvisional(sess.provider, sess.currentThreadID()))); err != nil {
			log.Printf("claim canonical native id for %s: %v", sess.hostedThreadID, err)
		}
	}
	if sess.agentExecutions == nil {
		return
	}
	if err := sess.agentExecutions.updateSession(
		sess.hostedThreadID, sess.id, sess.currentThreadID(), status, message,
	); err != nil {
		log.Printf("update hosted thread %s: %v", sess.hostedThreadID, err)
	}
}

func (sess *session) rememberDurableAttachment(rec attachmentRecord) {
	if sess.rememberAttachment == nil {
		return
	}
	// A never-prompted Claude child has only its shim handle. Keep the New
	// Window title beside that provisional identity until system.init supplies
	// the real native id and applyPendingName can write the ordinary name
	// overlay. Real attachment records must not retain this staging value: a
	// later manual rename is authoritative and must never be overwritten at boot.
	if rec.provisional() {
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

// setTurnActive tracks whether a turn is in flight, which turn, and when it
// began; only real flips reach the runtime SSE stream. A completing turn drops
// the id and the anchor with the flag, so nothing can later interrupt a turn
// that already ended and no surface can keep timing one.
//
// startedAtMs is evidence, not a default: it is honored only on the false→true
// edge (D7/D10 — a same-turn event must never restart the clock), and 0 is a
// legitimate value meaning "no evidence", which renders as working-without-a-
// timer rather than as a fabricated start (D2/D8).
//
// live gates the completion stamp the same way (D2/D8, mirrored on the close
// edge): a shim-replayed turn close reconstructs state, but the turn did not
// end NOW — stamping arrival time rewrote every reattached thread's "last
// turn ended" to the deploy moment on each nano restart (observed live
// 2026-08-06: 14 threads sharing one restart minute). A replayed close leaves
// the stamp absent, which is the documented post-restart state of this
// deliberately unpersisted field.
func (sess *session) setTurnActive(active bool, turnID string, startedAtMs int64, live bool) {
	sess.setTurnActiveState(active, turnID, startedAtMs, live, true)
}

func (sess *session) beginTurnRestore() {
	sess.stateMu.Lock()
	if !sess.turnRestoreInProgress {
		sess.turnRestoreInProgress = true
		sess.turnRestoreSequence = 0
		sess.turnRestoreActive = sess.workActiveLocked()
		sess.turnRestoreAccepted = sess.lifecycleState.State.Work == lifecycle.Accepted
		sess.turnRestoreID = sess.turnID
		sess.turnRestoreStartedAtMs = sess.turnStartedAtMs
	}
	sess.stateMu.Unlock()
}

func (sess *session) cancelTurnRestore() {
	sess.stateMu.Lock()
	sess.clearTurnRestoreLocked()
	sess.stateMu.Unlock()
}

func (sess *session) clearTurnRestoreLocked() {
	sess.turnRestoreInProgress = false
	sess.turnRestoreSequence = 0
	sess.turnRestoreActive = false
	sess.turnRestoreAccepted = false
	sess.turnRestoreID = ""
	sess.turnRestoreStartedAtMs = 0
}

// restoreTurnActive reconstructs one historical turn boundary without
// publishing it as present-tense runtime. A Claude shim replay can contain
// hundreds of completed turns; announcing each prompt/result pair makes every
// browser repeatedly promote and demote the restored thread while Nano boots.
// The attach path calls settleRestoredTurnState once after the complete replay
// and the shim's current-state snapshot have both landed.
func (sess *session) restoreTurnActive(active bool, turnID string, startedAtMs int64) {
	sess.setTurnActiveState(active, turnID, startedAtMs, false, false)
}

func (sess *session) setTurnActiveState(active bool, turnID string, startedAtMs int64, live, publish bool) {
	sess.stateMu.Lock()
	restoring := sess.turnRestoreInProgress
	currentActive, currentID := sess.workActiveLocked(), sess.turnID
	if restoring {
		currentActive, currentID = sess.turnRestoreActive, sess.turnRestoreID
	}
	if active && turnID != "" && (turnID == sess.durablyTerminalTurnID || turnID == sess.lastProviderTerminalTurnID) {
		sess.stateMu.Unlock()
		return
	}
	// A late terminal belongs to its own turn, never a newer one. Anonymous
	// boundaries are qualified by the provider adapter before reaching here.
	// Claude runs one execution at a time: its result ends it whichever
	// prompt it answered, and a prompt written mid-turn is often the newer
	// turn here.
	if !active && turnID != "" && currentID != "" && turnID != currentID && sess.provider != "claude" {
		sess.stateMu.Unlock()
		return
	}
	if !restoring && sess.lifecycleState.State.Connection != lifecycle.Ready {
		sess.stateMu.Unlock()
		return
	}
	changed, drain := false, false
	if restoring {
		sess.turnRestoreSequence = sess.nextEvidenceSequenceLocked()
		changed = currentActive != active
		if active {
			if !currentActive || sess.turnRestoreAccepted || currentID != turnID {
				sess.turnRestoreStartedAtMs = startedAtMs
			}
			sess.turnRestoreActive, sess.turnRestoreAccepted = true, false
			sess.turnRestoreID = turnID
		} else {
			sess.turnRestoreActive, sess.turnRestoreAccepted = false, false
			sess.turnRestoreID, sess.turnRestoreStartedAtMs = "", 0
		}
	} else if active {
		wasAccepted := sess.lifecycleState.State.Work == lifecycle.Accepted
		changed, drain = sess.transitionLifecycleLocked(lifecycle.EventStarted)
		if !currentActive || wasAccepted || currentID != turnID {
			sess.turnStartedAtMs = startedAtMs
		}
		sess.turnID = turnID
	} else {
		changed, drain = sess.transitionLifecycleLocked(lifecycle.EventFinished)
		sess.turnID, sess.turnStartedAtMs = "", 0
	}
	if !active {
		// A replayed turn end too: a compaction it held ends with it.
		sess.compactingSinceMs = 0
		if live {
			sess.lastProviderTerminalTurnID = turnID
		}
		if changed && live {
			sess.lastTurnEndedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
	}
	sess.stateMu.Unlock()
	if changed && publish && !restoring {
		sess.publishProjectionRuntime()
		sess.notifyRuntime()
		if drain {
			sess.kickQueuedTurnDrain()
		}
	}
}

// settleRestoredTurnState exposes exactly one runtime answer after a Claude
// replay. It also releases a prompt queued while the replay was draining when
// the settled state is idle.
func (sess *session) settleRestoredTurnState() {
	sess.stateMu.Lock()
	if !sess.turnRestoreInProgress {
		sess.stateMu.Unlock()
		return
	}
	drain, status := sess.settleRestoredTurnStateLocked()
	sess.stateMu.Unlock()
	sess.publishRestoredTurnState(drain, status)
}

func (sess *session) settleRestoredTurnStateLocked() (drain bool, status string) {
	_, drain = sess.transitionLifecycleLocked(sess.readyLifecycleEventLocked())
	if sess.workActiveLocked() {
		sess.turnID, sess.turnStartedAtMs = sess.turnRestoreID, sess.turnRestoreStartedAtMs
	} else {
		sess.turnID, sess.turnStartedAtMs = "", 0
	}
	sess.clearTurnRestoreLocked()
	return drain, sess.connectionStatusLocked()
}

func (sess *session) publishRestoredTurnState(drain bool, status string) {
	sess.publishProjectionRuntime()
	sess.notifyRuntime()
	sess.updateHostedThread(status, "")
	if drain {
		sess.kickQueuedTurnDrain()
	}
}

// restoreTurnAnchor supplies the start instant of a turn that is ALREADY
// active but has no anchor — the claude re-attach path, where the replay
// re-derives the turn state but deliberately refuses to date it from arrival
// time. Two guards make this a correction rather than a second source of
// truth: it never fires on an idle session, and it may only move the anchor
// BACKWARD (D9/D11), so a recovered start can lengthen the reported elapsed
// but never shorten it.
func (sess *session) restoreTurnAnchor(startedAtMs int64) bool {
	return sess.restoreTurnAnchorState(startedAtMs, true)
}

func (sess *session) restoreTurnAnchorQuietly(startedAtMs int64) bool {
	return sess.restoreTurnAnchorState(startedAtMs, false)
}

func (sess *session) restoreTurnAnchorState(startedAtMs int64, publish bool) bool {
	if startedAtMs <= 0 {
		return false
	}
	sess.stateMu.Lock()
	restoring := sess.turnRestoreInProgress
	active := sess.workActiveLocked()
	anchor := sess.turnStartedAtMs
	if restoring {
		active = sess.turnRestoreActive
		anchor = sess.turnRestoreStartedAtMs
	}
	restore := active && (anchor == 0 || startedAtMs < anchor)
	if restore {
		if restoring {
			sess.turnRestoreStartedAtMs = startedAtMs
		} else {
			sess.turnStartedAtMs = startedAtMs
		}
	}
	sess.stateMu.Unlock()
	if restore && publish && !restoring {
		sess.publishProjectionRuntime()
		sess.notifyRuntime()
	}
	return restore
}

// turnAnchor snapshots the live turn's anchor, 0 when there is none.
func (sess *session) turnAnchor() int64 {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	if sess.turnRestoreInProgress {
		return sess.turnRestoreStartedAtMs
	}
	return sess.turnStartedAtMs
}

// compactingSinceMsLocked is the compaction start the session shows: only a
// turn in flight can be compacting, whichever path ended the last one.
func (sess *session) compactingSinceMsLocked() int64 {
	if !sess.workActiveLocked() {
		return 0
	}
	return sess.compactingSinceMs
}

// markCompacting applies a provider's own compaction signal: a start keeps
// the first start it saw (startedAtMs), an end clears it. A change reaches
// every pane's status and the rail.
func (sess *session) markCompacting(compacting bool, startedAtMs int64) {
	sess.stateMu.Lock()
	since := sess.compactingSinceMs
	switch {
	case !compacting:
		since = 0
	case since == 0:
		since = startedAtMs
	}
	changed := since != sess.compactingSinceMs
	sess.compactingSinceMs = since
	sess.stateMu.Unlock()
	if changed {
		sess.publishProjectionRuntime()
		sess.notifyRuntime()
	}
}

// activeTurn snapshots the (thread, turn) pair an interrupt would address.
// An empty turn id means there is nothing to interrupt.
func (sess *session) activeTurn() (threadID, turnID string) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	if !sess.workActiveLocked() {
		return sess.threadID, ""
	}
	return sess.threadID, sess.turnID
}

// turnOptions snapshots the per-turn overrides the next turn/start will carry.
func (sess *session) turnOptions() (model, effort, serviceTier string) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.turnModel, sess.turnEffort, sess.turnServiceTier
}

func (sess *session) notifyRuntime() {
	if sess.runtimeNotify != nil {
		sess.runtimeNotify()
	}
}

func (sess *session) currentThreadID() string {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.threadID
}

func (sess *session) catalogPathForNudge(line []byte) string {
	if sess.provider == "codex" {
		sess.learnCatalogSourcePath(codexCatalogPath(line))
	}
	sess.stateMu.Lock()
	path, threadID := sess.catalogSourcePath, sess.threadID
	sess.stateMu.Unlock()
	if sess.provider == "claude" {
		return claudeTranscriptPath(sess.cwd, threadID)
	}
	return path
}

// learnCatalogSourcePath records the native file a Codex thread reported and
// moves the attachment's hold to it when it differs.
func (sess *session) learnCatalogSourcePath(path string) {
	if path == "" {
		return
	}
	sess.stateMu.Lock()
	changed := sess.catalogSourcePath != path
	sess.catalogSourcePath = path
	threadID := sess.threadID
	sess.stateMu.Unlock()
	if changed {
		sess.bindCatalogSource(threadID)
	}
}

func (sess *session) setProjectionThreadID(id string) {
	sess.stateMu.Lock()
	if sess.durablyTerminalTurnThreadID != "" && sess.durablyTerminalTurnThreadID != id {
		sess.durablyTerminalTurnID = ""
		sess.durablyTerminalTurnThreadID = ""
	}
	sess.stateMu.Unlock()
	if sess.projection != nil {
		sess.projection.setNativeThreadID(id)
	}
	sess.bindCatalogSource(id)
}

// bindCatalogSource holds the native files of identity id. Until id is a
// real provider identity the attachment is held unresolved, with no file.
// Holding runs outside session locks; a close that races it releases again,
// so a closed attachment never keeps a watch.
func (sess *session) bindCatalogSource(id string) {
	if sess.catalogSources == nil {
		return
	}
	sess.stateMu.Lock()
	closed, cwd, known, background := sess.closed, sess.cwd, sess.catalogSourcePath, sess.catalogBackground
	sess.stateMu.Unlock()
	if closed {
		return
	}
	sess.catalogSources.bindAttachedSource(sess.id, sess.provider, id, cwd, known, background)
	sess.stateMu.Lock()
	closed = sess.closed
	sess.stateMu.Unlock()
	if closed {
		sess.releaseCatalogSource()
	}
}

func (sess *session) releaseCatalogSource() {
	if sess.catalogSources != nil {
		sess.catalogSources.releaseAttachedSource(sess.id)
	}
}

func (sess *session) recordWire(dir protocol.Direction, line []byte) {
	sess.recordWireLive(dir, line, true)
}

// recordWireLive controls whether arrival time may stand as a turn's start
// anchor. Claude re-attach calls recordWireAt directly so it can also mark
// a frame as replayed.
func (sess *session) recordWireLive(dir protocol.Direction, line []byte, live bool, wireSeq ...int64) {
	sess.recordWireAt(dir, line, live, false, time.Time{}, wireSeq...)
}

// recordWireRaw preserves provider evidence that belongs to another native
// session multiplexed onto this attachment. It intentionally does not invoke
// any parent semantic consumer (projection, runtime, send confirmation, or
// hosted-operation observation).
func (sess *session) recordWireRaw(dir protocol.Direction, line []byte) {
	at := time.Now()
	// A multiplexed child without its own attachment is not watched: its raw
	// traffic hints nothing, and only an explicit child attachment opts in.
	sess.publishWireEvent(wireEvent{
		TS: at.Format(time.RFC3339Nano), Dir: string(dir),
		Msg: json.RawMessage(append([]byte(nil), line...)),
	}, true)
}

// recordWireAt is recordWireLive with an explicit arrival time, which the
// projection stamps onto every item the line produces.
//
// The live flag protects a turn's start anchor; replay independently marks a
// Claude re-attach source turn for owner reconciliation. Neither protects the
// per-message timestamps, and for a long time nothing did. A
// claude re-attach replayed a whole conversation through the arrival clock, so
// every message in the transcript re-dated itself to the instant nano came
// back: an eleven-minute session read as eleven minutes of activity compressed
// into the 0.3s the replay took. The shim spools the time it first saw each
// line and now sends it back with the replay, so a replayed line is dated when
// it happened. A zero `at` means "now" — every genuinely live path, and any
// line from a shim too old to send one.
func (sess *session) recordWireAt(dir protocol.Direction, line []byte, live, replay bool, at time.Time, wireSeq ...int64) {
	sess.recordWireWith(nil, nil, dir, line, live, replay, at, wireSeq...)
}

// recordWireWith checks gate, when set, immediately before the frame is
// applied and again before it is published: an error skips the frame, or at
// least its publication when the gate closed while the frame was applying.
// prepare runs once the frame is admitted. Replayed send lines take the
// session loop too, so they land in spool order among the received frames
// queued ahead of them.
func (sess *session) recordWireWith(gate func() error, prepare func(), dir protocol.Direction, line []byte, live, replay bool, at time.Time, wireSeq ...int64) {
	if at.IsZero() {
		at = time.Now()
	}
	evt := wireEvent{
		TS:  at.Format(time.RFC3339Nano),
		Dir: string(dir),
		Msg: json.RawMessage(append([]byte(nil), line...)),
	}
	if core := sess.coreSession(); core != nil && (dir == protocol.Recv || replay) {
		frame := append([]byte(nil), evt.Msg...)
		sequence := append([]int64(nil), wireSeq...)
		adapterEvent, terminal := sess.adapterTurnEvent(dir, frame, live, at)
		adapterEvent.LegacyApply = func() error {
			if gate != nil {
				if err := gate(); err != nil {
					return err
				}
			}
			if prepare != nil {
				prepare()
			}
			sess.applyWireSemantic(gate, dir, frame, live, replay, at, sequence...)
			if terminal != nil && sess.projection != nil {
				terminal.FinalMessageID = sess.projection.finalAgentItemID(terminal.TurnID)
				if usage := sess.completedTokenUsage(terminal.TurnID); usage != nil {
					terminal.Usage = coreturns.Usage{InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens, OutputTokens: usage.OutputTokens}
				}
			}
			// A frame admitted before its attach was abandoned finishes its own
			// apply, but neither publishes nor carries a completion.
			if gate != nil {
				if err := gate(); err != nil {
					return err
				}
			}
			sess.publishWireEvent(evt, true)
			return nil
		}
		if gate == nil && prepare == nil && live && !replay && sess.provider == "codex" &&
			dir == protocol.Recv && sess.projection != nil {
			if delta, ok := parseQueuedCodexDelta(frame); ok &&
				(delta.method != "item/agentMessage/delta" || !sess.projection.dropLiveTextDeltas) {
				delta.at = at
				delta.event = evt
				delta.wireSeq = sequence
				batch := &queuedCodexDeltaBatch{first: delta}
				batch.append(delta)
				adapterEvent.AsyncBatchKey = strings.Join([]string{
					"codex-wire-delta", delta.method, delta.threadID, delta.turnID, delta.itemID,
				}, "\x00")
				adapterEvent.AsyncBatchEpoch = core.AttachmentEpoch()
				adapterEvent.AsyncBatchBytes = len(frame)
				adapterEvent.AsyncBatchPayload = delta
				adapterEvent.AsyncBatchMerge = func(payload any) {
					if next, ok := payload.(queuedCodexDelta); ok {
						batch.append(next)
					}
				}
				adapterEvent.LegacyApply = func() error {
					merged, err := batch.mergedLine()
					if err != nil {
						return err
					}
					last := batch.frames[len(batch.frames)-1]
					sequences := make([]int64, 0, len(batch.frames))
					for _, queued := range batch.frames {
						sequences = append(sequences, queued.wireSeq...)
					}
					sess.applyWireSemantic(nil, dir, merged, live, false, last.at, sequences...)
					for _, queued := range batch.frames {
						sess.publishWireEvent(queued.event, true)
					}
					return nil
				}
			}
		}
		core.ObserveAsync(adapterEvent)
		return
	}
	if gate != nil && gate() != nil {
		return
	}
	if prepare != nil {
		prepare()
	}
	sess.applyWireSemantic(gate, dir, line, live, replay, at, wireSeq...)
	sess.publishWireEvent(evt, true)
}

func (sess *session) adapterTurnEvent(dir protocol.Direction, line []byte, live bool, at time.Time) (coresession.AdapterEvent, *coreturns.Finish) {
	if !live || dir != protocol.Recv {
		return coresession.AdapterEvent{}, nil
	}
	active, turnID, _, ok := turnActiveTransition(sess.provider, dir, line)
	if !ok || turnID == "" {
		return coresession.AdapterEvent{}, nil
	}
	actionID := ""
	if sess.queuedTurns != nil {
		if prompt, found := sess.queuedTurns.byProviderTurn(sess.provider, sess.currentThreadID(), sess.hostedThreadID, turnID); found {
			actionID = prompt.ClientActionID
		}
	}
	event := coresession.AdapterEvent{ProviderTurnID: turnID}
	if active {
		started := coreturns.Start{HostedThreadID: sess.hostedThreadID, Provider: sess.provider, NativeThreadID: sess.currentThreadID(), TurnID: turnID, ActionID: actionID, At: at, Source: "adapter"}
		event.TurnEvents = []coresession.TurnEvent{{Started: &started}}
		return event, nil
	}
	status := agentTurnCompleted
	if sess.provider == "codex" {
		var envelope struct {
			Params struct {
				Turn struct {
					Status string `json:"status"`
				} `json:"turn"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &envelope) == nil && envelope.Params.Turn.Status != "" {
			status = envelope.Params.Turn.Status
		}
	} else if sess.provider == "grok" {
		_, parsedStatus, _, terminal := grokTerminalFromWire(line)
		if !terminal {
			return coresession.AdapterEvent{}, nil
		}
		status = parsedStatus
	}
	finished := &coreturns.Finish{HostedThreadID: sess.hostedThreadID, Provider: sess.provider, NativeThreadID: sess.currentThreadID(), TurnID: turnID, ActionID: actionID, Outcome: status, At: at, Source: "adapter"}
	event.TurnEvents = []coresession.TurnEvent{{Finished: finished}}
	return event, finished
}

// applyWireSemantic is the c2: raw-wire translation shim. core/session calls
// it on its sole event goroutine, so one frame finishes before the next event.
//
// gate, when set, is rechecked before the durable agent-operation update, so a
// frame admitted before its attach was abandoned cannot complete an operation.
func (sess *session) applyWireSemantic(gate func() error, dir protocol.Direction, line []byte, live, replay bool, at time.Time, wireSeq ...int64) {
	// Normalize before publishing the raw line. Every browser therefore sees
	// the same projection order, and a slow/raw-feed tab cannot affect the
	// transcript reducer.
	if sess.projection != nil {
		sess.projection.applyWireLive(dir, line, at, live, replay, wireSeq...)
	}
	// Wire-native catalog freshness: turn edges hint the session's known
	// native file. Runs outside every session lock, and the callback is the
	// catalog's nonblocking queue, so recording never stalls.
	nudgesCatalog := wireNudgesCatalog(sess.provider, dir, line)
	if sess.catalogRefresh != nil && nudgesCatalog {
		if path := sess.catalogPathForNudge(line); path != "" {
			sess.catalogRefresh(path)
		}
	}
	sess.observeProviderWork(dir, line, live)
	sess.applyLiveRuntimeWire(dir, line, at)
	// An MCP server changing startup state means the cached tool registry is
	// stale; the next panel fetch re-asks app-server (mcp_inventory.go).
	if mcpInventoryStale(sess.provider, dir, line) {
		sess.mcp.invalidate()
	}
	if sess.projection != nil && wireNudgesProjectionMeta(sess.provider, dir, line) {
		sess.projection.nudgeMetaRefresh()
	}
	if gate != nil && gate() != nil {
		return
	}
	sess.observeAgentExecutionWire(dir, line)
}

func (sess *session) publishWireEvent(evt wireEvent, writeSpool bool) {
	sess.mu.Lock()
	sess.seq++
	evt.Seq = sess.seq
	sess.events = append(sess.events, evt)
	if len(sess.events) > wireBufferCap {
		sess.events = sess.events[len(sess.events)-wireBufferCap:]
	}
	subs := make([]chan wireEvent, 0, len(sess.subs))
	for ch := range sess.subs {
		subs = append(subs, ch)
	}
	spool := sess.spool
	sess.mu.Unlock()

	var spoolErr error
	if writeSpool && spool != nil {
		spoolErr = spool.append(evt)
		if spoolErr != nil {
			log.Printf("wire spool append %s seq %d: %v", sess.id, evt.Seq, spoolErr)
		}
	}
	for _, ch := range subs {
		select {
		case ch <- evt:
		default: // slow subscriber: drop rather than block the read loop
		}
	}
	if spoolErr != nil {
		sess.recordSpoolDropped(evt.Seq, spoolErr)
	}
}

// nanoNotification builds a synthetic JSON-RPC notification for events nano
// itself originates (spool failures, instruction-file edits) so they render
// in the Event Feed like any other wire notification.
func nanoNotification(method string, params map[string]any) (wireEvent, bool) {
	line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return wireEvent{}, false
	}
	return wireEvent{
		TS:  time.Now().Format(time.RFC3339Nano),
		Dir: string(protocol.Recv),
		Msg: json.RawMessage(line),
	}, true
}

// recordNanoEvent publishes a synthetic nano-origin notification into the
// feed (and spool), keeping actions nano performs outside the agent wire —
// like instruction-file edits — visible in the Event Feed.
func (sess *session) recordNanoEvent(method string, params map[string]any) {
	if evt, ok := nanoNotification(method, params); ok {
		sess.publishWireEvent(evt, true)
	}
}

func (sess *session) recordSpoolDropped(seq int64, err error) {
	evt, ok := nanoNotification("wire/spool_dropped", map[string]any{
		"afterSeq": seq,
		"error":    err.Error(),
	})
	if ok {
		sess.publishWireEvent(evt, false) // the spool just failed; don't re-enter it
	}
}

func (sess *session) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpapi.WriteHTTPError(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	ch := make(chan wireEvent, 512)
	sess.mu.Lock()
	replay := append([]wireEvent(nil), sess.events...)
	sess.subs[ch] = struct{}{}
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		delete(sess.subs, ch)
		sess.mu.Unlock()
	}()

	writeEvent := func(evt wireEvent) bool {
		data, err := json.Marshal(evt)
		if err != nil {
			return false
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", data)
		return err == nil
	}
	for _, evt := range replay {
		if !writeEvent(evt) {
			return
		}
	}
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case evt := <-ch:
			if !writeEvent(evt) {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (sess *session) handleEventsPage(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			httpapi.WriteHTTPError(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = n
	}
	if limit > 2000 {
		limit = 2000
	}

	sess.mu.Lock()
	firstSeq := int64(0)
	if len(sess.events) > 0 {
		firstSeq = sess.events[0].Seq
	}
	currentSeq := sess.seq
	memory := append([]wireEvent(nil), sess.events...)
	spool := sess.spool
	sess.mu.Unlock()

	beforeSeq := firstSeq
	if beforeSeq == 0 {
		beforeSeq = currentSeq + 1
	}
	if raw := r.URL.Query().Get("beforeSeq"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			httpapi.WriteHTTPError(w, "beforeSeq must be a positive integer", http.StatusBadRequest)
			return
		}
		beforeSeq = n
	}

	events, hasMore, err := readEventsBefore(memory, spool, beforeSeq, limit)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	oldestSeq, newestSeq := int64(0), int64(0)
	if len(events) > 0 {
		oldestSeq = events[0].Seq
		newestSeq = events[len(events)-1].Seq
	}
	httpapi.WriteJSON(w, map[string]any{
		"events":    events,
		"hasMore":   hasMore,
		"oldestSeq": oldestSeq,
		"newestSeq": newestSeq,
	})
}

func readEventsBefore(memory []wireEvent, spool *wireSpool, beforeSeq int64, limit int) ([]wireEvent, bool, error) {
	if spool != nil {
		return spool.readBefore(beforeSeq, limit)
	}
	var older []wireEvent
	for _, evt := range memory {
		if evt.Seq < beforeSeq {
			older = append(older, evt)
		}
	}
	hasMore := len(older) > limit
	if hasMore {
		older = older[len(older)-limit:]
	}
	return older, hasMore, nil
}

// handleState returns the handshake snapshots (init, thread/start). Every pane
// reads it when it opens, so what only a panel shows has its own endpoint,
// fetched when that panel opens: the MCP tool registry (measured 0.84–6.1s of
// a ~250 KB payload, mcp_inventory.go) and the skills list (about 80% of a
// 60 KB answer, skills_cache.go).
// Claude's system.init snapshot is all there is (no request surface without
// the control protocol).
func (sess *session) handleState(w http.ResponseWriter, r *http.Request) {
	sess.stateMu.Lock()
	out := map[string]any{
		"sessionId": sess.id, "provider": sess.provider, "threadId": sess.threadID,
		"status": sess.connectionStatusLocked(),
	}
	// The immutable anchor, and only the anchor (D4): a pane that opens
	// mid-turn learns when the turn started from the same value the rail reads,
	// and derives elapsed against its own clock. Omitted when there is none, so
	// a missing key is honestly "unknown", never epoch 0.
	if sess.turnStartedAtMs != 0 {
		out["turnStartedAtMs"] = sess.turnStartedAtMs
	}
	if sess.lastTurnEndedAt != "" {
		out["lastTurnEndedAt"] = sess.lastTurnEndedAt
	}
	// turnActive rides too: a pane opened (or reloaded) mid-turn has no wire
	// events yet, and without this seed it renders an idle composer and no
	// working surface while the rail — fed by the session summary — blinks
	// away (observed on the infra sessions, 2026-07-31).
	out["turnActive"] = sess.workActiveLocked()
	if sess.spawnError != "" {
		out["error"] = sess.spawnError
	}
	for k, v := range sess.state {
		out[k] = v
	}
	threadID := sess.threadID
	cwd := sess.cwd // the background resume handshake may still be updating it
	displayName := sess.displayName
	metadataDegraded := sess.metadataDegraded
	initRaw := sess.state["init"]
	sess.stateMu.Unlock()

	// The same unified projections the summary serves (decision 4), so /state
	// and /api/sessions cannot disagree about model/effort/permission/tier.
	out["posture"] = sess.posture()
	if sess.metadata != nil {
		if view, err := sess.metadata.view(sess.hostedThreadID); err == nil {
			view.Attachment = hostedAttachmentView{State: fmt.Sprint(out["status"]), Provenance: "live_session"}
			if message, _ := out["error"].(string); message != "" {
				view.Attachment.Error = message
			}
			if name, ok := sess.hostedMetadataName(sess.provider, threadID, displayName); ok {
				view.Name = name
			}
			out["metadata"] = view
			if canonicalCwd, ok := canonicalMetadataString(view.Fields["cwd"]); ok {
				out["cwd"] = canonicalCwd
			}
		} else {
			out["metadataUnavailable"] = true
		}
	} else {
		out["metadataUnavailable"] = true
	}
	if metadataDegraded {
		out["metadataDegraded"] = true
	}

	// The pane's info bar names the branch this session is working on. Cached
	// per directory and refreshed only when /state is rebuilt (live_git.go).
	if branch := liveGit.Branch(r.Context(), cwd); branch != "" {
		out["gitBranch"] = branch
	}

	if sess.provider == "claude" {
		if p := claudeTranscriptPath(cwd, threadID); p != "" {
			out["transcriptPath"] = p
		}
		out["claudeContext"] = claudeContext(cwd, initRaw)
		paths := []string{}
		for _, src := range claudeInstructionSources(cwd, initRaw) {
			paths = append(paths, src.Path)
		}
		out["instructionFiles"] = paths
		// Both are provider-generic now that claude has a control channel: the
		// pane's model switcher reads turnOptions, and the dialog cards read the
		// pending list — which must come from the snapshot, because the wire
		// buffer is bounded and a late-opened tab cannot replay its way to a
		// parked request. `controlInit` rides along in the state map above and
		// carries the model catalog the switcher renders.
		out["turnOptions"] = sess.turnOptionsPayload()
		out["pendingInteractions"] = sess.pendingInteractionList()
		httpapi.WriteJSON(w, out)
		return
	}
	if sess.provider == "grok" {
		out["turnOptions"] = sess.turnOptionsPayload()
		out["pendingInteractions"] = sess.pendingInteractionList()
		httpapi.WriteJSON(w, out)
		return
	}

	out["turnOptions"] = sess.turnOptionsPayload()
	// Server-owned pending interactions. The bounded wire buffer means a tab
	// opened after the request landed cannot find it by replay, so the snapshot
	// is the truth and the feed event is only the nudge (interactions.go).
	out["pendingInteractions"] = sess.pendingInteractionList()
	httpapi.WriteJSON(w, out)
}

// handleInterrupt stops the provider turn in flight through the adapter.
//
// The completion race is the whole design here. Between the operator reading
// "Working" and the request landing, the turn can finish; the id nano holds is
// then stale. Probed against the running app-server (2026-07-30): a stale
// turn/interrupt is met with **silence** — no result, no error — so the race
// surfaces as a deadline, not as an RPC error. Both are the same non-event to
// the operator, who asked for a turn to stop and got a stopped turn, so both
// degrade to a quiet 200 with interrupted:false. That raced result also settles
// the exact active claim and releases the next queued prompt. Only a broken
// daemon (a transport failure, not a missing answer) is worth a 502.
//
// A session whose event loop is wedged is the other 502: the request is bounded
// by coresession.InterruptHandoffTimeout and reports the failure instead of
// hanging (2026-09-21 incident, where a backlogged loop froze this endpoint for
// minutes).
func (sess *session) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	body, err := sess.interruptTurn(r.Context())
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	httpapi.WriteJSON(w, body)
}

func (sess *session) interruptTurn(ctx context.Context) (map[string]any, error) {
	sess.stateMu.Lock()
	active := sess.workActiveLocked()
	sess.stateMu.Unlock()
	threadID, turnID := sess.activeTurn()
	if !active {
		return map[string]any{"interrupted": false, "reason": "no turn in flight"}, nil
	}
	if err := sess.ensureProviderCoordinator(); err != nil {
		return nil, err
	}
	scope := sess.deliveryScope()
	if turnID != "" {
		scope.NativeTurnID = &turnID
		// Recorded before the provider hears of it, so the turn's end can
		// never be read as a prompt lost without a Stop.
		sess.stateMu.Lock()
		if sess.stoppedTurns == nil {
			sess.stoppedTurns = make(map[string]bool)
		}
		sess.stoppedTurns[turnID] = true
		sess.stateMu.Unlock()
	}
	result, err := sess.core.Interrupt(ctx, providers.InterruptRequest{Scope: scope})
	if errors.Is(err, coresession.ErrInterruptNotAccepted) {
		// The stop button must fail loudly rather than hang. Core kept the
		// interrupt at the front of the session queue, so it still reaches the
		// provider if that event ever returns — but nothing here may claim the
		// turn was interrupted.
		return nil, fmt.Errorf("the session is not processing events: the interrupt was not handed to the provider within %s", coresession.InterruptHandoffTimeout)
	}
	if err != nil {
		return nil, err
	}
	body := map[string]any{"interrupted": result.Interrupted}
	if result.Reason != "" {
		body["reason"] = result.Reason
	}
	if threadID != "" && turnID != "" {
		body["turnId"] = turnID
	}
	return body, nil
}

// interruptRaceReason classifies a failed turn/interrupt: an unanswered call
// (the app-server's way of saying "no such turn") or an RPC error both mean the
// turn was already gone. Anything else is a real failure worth reporting.
func interruptRaceReason(err error) (string, bool) {
	if errors.Is(err, context.DeadlineExceeded) {
		return "the turn had already finished", true
	}
	var rpcErr *protocol.RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Message, true
	}
	return "", false
}

func (sess *session) handleTurnOptionsRead(w http.ResponseWriter, _ *http.Request) {
	httpapi.WriteJSON(w, sess.turnOptionsPayload())
}

// Model and effort are next-turn overrides. Priority is a session setting:
// turn/start applies it to that turn and subsequent turns, while Nano also
// persists the choice immediately so a restart before the next prompt cannot
// forget it. A field absent from the body is left alone; an explicitly empty
// string clears its override back to the thread's own value.
func (sess *session) handleTurnOptionsUpdate(w http.ResponseWriter, r *http.Request) {
	if sess.provider == "claude" {
		sess.handleClaudeModelUpdate(w, r)
		return
	}
	if sess.provider == "grok" {
		httpapi.WriteHTTPError(w, "Grok model and effort are fixed when the native session starts", http.StatusNotImplemented)
		return
	}
	var req struct {
		Model       *string `json:"model"`
		Effort      *string `json:"effort"`
		ServiceTier *string `json:"serviceTier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.WriteHTTPError(w, "body must contain model, effort, or serviceTier", http.StatusBadRequest)
		return
	}
	serviceTier := ""
	if req.ServiceTier != nil {
		serviceTier = strings.ToLower(strings.TrimSpace(*req.ServiceTier))
		if serviceTier != "" && serviceTier != "default" && serviceTier != "priority" && serviceTier != "ultrafast" {
			httpapi.WriteHTTPError(w, "serviceTier must be default, priority, or ultrafast", http.StatusBadRequest)
			return
		}
	}
	priorModel, _, priorTier := sess.turnOptions()
	validatedUltrafast := false
	if req.Model != nil || req.ServiceTier != nil {
		model, _, tier := sess.turnOptions()
		if req.ServiceTier != nil {
			tier = serviceTier
		}
		if tier == "" {
			tier = sess.posture().ServiceTier
		}
		if tier == "ultrafast" {
			validatedUltrafast = true
			if model == "" {
				model = sess.posture().Model
			}
			if req.Model != nil {
				model = strings.TrimSpace(*req.Model)
				if model == "" {
					model = sess.model
				}
			}
			if err := sess.codex.validateUltrafast(model, tier); err != nil {
				httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
	}
	// Each override the request carried is one operator decision — an empty
	// value included, which records "back to the thread's own" as a clear.
	var decisions []postureEntry
	record := func(field, value string) {
		decisions = append(decisions, postureEntry{
			Field: field, Value: value,
			Source: postureSourceOperator, Evidence: postureEvidenceTurnOptions,
		})
	}
	sess.stateMu.Lock()
	if validatedUltrafast && (sess.turnModel != priorModel || sess.turnServiceTier != priorTier) {
		sess.stateMu.Unlock()
		httpapi.WriteHTTPError(w, "speed settings changed during capability refresh; try again", http.StatusConflict)
		return
	}
	if req.Model != nil {
		sess.turnModel = strings.TrimSpace(*req.Model)
		record(postureFieldModel, sess.turnModel)
	}
	if req.Effort != nil {
		sess.turnEffort = strings.TrimSpace(*req.Effort)
		record(postureFieldEffort, sess.turnEffort)
	}
	if req.ServiceTier != nil {
		sess.turnServiceTier = serviceTier
		record(postureFieldServiceTier, serviceTier)
	}
	sess.stateMu.Unlock()
	sess.recordPosture(decisions, metadataFreshnessLive)
	httpapi.WriteJSON(w, sess.turnOptionsPayload())
}

// handleClaudeModel is the claude arm of the same endpoint, and its semantics
// are deliberately different: codex's model is a per-turn override merged into
// the next turn/start, while claude's `set_model` changes the live session
// immediately — the child, its native session id and the whole conversation
// survive (probed 2026-07-30). The posture-preserving `--resume` respawn stays
// where it was, as the fallback rung for a session with no live transport, and
// is not reached from here.
//
// One surface, two truths, so the payload keeps the same field names: `model`
// is the override in force ("" = the model the session was spawned with, which
// `threadModel` names).
func (sess *session) handleClaudeModelUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model *string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.WriteHTTPError(w, "body must be {\"model\": \"…\"}", http.StatusBadRequest)
		return
	}
	if req.Model != nil {
		model := strings.TrimSpace(*req.Model)
		claude := sess.claudeHandle()
		if claude == nil {
			httpapi.WriteHTTPError(w, "session transport is still starting", http.StatusConflict)
			return
		}
		decision := postureEntry{Field: postureFieldModel, Value: model, Source: postureSourceOperator, Evidence: postureEvidenceSetModel}
		// Persist intent before delivery; the successful provider control reply
		// below is the causal confirmation rather than a local assignment.
		sess.recordPosture([]postureEntry{decision}, metadataFreshnessLive)
		snapshot := sess.snapshotProviderCausation(map[string]string{postureFieldModel: model})
		if err := claude.setModel(r.Context(), model); err != nil {
			httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
			return
		}
		sess.stateMu.Lock()
		sess.turnModel = model
		sess.stateMu.Unlock()
		sess.recordProviderEvidence("claude_set_model", map[string]string{postureFieldModel: model}, metadataFreshnessLive, snapshot)
	}
	httpapi.WriteJSON(w, sess.turnOptionsPayload())
}

// turnOptionsPayload is what the pane's controls render from: the overrides in
// force plus the thread's own model, so "no override" can be shown as the
// thread's actual model rather than as a blank.
func (sess *session) turnOptionsPayload() map[string]any {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return map[string]any{
		"model":       sess.turnModel,
		"effort":      sess.turnEffort,
		"serviceTier": sess.turnServiceTier,
		"summary":     sess.reasoningSummary,
		"threadModel": sess.model,
	}
}

// noteHumanPrompt records that a human submitted a prompt to this thread now.
// It is the only source of the "human typed recently" availability fact;
// agent-origin prompts never set it, even though they also land as user-role
// transcript items.
func (sess *session) noteHumanPrompt(at time.Time) {
	sess.stateMu.Lock()
	if at.After(sess.lastHumanPromptAt) {
		sess.lastHumanPromptAt = at
	}
	sess.stateMu.Unlock()
	if sess.agentExecutions != nil && sess.hostedThreadID != "" {
		if err := sess.agentExecutions.noteHumanPrompt(sess.hostedThreadID, at); err != nil {
			log.Printf("record human prompt time hosted=%s: %v", sess.hostedThreadID, err)
		}
	}
}

func (sess *session) humanPromptAt() time.Time {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.lastHumanPromptAt
}
