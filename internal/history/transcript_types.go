package history

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

const (
	TranscriptSchemaVersion = 1
	TranscriptTaxonomyV1    = "transcript-categories-v1"

	DefaultTranscriptPageLimit = 40
	MaxTranscriptPageLimit     = 100
	MaxTranscriptPageBytes     = 256 << 10
)

type TranscriptCategory string

const (
	CategoryMessages     TranscriptCategory = "messages"
	CategoryReasoning    TranscriptCategory = "reasoning"
	CategoryToolCall     TranscriptCategory = "toolCall"
	CategoryToolResult   TranscriptCategory = "toolResult"
	CategorySubagent     TranscriptCategory = "subagent"
	CategorySystem       TranscriptCategory = "system"
	CategoryError        TranscriptCategory = "error"
	CategoryUnclassified TranscriptCategory = "unclassified"
)

var TranscriptCategories = []TranscriptCategory{
	CategoryMessages, CategoryReasoning, CategoryToolCall, CategoryToolResult,
	CategorySubagent, CategorySystem, CategoryError, CategoryUnclassified,
}

var (
	ErrTranscriptChanged             = errors.New("history transcript projection changed")
	ErrInvalidTranscriptPage         = errors.New("invalid history transcript page")
	ErrTranscriptConflict            = errors.New("history transcript source conflicts with canonical evidence")
	ErrTranscriptEvidenceUnavailable = errors.New("history transcript evidence unavailable")
	ErrTranscriptBodyUnavailable     = errors.New("history transcript retained body unavailable")
)

type TranscriptProjectionMode string

const (
	TranscriptAppend  TranscriptProjectionMode = "append"
	TranscriptRebuild TranscriptProjectionMode = "rebuild"
)

type TranscriptProjectionState string

const (
	TranscriptPending  TranscriptProjectionState = "pending"
	TranscriptReady    TranscriptProjectionState = "ready"
	TranscriptEmpty    TranscriptProjectionState = "empty"
	TranscriptDegraded TranscriptProjectionState = "degraded"
	TranscriptRetained TranscriptProjectionState = "retained"
)

type TranscriptItemType string

const (
	ItemUserMessage   TranscriptItemType = "user_message"
	ItemAgentMessage  TranscriptItemType = "agent_message"
	ItemInteraction   TranscriptItemType = "interaction"
	ItemReasoning     TranscriptItemType = "reasoning"
	ItemCommand       TranscriptItemType = "command"
	ItemToolCall      TranscriptItemType = "tool_call"
	ItemToolResult    TranscriptItemType = "tool_result"
	ItemFileChange    TranscriptItemType = "file_change"
	ItemPlan          TranscriptItemType = "plan"
	ItemCompaction    TranscriptItemType = "compaction"
	ItemNotice        TranscriptItemType = "notice"
	ItemProviderEvent TranscriptItemType = "provider_event"
)

type TranscriptItemStatus string

const (
	ItemStatusCompleted   TranscriptItemStatus = "completed"
	ItemStatusInProgress  TranscriptItemStatus = "in_progress"
	ItemStatusFailed      TranscriptItemStatus = "failed"
	ItemStatusInterrupted TranscriptItemStatus = "interrupted"
	ItemStatusUnknown     TranscriptItemStatus = "unknown"
)

type TranscriptTurnStatus string

const (
	TurnInProgress  TranscriptTurnStatus = "in_progress"
	TurnCompleted   TranscriptTurnStatus = "completed"
	TurnInterrupted TranscriptTurnStatus = "interrupted"
	TurnTruncated   TranscriptTurnStatus = "truncated"
	TurnUnknown     TranscriptTurnStatus = "unknown"
)

type TranscriptCompletionEvidence string

