package app

import (
	"encoding/json"
	"time"

	"github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// sessionProjection is the live view consumed by one session. Projection state
// and its locks stay with the view; sessions supply ordered provider evidence.
type sessionProjection interface {
	// ApplyWireLive applies ordered provider evidence to the live view.
	ApplyWireLive(dir protocol.Direction, line []byte, at time.Time, live, replay bool, _ ...int64)
	// ClaudeResultTurn reads the Claude turn currently associated with a result.
	ClaudeResultTurn() string
	// CompleteTurnUnlessWorked applies a saved completion only if no later live work exists.
	CompleteTurnUnlessWorked(turnID string, version uint64, status string, durationMS, completedAtMs int64) bool
	// CurrentTurnID reads the current projected turn identity.
	CurrentTurnID() string
	// EnsureClaudeActiveTurn ensures Claude activity has a projected turn identity.
	EnsureClaudeActiveTurn(startedAtMs int64) string
	// FinalAgentItemID reads the final assistant item for a turn.
	FinalAgentItemID(turnID string) string
	// LatestTerminalTurnID reads the newest terminal turn identity.
	LatestTerminalTurnID() string
	// NudgeMetaRefresh publishes a view metadata refresh.
	NudgeMetaRefresh()
	// RecordDiagnostic records a bounded content-free thread diagnostic.
	RecordDiagnostic(diagnostic threadDiagnostic, once bool)
	// RemoveInteraction removes a resolved interaction from the view.
	RemoveInteraction(id string)
	// RetainedTurnEvidence reads a turn and its latest live evidence version.
	RetainedTurnEvidence(turnID string) (liveProjectionTurn, int64, uint64, bool)
	// SeedResumeTurns initializes the view from normalized resumed turns.
	SeedResumeTurns(id string, seedTurns []*liveProjectionTurn)
	// ProviderName reports the view’s provider.
	ProviderName() string
	// SetClaudeFrameTurnID sets the turn associated with the current Claude frame.
	SetClaudeFrameTurnID(id string)
	// SetClaudeProviderTurnID sets the provider identity of the current Claude turn.
	SetClaudeProviderTurnID(id string)
	// SetClaudeResultTurnID sets the turn associated with a Claude result.
	SetClaudeResultTurnID(id string)
	// SetNativeThreadID sets the native identity represented by the view.
	SetNativeThreadID(id string)
	// Status reads the current live-view status.
	Status() liveTextStatus
	// Subscribe subscribes before the caller reads its initial view snapshot.
	Subscribe() (ch chan liveProjectionUpdate, cancel func())
	// ThreadDiagnosticsSnapshot copies the bounded diagnostic list.
	ThreadDiagnosticsSnapshot() []threadDiagnostic
	// TurnList copies the retained projected turns.
	TurnList() []liveProjectionTurn
	// UpdateCompletedTurn records terminal evidence for a projected turn.
	UpdateCompletedTurn(turnID, status string, durationMS int64, completedAtMs int64, turnError ...json.RawMessage)
	// UpdateError records the latest live error.
	UpdateError(message string)
	// UpdateRuntime publishes the attachment’s runtime view.
	UpdateRuntime(runtime liveProjectionRuntime)
	// UpdateTokenUsage records current provider token usage.
	UpdateTokenUsage(raw json.RawMessage)
	// UpsertInteraction records a pending provider interaction.
	UpsertInteraction(interaction pendingInteraction)
	// DropsLiveTextDeltas reports whether live text delta publication is disabled.
	DropsLiveTextDeltas() bool
	// SetLiveText connects the observer of ordered parsed evidence.
	SetLiveText(observer liveTextObserver)
	// TurnActive reports whether the projected turn is active.
	TurnActive() bool
}

// resumeProjection accepts the normalized resume prefix, not provider wire data.
type resumeProjection interface {
	// ProviderName reports the view’s provider.
	ProviderName() string
	// SeedResumeTurns initializes the view from normalized resumed turns.
	SeedResumeTurns(string, []*liveProjectionTurn)
}

// liveTextObserver consumes the projection's ordered parsed evidence.
type liveTextObserver interface {
	// ObserveBatch consumes ordered parsed provider evidence.
	ObserveBatch(model.Batch, string)
	// TurnEnded observes completion of a provider turn.
	TurnEnded(string)
}
