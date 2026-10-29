package sessions

import (
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"

	"github.com/dkroshow/nano/internal/observability"
)

// Session owns one live provider attachment and its ordered runtime state.
type Session struct {
	id                    string
	provider              string
	hostedThreadID        string
	invokerHostedThreadID string
	invokerNativeThreadID string
	invokerProvider       string
	createdAt             time.Time
	// processStarted is the immutable application start supplied at construction.
	processStarted time.Time
	// spawnMode is how this session came into being — "start" (a thread that
	// did not exist until now) or "resume" (attaching to one that does). Written
	// once at spawn and never mutated, because it describes the request, not the
	// state: it rides the summary so a surface showing "starting" can say which
	// kind of starting it is (creating vs attaching) instead of guessing from an
	// absent thread id.
	spawnMode       string
	agentExecutions Executions
	metadata        Metadata

	wire Wire

	delivery Delivery

	stateOwner State

	runtime Runtime

	lifecycle Lifecycle

	view View
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
	ThreadDiagnostics []observability.ThreadDiagnostic `json:"threadDiagnostics,omitempty"`
	// LastTurnEndedAt is the server wall-clock instant at which the most recent
	// hosted turn ended. It seeds the client's completed stack on reload.
	LastTurnEndedAt string `json:"lastTurnEndedAt,omitempty"`
	CreatedAt       string `json:"createdAt"`
	// Starred is the operator pin overlay, stamped at list time so a
	// pending sidebar row can join the Starred section before its catalog row.
	Starred bool `json:"starred,omitempty"`
	// Posture and Metadata both come from control.sqlite for hosted threads.
	Posture             hostedstore.Posture       `json:"posture"`
	Metadata            *hostedstore.MetadataView `json:"metadata,omitempty"`
	MetadataUnavailable bool                      `json:"metadataUnavailable,omitempty"`
	MetadataDegraded    bool                      `json:"metadataDegraded,omitempty"`
	// FileTranscript is always true: every provider serves pane rows and live
	// text through its file transcript stream (live_text.go).
	FileTranscript bool `json:"fileTranscript"`
}
