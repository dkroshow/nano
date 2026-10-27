// Extracted from internal/grokacp/projector.go at 115207e.
// Native decoding stays shared by file and wire readers.
package grok

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MaxUpdateBytes bounds the size of a raw envelope decodeUpdate will
// decode. Envelopes larger than this are rejected before any JSON parsing
// is attempted.
const MaxUpdateBytes = 1 << 20 // 1 MiB

// ErrUpdateTooLarge is returned when raw exceeds MaxUpdateBytes.
var ErrUpdateTooLarge = errors.New("grok: update exceeds MaxUpdateBytes")

// Kind classifies a projected Update.
type updateKind string

const (
	kindUserMessage        updateKind = "userMessage"
	kindAgentMessage       updateKind = "agentMessage"
	kindReasoning          updateKind = "reasoning"
	kindToolCall           updateKind = "toolCall"
	kindToolUpdate         updateKind = "toolUpdate"
	kindPlan               updateKind = "plan"
	kindTask               updateKind = "task"
	kindTerminal           updateKind = "terminal"
	kindSubagentSpawn      updateKind = "subagentSpawn"
	kindSubagentProgress   updateKind = "subagentProgress"
	kindSubagentFinish     updateKind = "subagentFinish"
	kindSubagentDiagnostic updateKind = "subagentDiagnostic"
	kindHook               updateKind = "hook"
	kindMetadata           updateKind = "metadata"
	kindHiddenUser         updateKind = "hiddenUser"
	kindUnclassified       updateKind = "unclassified"
)

// decodedSubagentActivity is the bounded parent-side coordination evidence Grok
// emits. It deliberately excludes child prompt and output bodies; those stay
// in the raw event feed and the child's own native transcript.
type decodedSubagentActivity struct {
	ChildSessionID, ParentSessionID, ParentPromptID           string
	Description, Status, Model, ContextSource, CapabilityMode string
	DurationMS, ToolCalls, Turns, TokensUsed                  int64
	ContextPercentage                                         float64
}

// isSyntheticPromptID identifies Grok's provider-generated continuation turns.
// They can carry useful parent output, but they are not delivery identity for
// an operator submission and must not claim an unresolved operator turn.
func isSyntheticPromptID(id string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(id)), "subagent-completed-")
}

// IsSyntheticPromptID reports provider-generated continuation identity. Such
// an identity cannot claim an operator submission in a consumer's owner store.
func IsSyntheticPromptID(id string) bool { return isSyntheticPromptID(id) }

// Update is the normalized projection of one Grok ACP session/update
// envelope. Every byte slice field is an independent copy of the input.
type decodedUpdate struct {
	IsReplay         bool
	AgentTimestampMS *int64
	FileTimestamp    *int64
	Kind             updateKind
	SessionID        string
	PromptID         string
	EventID          string
	NativeID         string
	Text             string
	Delta            string
	Done             bool
	Status           string
	Title            string
	ToolName         string
	ToolKind         string
	CallID           string
	AtMs             int64
	Usage            json.RawMessage
	Payload          json.RawMessage
	Raw              json.RawMessage
	Subagent         *decodedSubagentActivity
}

