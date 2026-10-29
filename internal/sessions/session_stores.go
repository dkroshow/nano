package sessions

import (
	"context"
	"database/sql"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/threads"
)

// Metadata is the hosted authority a running session reads and updates.
// Storage, identity allocation, and their locks stay with the hosted owner.
type Metadata interface {
	// AppendEvent appends canonical hosted evidence.
	AppendEvent(event hostedstore.MetadataEvent) (hostedstore.MetadataEvent, error)
	// AppendPermissionEvent persists one provider permission observation.
	AppendPermissionEvent(event hostedstore.PermissionEvent) error
	// ClaimNativeID binds an unclaimed native identity to a hosted thread.
	ClaimNativeID(hostedID, provider, nativeID string, provisional bool) error
	// ClaimNativeIDResolving binds a native identity while resolving its provisional handle.
	ClaimNativeIDResolving(hostedID, provider, nativeID string, provisional bool, pair *hostedstore.NativeResolution) error
	// EventsAfter reads a bounded page of hosted evidence after a cursor.
	EventsAfter(hostedID string, after int64, limit int, filter hostedstore.MetadataEventFilter) (hostedstore.MetadataEventPage, error)
	// Posture reads the canonical display posture.
	Posture(hostedID string) hostedstore.Posture
	// ReadPool returns the canonical read connection pool.
	ReadPool() *sql.DB
	// ResolvedNative finds the current native identity for a provisional handle.
	ResolvedNative(ctx context.Context, hostedID, provider, handle string) (string, error)
	// View reads one canonical hosted metadata view.
	View(hostedID string) (hostedstore.MetadataView, error)
	// CanonicalDatabase returns the canonical writer connection.
	CanonicalDatabase() *sql.DB
}

// Executions records runtime evidence and prompt outcomes. Admission and
// execution-store mutation remain behind the owning store operations.
type Executions interface {
	// BindSession binds the current runtime attachment to its hosted record.
	BindSession(id, nanoSessionID, nativeThreadID, status, presentationMode string) error
	// FinishJoinedTurn settles an operation that joined an existing provider turn.
	FinishJoinedTurn(operationID, providerTurnID, outcome string, executionErr *threads.ExecutionError, response, itemID string, meta hostedstore.ResultMeta) error
	// FinishResolvedTurn settles an operation from its resolved provider outcome.
	FinishResolvedTurn(operationID, providerTurnID, outcome string, executionErr *threads.ExecutionError, response, itemID string) error
	// MarkResumedTurnsTerminal reconciles resumed terminal evidence with open operations.
	MarkResumedTurnsTerminal(hostedThreadID, provider, nativeThreadID string, turns []hostedstore.TerminalTurn, matches []hostedstore.ResumedTurnMatch) ([]bool, error)
	// MarkSessionTurns sets the outcome of the session’s open operations.
	MarkSessionTurns(sessionID, status string, executionErr *threads.ExecutionError) error
	// MarkTurnDelivery records a provider delivery outcome.
	MarkTurnDelivery(operationID, providerTurnID, status string, executionErr *threads.ExecutionError) error
	// MarkTurnDeliveryWith records delivery with a caller-supplied transactional update.
	MarkTurnDeliveryWith(operationID, providerTurnID, status string, executionErr *threads.ExecutionError, with func(*sql.Tx) error) error
	// MarkTurnTerminal records terminal provider evidence for matching operations.
	MarkTurnTerminal(provider, nativeThreadID, providerTurnID, status string, executionErr *threads.ExecutionError, response, itemID string, resultMeta ...hostedstore.ResultMeta) error
	// MarkWaiting records whether an operation awaits input.
	MarkWaiting(sessionID, providerTurnID string, waiting bool) error
	// NoteHumanPrompt records the latest human prompt time.
	NoteHumanPrompt(hostedThreadID string, at time.Time) error
	// ObserveCandidate records a possible final answer until terminal evidence arrives.
	ObserveCandidate(provider, nativeThreadID, providerTurnID, itemID, phase, response string) error
	// OpenTurnsForProviderTurn reads open operations bound to a provider turn.
	OpenTurnsForProviderTurn(hostedThreadID, provider, providerTurnID string) []threads.TurnRecord
	// PersistedTurnForTransportAction reads the persisted operation for a transport action.
	PersistedTurnForTransportAction(ctx context.Context, hostedID, transportActionID string) (threads.TurnRecord, bool, error)
	// ResumedTurnMatches matches resumed provider evidence to hosted operations.
	ResumedTurnMatches(hostedThreadID, provider, nativeThreadID string, turns []hostedstore.TerminalTurn) []hostedstore.ResumedTurnMatch
	// SessionNeedsAttention reports unresolved failures for a hosted thread.
	SessionNeedsAttention(hostedID string) bool
	// SharedTurnResult reads the shared saved answer and its size metadata.
	SharedTurnResult(hostedThreadID, provider, nativeThreadID, providerTurnID, finalItemID string) (string, hostedstore.ResultMeta, bool)
	// TurnForTransportAction finds an operation by its transport action.
	TurnForTransportAction(actionID string) (threads.TurnRecord, bool)
	// TurnsForHosted reads a hosted thread’s operation records.
	TurnsForHosted(hostedID string) []threads.TurnRecord
	// UpdateSession records the attachment’s current runtime state.
	UpdateSession(id, nanoSessionID, nativeThreadID, status, message string) error
	// ReadsSavedResults reports whether saved-result reads are configured.
	ReadsSavedResults() bool
}
