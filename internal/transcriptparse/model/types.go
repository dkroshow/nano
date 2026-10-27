// The base item, turn and token fields are extracted from history/transcript_types.go.
package model

import (
	"encoding/json"
	"time"
)

type Category string

const (
	CategoryMessages     Category = "messages"
	CategoryReasoning    Category = "reasoning"
	CategoryToolCall     Category = "toolCall"
	CategoryToolResult   Category = "toolResult"
	CategorySubagent     Category = "subagent"
	CategorySystem       Category = "system"
	CategoryError        Category = "error"
	CategoryUnclassified Category = "unclassified"
)

type ItemType string

const (
	ItemUserMessage   ItemType = "user_message"
	ItemAgentMessage  ItemType = "agent_message"
	ItemInteraction   ItemType = "interaction"
	ItemReasoning     ItemType = "reasoning"
	ItemCommand       ItemType = "command"
	ItemToolCall      ItemType = "tool_call"
	ItemToolResult    ItemType = "tool_result"
	ItemFileChange    ItemType = "file_change"
	ItemPlan          ItemType = "plan"
	ItemCompaction    ItemType = "compaction"
	ItemNotice        ItemType = "notice"
	ItemProviderEvent ItemType = "provider_event"
)

type ItemStatus string

const (
	ItemStatusCompleted   ItemStatus = "completed"
	ItemStatusInProgress  ItemStatus = "in_progress"
	ItemStatusFailed      ItemStatus = "failed"
	ItemStatusInterrupted ItemStatus = "interrupted"
	ItemStatusUnknown     ItemStatus = "unknown"
)

type TurnStatus string

const (
	TurnInProgress  TurnStatus = "in_progress"
	TurnCompleted   TurnStatus = "completed"
	TurnInterrupted TurnStatus = "interrupted"
	TurnTruncated   TurnStatus = "truncated"
	TurnUnknown     TurnStatus = "unknown"
)

type CompletionEvidence string