// envelope is the outer JSON-RPC-shaped frame shared by live notifications
// and durable updates.jsonl rows. Durable rows add "timestamp" (unix
// seconds); live notifications omit it.
type envelope struct {
	Timestamp *int64          `json:"timestamp"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
}

type envelopeParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
	Meta      *paramsMeta     `json:"_meta"`
}

type paramsMeta struct {
	IsReplay         bool              `json:"isReplay"`
	EventID          string            `json:"eventId"`
	PromptID         string            `json:"promptId"`
	AgentTimestampMs *int64            `json:"agentTimestampMs"`
	UpdateParams     *updateParamsMeta `json:"updateParams"`
}

type updateParamsMeta struct {
	Status *string `json:"status"`
}

// updateBody is the discriminated "update" object. Content is kept as
// json.RawMessage because its shape varies by discriminant (a single
// {"type":"text","text":...} block for message/thought chunks, an array of
// blocks for tool_call_update).
type updateBody struct {
	SessionUpdate     string          `json:"sessionUpdate"`
	Content           json.RawMessage `json:"content"`
	ToolCallID        string          `json:"toolCallId"`
	Title             string          `json:"title"`
	Kind              string          `json:"kind"`
	Status            *string         `json:"status"`
	PromptID          string          `json:"prompt_id"`
	StopReason        string          `json:"stop_reason"`
	Usage             json.RawMessage `json:"usage"`
	Meta              *updateMeta     `json:"_meta"`
	SubagentID        string          `json:"subagent_id"`
	ChildSessionID    string          `json:"child_session_id"`
	ParentSessionID   string          `json:"parent_session_id"`
	ParentPromptID    string          `json:"parent_prompt_id"`
	Description       string          `json:"description"`
	Model             string          `json:"model"`
	ContextSource     string          `json:"effective_context_source"`
	CapabilityMode    string          `json:"capability_mode"`
	DurationMS        int64           `json:"duration_ms"`
	ToolCalls         int64           `json:"tool_calls"`
	Turns             int64           `json:"turns"`
	TokensUsed        int64           `json:"tokens_used"`
	ContextPercentage float64         `json:"context_percentage"`
}

type updateMeta struct {
	XAITool            *xaiTool `json:"x.ai/tool"`
	HideFromScrollback bool     `json:"hideFromScrollback"`
}

type xaiTool struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

const (
	methodSessionUpdate    = "session/update"
	methodXAISessionUpdate = "_x.ai/session/update"
	methodXAISessionNotify = "_x.ai/session_notification"
)

// decodeUpdate projects one raw Grok ACP envelope into zero or one
// normalized Updates. Unrelated methods return (nil, nil). An unknown
// sessionUpdate discriminant returns exactly one kindUnclassified Update
// rather than an error. Malformed JSON and oversized input return a
// descriptive error.
func decodeUpdate(raw []byte) ([]decodedUpdate, error) {
	return decodeUpdateLimit(raw, MaxUpdateBytes)
}

func decodeUpdateLimit(raw []byte, limit int) ([]decodedUpdate, error) {
	if len(raw) > limit {
		return nil, ErrUpdateTooLarge
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("grok: decode envelope: %w", err)
	}

	if env.Method != methodSessionUpdate && env.Method != methodXAISessionUpdate && env.Method != methodXAISessionNotify {
		return nil, nil
	}

	if len(env.Params) == 0 {
		return nil, fmt.Errorf("grok: %s envelope missing params", env.Method)
	}

	var params envelopeParams
	if err := json.Unmarshal(env.Params, &params); err != nil {
		return nil, fmt.Errorf("grok: decode params: %w", err)
	}

	if len(params.Update) == 0 {
		return nil, fmt.Errorf("grok: %s envelope missing params.update", env.Method)
	}

	var body updateBody
	if err := json.Unmarshal(params.Update, &body); err != nil {
		return nil, fmt.Errorf("grok: decode update: %w", err)
	}

	u := decodedUpdate{
		SessionID: params.SessionID,
		EventID:   metaEventID(params.Meta),
		PromptID:  metaPromptID(params.Meta),
		AtMs:      resolveAtMs(params.Meta, env.Timestamp),
		CallID:    body.ToolCallID,
		Title:     body.Title,
	}
	u.FileTimestamp = env.Timestamp
	if params.Meta != nil {
		u.AgentTimestampMS = params.Meta.AgentTimestampMs
		u.IsReplay = params.Meta.IsReplay
	}

	switch body.SessionUpdate {
	case "user_message_chunk":
		if body.Meta != nil && body.Meta.HideFromScrollback {
			u.Kind = kindHiddenUser
		} else {
			u.Kind = kindUserMessage
		}
		u.Text = extractText(body.Content)
		u.Delta = u.Text
	case "agent_message_chunk":
		u.Kind = kindAgentMessage
		u.Text = extractText(body.Content)
		u.Delta = u.Text
	case "agent_thought_chunk":
		u.Kind = kindReasoning
		u.Text = extractText(body.Content)
		u.Delta = u.Text
	case "tool_call":
		u.Kind = kindToolCall
		u.ToolName, u.ToolKind = toolIdentity(body)
		u.Status = toolStatus(body, params.Meta)
	case "tool_call_update":
		u.Kind = kindToolUpdate
		u.ToolName, u.ToolKind = toolIdentity(body)
		u.Status = toolStatus(body, params.Meta)
	case "task_completed", "task_backgrounded":
		u.Kind = kindTask
	case "plan":
		u.Kind = kindPlan
		u.Text = extractText(body.Content)
	case "current_mode_update", "retry_state":
		u.Kind = kindMetadata
	case "turn_completed":
		u.Kind = kindTerminal
		u.Done = true
		if body.PromptID != "" {
			u.PromptID = body.PromptID
		}
		u.Status = body.StopReason
		u.Usage = copyBytes(body.Usage)
	case "subagent_spawned", "subagent_progress", "subagent_finished":
		switch body.SessionUpdate {
		case "subagent_spawned":
			u.Kind = kindSubagentSpawn
		case "subagent_progress":
			u.Kind = kindSubagentProgress
		default:
			u.Kind = kindSubagentFinish
		}
		u.Subagent = subagentActivity(body)
	default:
		if len(body.SessionUpdate) >= len("subagent_") && body.SessionUpdate[:len("subagent_")] == "subagent_" {
			u.Kind = kindSubagentDiagnostic
			u.Subagent = subagentActivity(body)
		} else if body.SessionUpdate == "hook_execution" {
			u.Kind = kindHook
		} else {
			u.Kind = kindUnclassified
		}
	}

	u.NativeID = nativeID(u.CallID, u.PromptID, u.EventID, u.Kind)
	u.Payload = copyBytes(params.Update)
	u.Raw = copyBytes(raw)

	return []decodedUpdate{u}, nil
}

func subagentActivity(body updateBody) *decodedSubagentActivity {
	return &decodedSubagentActivity{
		ChildSessionID: firstNonEmpty(body.ChildSessionID, body.SubagentID), ParentSessionID: body.ParentSessionID,
		ParentPromptID: body.ParentPromptID, Description: body.Description, Status: body.StatusString(),
		Model: body.Model, ContextSource: body.ContextSource, CapabilityMode: body.CapabilityMode,
		DurationMS: body.DurationMS, ToolCalls: body.ToolCalls, Turns: body.Turns, TokensUsed: body.TokensUsed,
		ContextPercentage: body.ContextPercentage,
	}
}

func (b updateBody) StatusString() string {
	if b.Status == nil {
		return ""
	}
	return *b.Status
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func metaEventID(m *paramsMeta) string {
	if m == nil {
		return ""
	}
	return m.EventID
}

func metaPromptID(m *paramsMeta) string {
	if m == nil {
		return ""
	}
	return m.PromptID
}

func resolveAtMs(m *paramsMeta, durableTimestamp *int64) int64 {
	if m != nil && m.AgentTimestampMs != nil {
		return *m.AgentTimestampMs
	}
	if durableTimestamp != nil {
		return *durableTimestamp * 1000
	}
	return 0
}

// extractText pulls {"type":"text","text":"..."} content out of a raw
// content value. Content shapes that are not a single text block (arrays,
// diffs, absent) yield "" rather than an error.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return ""
	}
	if block.Type != "text" {
		return ""
	}
	return block.Text
}

// toolIdentity reads the machine tool name/kind carried in the update's own
// nested x.ai/tool metadata, preferring the update's top-level "kind" field
// (present on tool_call_update) when set.
func toolIdentity(body updateBody) (name, kind string) {
	if body.Meta != nil && body.Meta.XAITool != nil {
		name = body.Meta.XAITool.Name
		kind = body.Meta.XAITool.Kind
	}
	if body.Kind != "" {
		kind = body.Kind
	}
	return name, kind
}

// toolStatus prefers the update's own top-level status (tool_call_update)
// and falls back to the accompanying params._meta.updateParams.status
// (tool_call's initial Pending status has no top-level field).
func toolStatus(body updateBody, m *paramsMeta) string {
	if body.Status != nil {
		return *body.Status
	}
	if m != nil && m.UpdateParams != nil && m.UpdateParams.Status != nil {
		return *m.UpdateParams.Status
	}
	return ""
}

// nativeID derives a stable identity with no clock or random component:
// toolCallId for tool evidence, else promptId+kind, else eventId+kind.
func nativeID(callID, promptID, eventID string, kind updateKind) string {
	switch {
	case callID != "":
		return callID
	case kind == kindUnclassified && eventID != "":
		return eventID + "/" + string(kind)
	case promptID != "":
		return promptID + "/" + string(kind)
	default:
		return eventID + "/" + string(kind)
	}
}

func copyBytes(src json.RawMessage) json.RawMessage {
	if src == nil {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}