const (
	CompletionNone             TranscriptCompletionEvidence = "none"
	CompletionTaskComplete     TranscriptCompletionEvidence = "task_complete"
	CompletionTurnAborted      TranscriptCompletionEvidence = "turn_aborted"
	CompletionClaudeResult     TranscriptCompletionEvidence = "claude_result"
	CompletionTurnDuration     TranscriptCompletionEvidence = "turn_duration"
	CompletionStopReason       TranscriptCompletionEvidence = "stop_reason"
	CompletionSubsequentPrompt TranscriptCompletionEvidence = "subsequent_prompt"
	CompletionMaxTokens        TranscriptCompletionEvidence = "max_tokens"
)

type TranscriptRole string

const (
	RoleUser      TranscriptRole = "user"
	RoleAssistant TranscriptRole = "assistant"
	RoleSystem    TranscriptRole = "system"
	RoleTool      TranscriptRole = "tool"
)

type TranscriptBody struct {
	ItemID        string
	ContentRef    string
	Text          string
	JSON          json.RawMessage
	OriginalBytes int64
	StoredBytes   int64
	Truncated     bool
}

// TranscriptRetainedBody is Nano-owned normalized content. Unlike native
// evidence, it remains readable when the provider source has been cleaned up.
type TranscriptRetainedBody struct {
	ThreadID      string          `json:"threadId"`
	ItemID        string          `json:"itemId"`
	ProjectionKey string          `json:"projectionKey"`
	ContentRef    string          `json:"contentRef"`
	Text          string          `json:"text,omitempty"`
	JSON          json.RawMessage `json:"json,omitempty"`
	OriginalBytes int64           `json:"originalBytes"`
	StoredBytes   int64           `json:"storedBytes"`
	Truncated     bool            `json:"truncated"`
}

type TranscriptItemPatch struct {
	ItemID       string
	NativeItemID string
	Phase        string
	NativeFinal  bool
}

type TranscriptItem struct {
	ID                string               `json:"id"`
	ProjectedSeq      int64                `json:"projectedSeq"`
	SourceKey         string               `json:"-"`
	NativeItemID      string               `json:"nativeItemId,omitempty"`
	TurnKey           string               `json:"turnKey"`
	TurnOrdinal       int                  `json:"turnOrdinal"`
	Type              TranscriptItemType   `json:"type"`
	Category          TranscriptCategory   `json:"category"`
	Status            TranscriptItemStatus `json:"status"`
	Role              TranscriptRole       `json:"role,omitempty"`
	Text              string               `json:"text,omitempty"`
	ToolName          string               `json:"toolName,omitempty"`
	CallID            string               `json:"callId,omitempty"`
	Detail            json.RawMessage      `json:"detail,omitempty"`
	CreatedAt         *time.Time           `json:"createdAt,omitempty"`
	Phase             string               `json:"phase,omitempty"`
	FilterTags        []string             `json:"filterTags,omitempty"`
	Synthetic         bool                 `json:"synthetic,omitempty"`
	Sidechain         bool                 `json:"sidechain,omitempty"`
	IsFinalAnswer     bool                 `json:"isFinalAnswer,omitempty"`
	ContentRef        string               `json:"contentRef,omitempty"`
	ContentTruncated  bool                 `json:"contentTruncated,omitempty"`
	OriginalBytes     int64                `json:"originalBytes,omitempty"`
	SourceLine        int                  `json:"-"`
	SourceRowType     string               `json:"-"`
	SourcePayloadType string               `json:"-"`
	SourceLineSHA256  string               `json:"-"`
	EmissionIndex     int                  `json:"-"`
}

type TranscriptTurn struct {
	Key                string                       `json:"key"`
	NativeTurnID       string                       `json:"nativeTurnId,omitempty"`
	Ordinal            int                          `json:"ordinal"`
	StartSeq           int64                        `json:"startSeq"`
	EndSeq             int64                        `json:"endSeq"`
	CompletionSeq      int64                        `json:"completionSeq,omitempty"`
	Status             TranscriptTurnStatus         `json:"status"`
	CompletionEvidence TranscriptCompletionEvidence `json:"completionEvidence"`
	FinalItemID        string                       `json:"finalItemId,omitempty"`
	FinalitySource     string                       `json:"finalitySource,omitempty"`
	FinalityConfidence string                       `json:"finalityConfidence,omitempty"`
	StartedAt          *time.Time                   `json:"startedAt,omitempty"`
	CompletedAt        *time.Time                   `json:"completedAt,omitempty"`
	DurationMS         int64                        `json:"durationMs,omitempty"`
	Config             json.RawMessage              `json:"config,omitempty"`
	Usage              *TranscriptTokenUsage        `json:"usage,omitempty"`
	Model              string                       `json:"model,omitempty"`
	Effort             string                       `json:"effort,omitempty"`
	ServiceTier        string                       `json:"serviceTier,omitempty"`
}

