package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/threads"
)

const (
	presentationShared          = threads.PresentationShared
	presentationBackground      = threads.PresentationBackground
	sidebarPlacementInvoker     = threads.SidebarPlacementInvoker
	sidebarPlacementStandalone  = threads.SidebarPlacementStandalone
	agentSessionCreating        = threads.AgentSessionCreating
	agentSessionCreationUnknown = threads.AgentSessionCreationUnknown
	agentSessionDetached        = threads.AgentSessionDetached
	agentTurnQueued             = threads.AgentTurnQueued
	agentTurnDeliveryPending    = threads.AgentTurnDeliveryPending
	agentTurnDeliveryUnknown    = threads.AgentTurnDeliveryUnknown
	agentTurnRunning            = threads.AgentTurnRunning
	agentTurnWaiting            = threads.AgentTurnWaiting
	agentTurnAwaitingSaved      = threads.AgentTurnAwaitingSaved
	agentTurnCompleted          = threads.AgentTurnCompleted
	agentTurnFailed             = threads.AgentTurnFailed
	agentTurnInterrupted        = threads.AgentTurnInterrupted
	maxAgentResultBytes         = 8 << 20
	maxAgentTurns               = 100
)

// ErrClientSessionIdempotencyConflict and errAgentTurnIdempotencyConflict report reused client keys with different requests.
var (
	ErrClientSessionIdempotencyConflict = errors.New("client session idempotency conflict")
	ErrAgentTurnIdempotencyConflict     = errors.New("clientActionId was already used with a different prompt")
)

type agentExecutionError = threads.ExecutionError

type agentSessionRecord = threads.SessionRecord

type agentTurnRecord = threads.TurnRecord

// ExecutionStore is the small durable control-plane record shared by all
// hosted threads. It deliberately stores prompt hashes, never prompt text.
// Final responses are provider output and are bounded explicitly at 8 MiB.
type ExecutionStore struct {
	mu                 sync.RWMutex
	runID              string
	sessions           map[string]agentSessionRecord
	turns              map[string]agentTurnRecord
	turnsByHosted      map[string]map[string]bool
	dirtyTurnThreads   map[string]bool
	dirtyTurns         map[string]bool
	attentionDirty     map[string]bool
	attentionCache     map[string]bool
	byAction           map[string]string
	byClient           map[string]string
	deletedSessions    map[string]bool
	deletedTurns       map[string]bool
	sessionBaseline    map[string]int64
	turnBaseline       map[string]int64
	pendingTransitions []AgentOperationTransition
	changed            chan struct{}
	// changes is the in-memory semantic-transition ring behind the
	// turn-operations changes feed (agent_execution_changes.go). Loss-tolerant
	// by design; the durable records remain the truth.
	changes *agentChangeLog
	// control owns every durable mutation in its SQLite execution snapshot.
	control *MetadataStore
	// pruneEligible is the prompt owner's answer to which surplus terminal
	// operations' prompts can never owe a member completion, so their evidence
	// may go. Until it is set, retention keeps every operation.
	pruneEligible       func(ctx context.Context, hostedID string, transportActionIDs []string) (map[string]bool, error)
	savedResultRead     func(context.Context, agentTurnRecord) (string, string, error)
	savedResultJobs     map[string]*savedResultJob
	savedResultFinished func(string)
}

// pruneEligibleReadTimeout bounds each prompt-owner eligibility read made
// while retention selects what to prune, and maxPruneEligibleBatch, the prompt
// owner's own bound, the operations one read asks about.
const (
	pruneEligibleReadTimeout = 2 * time.Second
	maxPruneEligibleBatch    = 32
)
