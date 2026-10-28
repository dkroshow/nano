package app

import (
	"context"
	"database/sql"
	"time"
)

// sessionMetadata is the hosted authority a running session reads and updates.
// Storage, identity allocation, and their locks stay with the hosted owner.
type sessionMetadata interface {
	appendEvent(event metadataEvent) (metadataEvent, error)
	appendPermissionEvent(event permissionEvent) error
	claimNativeID(hostedID, provider, nativeID string, provisional bool) error
	claimNativeIDResolving(hostedID, provider, nativeID string, provisional bool, pair *nativeResolution) error
	eventsAfter(hostedID string, after int64, limit int, filter metadataEventFilter) (metadataEventPage, error)
	posture(hostedID string) sessionPosture
	readPool() *sql.DB
	resolvedNative(ctx context.Context, hostedID, provider, handle string) (string, error)
	view(hostedID string) (hostedMetadataView, error)
	canonicalDatabase() *sql.DB
}

// sessionExecutions records runtime evidence and prompt outcomes. Admission and
// execution-store mutation remain behind the owning store operations.
type sessionExecutions interface {
	bindSession(id, nanoSessionID, nativeThreadID, status, presentationMode string) error
	finishJoinedTurn(operationID, providerTurnID, outcome string, executionErr *agentExecutionError, response, itemID string, meta agentResultMeta) error
	finishResolvedTurn(operationID, providerTurnID, outcome string, executionErr *agentExecutionError, response, itemID string) error
	markResumedTurnsTerminal(hostedThreadID, provider, nativeThreadID string, turns []agentTerminalTurn, matches []agentResumedTurnMatch) ([]bool, error)
	markSessionTurns(sessionID, status string, executionErr *agentExecutionError) error
	markTurnDelivery(operationID, providerTurnID, status string, executionErr *agentExecutionError) error
	markTurnDeliveryWith(operationID, providerTurnID, status string, executionErr *agentExecutionError, with func(*sql.Tx) error) error
	markTurnTerminal(provider, nativeThreadID, providerTurnID, status string, executionErr *agentExecutionError, response, itemID string, resultMeta ...agentResultMeta) error
	markWaiting(sessionID, providerTurnID string, waiting bool) error
	noteHumanPrompt(hostedThreadID string, at time.Time) error
	observeCandidate(provider, nativeThreadID, providerTurnID, itemID, phase, response string) error
	openTurnsForProviderTurn(hostedThreadID, provider, providerTurnID string) []agentTurnRecord
	persistedTurnForTransportAction(ctx context.Context, hostedID, transportActionID string) (agentTurnRecord, bool, error)
	resumedTurnMatches(hostedThreadID, provider, nativeThreadID string, turns []agentTerminalTurn) []agentResumedTurnMatch
	sessionNeedsAttention(hostedID string) bool
	sharedTurnResult(hostedThreadID, provider, nativeThreadID, providerTurnID, finalItemID string) (string, agentResultMeta, bool)
	turnForTransportAction(actionID string) (agentTurnRecord, bool)
	turnsForHosted(hostedID string) []agentTurnRecord
	updateSession(id, nanoSessionID, nativeThreadID, status, message string) error
	readsSavedResults() bool
}