// UsageContractPerTurnV1 is the documented per-turn envelope grain. Views expose
// envelope columns only when this value is stored on the turn.
const UsageContractPerTurnV1 = 1

// TranscriptTokenUsage is per-turn token accounting. Envelope fields are
// nullable: nil means the provider did not report that quantity. They are a
// shared shape, not equivalent units across providers. Cached is a legacy
// alias of CacheRead for Codex/Grok transcript JSON; Claude no longer folds
// cache-write into it. Total is provider-reported only and is never derived.
type TranscriptTokenUsage struct {
	Input         *int64          `json:"input,omitempty"`
	Output        *int64          `json:"output,omitempty"`
	Reasoning     *int64          `json:"reasoning,omitempty"`
	CacheRead     *int64          `json:"cacheRead,omitempty"`
	CacheWrite    *int64          `json:"cacheWrite,omitempty"`
	Cached        *int64          `json:"cached,omitempty"`
	Total         *int64          `json:"total,omitempty"`
	ContextWindow *int64          `json:"contextWindow,omitempty"`
	Native        json.RawMessage `json:"native,omitempty"`
	Contract      int             `json:"-"`
}

type TranscriptProjectionResult struct {
	ProjectionKey  string
	CompleteOffset int64
	// ReadOffset includes the unfinished row retained in module continuation.
	// Zero selects CompleteOffset for the older adapters.
	ReadOffset    int64
	CompleteLines int
	BytesRead     int64
	ParserState   json.RawMessage
	Items         []TranscriptItem
	Turns         []TranscriptTurn
	Bodies        []TranscriptBody
	ItemPatches   []TranscriptItemPatch
	Diagnostics   []Diagnostic
	Summary       DiagnosticSummary
	// The module bridge can replay a bounded spool into the guarded store
	// transaction. It does not reopen or parse the provider source there.
	parsedBatches func(func(model.Batch) error) error
	// recordSource enables legacy Dev links for this native file only.
	recordSource string
	// continuation, for an append resumed from a resident copy, replaces
	// ParserState with the changed entries; residentNext is the copy to keep
	// once the save commits.
	continuation *continuationDelta
	residentNext *residentContinuation
	// Grok's existing usage view displays current session model/effort on
	// historical turns. This consumer snapshot never enters model.Turn.
	sessionUsage *Metadata
}

type TranscriptCheckpoint struct {
	ProjectionKey         string
	ThreadID              string
	SourceID              int64
	SourceGeneration      int64
	Provider              Provider
	ParserVersion         string
	CompletionRuleVersion string
	CompleteOffset        int64
	ReadOffset            int64
	CompleteLines         int
	LatestSeq             int64
	ParserState           json.RawMessage
	ErrorSummary          *string
	// resident, from LoadTranscriptCheckpointForAppend, stands in for
	// ParserState: the decoded continuation this checkpoint saved.
	resident *residentContinuation
}

type TranscriptApplyRequest struct {
	Mode                  TranscriptProjectionMode
	ThreadID              string
	SourceID              int64
	SourceGeneration      int64
	Provider              Provider
	ParserVersion         string
	CompletionRuleVersion string
	ExpectedProjectionKey string
	// ExpectNoProjection rejects a concurrent first projection after file preparation.
	ExpectNoProjection bool
	IndexedAt          time.Time
	Result             TranscriptProjectionResult
}