const (
	CompletionNone             CompletionEvidence = "none"
	CompletionTaskComplete     CompletionEvidence = "task_complete"
	CompletionTurnAborted      CompletionEvidence = "turn_aborted"
	CompletionClaudeResult     CompletionEvidence = "claude_result"
	CompletionTurnDuration     CompletionEvidence = "turn_duration"
	CompletionStopReason       CompletionEvidence = "stop_reason"
	CompletionSubsequentPrompt CompletionEvidence = "subsequent_prompt"
	CompletionMaxTokens        CompletionEvidence = "max_tokens"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

// Item is a logical content or action facet. SourceSeq records its first
// appearance within one source incarnation; upserts retain that position.
type Item struct {
	Scope        Scope             `json:"scope"`
	Aliases      []Alias           `json:"aliases,omitempty"`
	Observations []Observation     `json:"observations,omitempty"`
	Parts        []Part            `json:"parts,omitempty"`
	Times        []TimeObservation `json:"times,omitempty"`
	Capture      CaptureState      `json:"capture"`
	Outcome      Outcome           `json:"outcome"`
	Complete     bool              `json:"complete"`
	Finality     Finality          `json:"finality"`
	Relations    []Relation        `json:"relations,omitempty"`

	ID                string          `json:"id"`
	SourceSeq         int64           `json:"sourceSeq"`
	SourceKey         string          `json:"sourceKey,omitempty"`
	NativeItemID      string          `json:"nativeItemId,omitempty"`
	TurnKey           string          `json:"turnKey"`
	TurnOrdinal       int             `json:"turnOrdinal"`
	Type              ItemType        `json:"type"`
	Category          Category        `json:"category"`
	Status            ItemStatus      `json:"status"`
	Role              Role            `json:"role,omitempty"`
	Text              string          `json:"text,omitempty"`
	ToolName          string          `json:"toolName,omitempty"`
	CallID            string          `json:"callId,omitempty"`
	Detail            json.RawMessage `json:"detail,omitempty"`
	CreatedAt         *time.Time      `json:"createdAt,omitempty"`
	Phase             string          `json:"phase,omitempty"`
	FilterTags        []string        `json:"filterTags,omitempty"`
	Synthetic         bool            `json:"synthetic,omitempty"`
	Sidechain         bool            `json:"sidechain,omitempty"`
	IsFinalAnswer     bool            `json:"isFinalAnswer,omitempty"`
	ContentRef        string          `json:"contentRef,omitempty"`
	ContentTruncated  bool            `json:"contentTruncated,omitempty"`
	OriginalBytes     int64           `json:"originalBytes,omitempty"`
	SourceLine        int             `json:"sourceLine,omitempty"`
	SourceRowType     string          `json:"sourceRowType,omitempty"`
	SourcePayloadType string          `json:"sourcePayloadType,omitempty"`
	SourceLineSHA256  string          `json:"sourceLineSHA256,omitempty"`
	EmissionIndex     int             `json:"emissionIndex,omitempty"`
}

type Turn struct {
	Capture           CaptureState               `json:"capture"`
	Outcome           Outcome                    `json:"outcome"`
	Scope             Scope                      `json:"scope"`
	Observations      []Observation              `json:"observations,omitempty"`
	Completion        []CompletionFact           `json:"completion,omitempty"`
	FinalItemRefs     []string                   `json:"finalItemRefs,omitempty"`
	Times             []TimeObservation          `json:"times,omitempty"`
	Durations         []Duration                 `json:"durations,omitempty"`
	UsageObservations []UsageObservation         `json:"usageObservations,omitempty"`
	UsageBasis        string                     `json:"usageBasis,omitempty"`
	UsageRefs         []string                   `json:"usageRefs,omitempty"`
	UsageCoverage     map[string]CounterCoverage `json:"usageCoverage,omitempty"`
	UsageIncomplete   bool                       `json:"usageIncomplete,omitempty"`

	Key                string             `json:"key"`
	NativeTurnID       string             `json:"nativeTurnId,omitempty"`
	Ordinal            int                `json:"ordinal"`
	StartSourceSeq     int64              `json:"startSourceSeq"`
	EndSourceSeq       int64              `json:"endSourceSeq"`
	Status             TurnStatus         `json:"status"`
	CompletionEvidence CompletionEvidence `json:"completionEvidence"`
	FinalItemID        string             `json:"finalItemId,omitempty"`
	StartedAt          *time.Time         `json:"startedAt,omitempty"`
	CompletedAt        *time.Time         `json:"completedAt,omitempty"`
	DurationMS         int64              `json:"durationMs,omitempty"`
	Config             json.RawMessage    `json:"config,omitempty"`
	Usage              *TokenUsage        `json:"usage,omitempty"`
	Model              string             `json:"model,omitempty"`
	Effort             string             `json:"effort,omitempty"`
	ServiceTier        string             `json:"serviceTier,omitempty"`
}

// UsageContractPerTurnV1 is the documented per-turn envelope grain. Views expose
// envelope columns only when this value is stored on the turn.
const UsageContractPerTurnV1 = 1

// TokenUsage holds provider counters at the containing observation's grain.
// On Turn.Usage it is a per-turn summary. Envelope fields are
// nullable: nil means the provider did not report that quantity. They are a
// shared shape, not equivalent units across providers. Cached is a legacy
// alias of CacheRead for Codex/Grok transcript JSON; Claude no longer folds
// cache-write into it. Total is provider-reported only and is never derived.
type TokenUsage struct {
	Input         *int64          `json:"input,omitempty"`
	Output        *int64          `json:"output,omitempty"`
	Reasoning     *int64          `json:"reasoning,omitempty"`
	CacheRead     *int64          `json:"cacheRead,omitempty"`
	CacheWrite    *int64          `json:"cacheWrite,omitempty"`
	Cached        *int64          `json:"cached,omitempty"`
	Total         *int64          `json:"total,omitempty"`
	ContextWindow *int64          `json:"contextWindow,omitempty"`
	Native        json.RawMessage `json:"native,omitempty"`
	Contract      int             `json:"contract,omitempty"`
}
