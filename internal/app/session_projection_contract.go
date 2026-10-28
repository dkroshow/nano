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
	applyWireLive(dir protocol.Direction, line []byte, at time.Time, live, replay bool, _ ...int64)
	claudeResultTurn() string
	completeTurnUnlessWorked(turnID string, version uint64, status string, durationMS, completedAtMs int64) bool
	currentTurnID() string
	ensureClaudeActiveTurn(startedAtMs int64) string
	finalAgentItemID(turnID string) string
	latestTerminalTurnID() string
	nudgeMetaRefresh()
	recordDiagnostic(diagnostic threadDiagnostic, once bool)
	removeInteraction(id string)
	retainedTurnEvidence(turnID string) (liveProjectionTurn, int64, uint64, bool)
	seedResumeTurns(id string, seedTurns []*liveProjectionTurn)
	providerName() string
	setClaudeFrameTurnID(id string)
	setClaudeProviderTurnID(id string)
	setClaudeResultTurnID(id string)
	setNativeThreadID(id string)
	status() liveTextStatus
	subscribe() (ch chan liveProjectionUpdate, cancel func())
	threadDiagnosticsSnapshot() []threadDiagnostic
	turnList() []liveProjectionTurn
	updateCompletedTurn(turnID, status string, durationMS int64, completedAtMs int64, turnError ...json.RawMessage)
	updateError(message string)
	updateRuntime(runtime liveProjectionRuntime)
	updateTokenUsage(raw json.RawMessage)
	upsertInteraction(interaction pendingInteraction)
	dropsLiveTextDeltas() bool
	setLiveText(observer liveTextObserver)
	turnActive() bool
}

// resumeProjection accepts the normalized resume prefix, not provider wire data.
type resumeProjection interface {
	providerName() string
	seedResumeTurns(string, []*liveProjectionTurn)
}

// liveTextObserver consumes the projection's ordered parsed evidence.
type liveTextObserver interface {
	observeBatch(model.Batch, string)
	turnEnded(string)
}