type TranscriptApplyResult struct {
	ProjectionKey string
	LatestSeq     int64
	ItemCount     int64
	Changed       bool
}

type TranscriptReadRequest struct {
	ThreadID              string
	ExpectedProjectionKey string
	SnapshotHeadSeq       int64
	BeforeSeq             int64
	BeforeNativeTurnID    string
	BeforeTurnOrdinal     int
	TurnLimit             int
	Limit                 int
	MaxBytes              int
	HiddenCategories      []TranscriptCategory
	// MessagesOnly reads the rows transcriptMessageSQL defines, whatever
	// their category, instead of filtering by HiddenCategories: the default
	// Enhanced profile. It takes no hidden categories.
	MessagesOnly bool
}

// TranscriptMatch is one kind of exact native key: an alias namespace, which
// the catalog indexes only for native or exact-derived identity, and the row
// type that carries it. Only a top-level, non-sidechain row matches, and a key
// carried by more than one row matches nothing. MatchTurnItem names a turn
// lookup instead: the row of that type in the turn with that native turn id.
// MatchTurnNativeItem names the row of that type, in the native turn Turns
// gives for the key, whose native item id is the key. A Codex message an older
// CLI saved as an event without its id, then as the item with it, is one row
// on the event's line, while its id alias is on the item's line; only this
// lookup finds it. MatchTurnEvent names, in the native turn Turns gives for the
// key, the row one of whose observations carries the key as its native event
// id: a prompt Codex saved with its client id, possibly on the second of its
// two copies, in a turn several prompts share.
type TranscriptMatch struct {
	Namespace string
	ItemType  string
	Keys      []string
	Turns     map[string]string
	// Exclude gives a MatchTurnItem key the event ids of other prompts bound
	// to that turn: their rows are theirs, so the key matches the one row left.
	Exclude map[string][]string
}

const (
	MatchTurnItem       = "turn.item"
	MatchTurnNativeItem = "turn.nativeItem"
	MatchTurnEvent      = "turn.event"
)

type TranscriptEvidenceRef struct {
	// Byte ranges come from module observations. Legacy retained rows can use
	// their guarded line reference until their source is rebuilt.
	HasByteRange      bool
	ByteStart         int64
	ByteLength        int64
	ThreadID          string
	ItemID            string
	ProjectionKey     string
	Provider          Provider
	SourcePath        string
	SourceLine        int
	SourceRowType     string
	SourcePayloadType string
	SourceLineSHA256  string
	SourceGeneration  int64
	CurrentGeneration int64
	CompleteOffset    int64
	Device            int64
	Inode             int64
	SourceStatus      SourceStatus
}

type TranscriptProjectionInfo struct {
	Key                string                    `json:"key"`
	ParserVersion      string                    `json:"parserVersion,omitempty"`
	SchemaVersion      int                       `json:"schemaVersion"`
	State              TranscriptProjectionState `json:"state"`
	SourceStatus       SourceStatus              `json:"sourceStatus"`
	LastIndexedAt      *time.Time                `json:"lastIndexedAt,omitempty"`
	EvidenceAvailable  bool                      `json:"evidenceAvailable"`
	ItemCount          int64                     `json:"itemCount"`
	AliasCount         int64                     `json:"aliasCount,omitempty"`
	AliasConflictCount int64                     `json:"aliasConflictCount,omitempty"`
	ErrorSummary       *string                   `json:"errorSummary,omitempty"`
}

type TranscriptPage struct {
	Projection      TranscriptProjectionInfo `json:"projection"`
	Items           []TranscriptItem         `json:"items"`
	Turns           []TranscriptTurn         `json:"turns"`
	StartSeq        int64                    `json:"startSeq"`
	EndSeq          int64                    `json:"endSeq"`
	LatestSeq       int64                    `json:"latestSeq"`
	SnapshotHeadSeq int64                    `json:"snapshotHeadSeq"`
	HasMoreBefore   bool                     `json:"hasMoreBefore"`
	StartsMidTurn   bool                     `json:"startsMidTurn"`
}
