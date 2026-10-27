package grok

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MaxFrameBytes bounds one ACP frame before JSON decoding.
const MaxFrameBytes = 1 << 20

// ErrFrameTooLarge reports an ACP frame that exceeds MaxFrameBytes.
var ErrFrameTooLarge = errors.New("grok: update exceeds MaxUpdateBytes")

// Frame is one decoded ACP JSON-RPC envelope.
type Frame struct {
	Timestamp *int64
	Method    string
	Params    Params
}

// Params is the session/update payload carried by a Grok ACP frame.
type Params struct {
	SessionID string
	Update    json.RawMessage
	Meta      *Meta
}

// Meta is Grok's transport metadata for one session update.
type Meta struct {
	IsReplay         bool
	EventID          string
	PromptID         string
	AgentTimestampMs *int64
	UpdateParams     *UpdateParams
}

// UpdateParams carries transport status metadata for one update.
type UpdateParams struct {
	Status *string
}

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

// DecodeFrame decodes one live or persisted ACP envelope without interpreting its update body.
func DecodeFrame(raw []byte, limit int) (Frame, bool, error) {
	if len(raw) > limit {
		return Frame{}, false, ErrFrameTooLarge
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Frame{}, false, fmt.Errorf("grok: decode envelope: %w", err)
	}
	if env.Method != "session/update" && env.Method != "_x.ai/session/update" && env.Method != "_x.ai/session_notification" {
		return Frame{}, false, nil
	}
	if len(env.Params) == 0 {
		return Frame{}, false, fmt.Errorf("grok: %s envelope missing params", env.Method)
	}
	var params envelopeParams
	if err := json.Unmarshal(env.Params, &params); err != nil {
		return Frame{}, false, fmt.Errorf("grok: decode params: %w", err)
	}
	if len(params.Update) == 0 {
		return Frame{}, false, fmt.Errorf("grok: %s envelope missing params.update", env.Method)
	}
	frame := Frame{Timestamp: env.Timestamp, Method: env.Method, Params: Params{
		SessionID: params.SessionID,
		Update:    append(json.RawMessage(nil), params.Update...),
	}}
	if params.Meta != nil {
		frame.Params.Meta = &Meta{
			IsReplay:         params.Meta.IsReplay,
			EventID:          params.Meta.EventID,
			PromptID:         params.Meta.PromptID,
			AgentTimestampMs: params.Meta.AgentTimestampMs,
		}
		if params.Meta.UpdateParams != nil {
			frame.Params.Meta.UpdateParams = &UpdateParams{Status: params.Meta.UpdateParams.Status}
		}
	}
	return frame, true, nil
}

// SessionID returns a frame's explicit session scope, when present.
func SessionID(line []byte) (string, bool) {
	var envelope struct {
		Params struct {
			SessionID *string `json:"sessionId"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &envelope) != nil || envelope.Params.SessionID == nil {
		return "", false
	}
	return strings.TrimSpace(*envelope.Params.SessionID), true
}

// IsReplay reports the provider's replay marker on one ACP frame.
func IsReplay(line []byte) bool {
	if frame, relevant, err := DecodeFrame(line, MaxFrameBytes); err == nil && relevant {
		return frame.Params.Meta != nil && frame.Params.Meta.IsReplay
	}
	var envelope struct {
		Params struct {
			Meta struct {
				IsReplay bool `json:"isReplay"`
			} `json:"_meta"`
		} `json:"params"`
	}
	return json.Unmarshal(line, &envelope) == nil && envelope.Params.Meta.IsReplay
}

// AcceptancePromptID extracts the exact matching operator prompt from a queue frame.
func AcceptancePromptID(line []byte, submittedText, attachedSessionID string) string {
	var envelope struct {
		Method string `json:"method"`
		Params struct {
			SessionID       string `json:"sessionId"`
			RunningPromptID string `json:"runningPromptId"`
			RunningText     string `json:"runningText"`
			Entries         []struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
				Text string `json:"text"`
			} `json:"entries"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &envelope) != nil || envelope.Method != "_x.ai/queue/changed" || envelope.Params.SessionID != attachedSessionID {
		return ""
	}
	if envelope.Params.RunningPromptID != "" && !IsSyntheticPromptID(envelope.Params.RunningPromptID) && envelope.Params.RunningText == submittedText {
		return envelope.Params.RunningPromptID
	}
	for _, entry := range envelope.Params.Entries {
		if entry.ID != "" && !IsSyntheticPromptID(entry.ID) && entry.Kind == "prompt" && entry.Text == submittedText {
			return entry.ID
		}
	}
	return ""
}

// IsSyntheticPromptID reports a provider-generated continuation turn.
func IsSyntheticPromptID(id string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(id)), "subagent-completed-")
}

// Sanitize preserves an ACP frame while redacting host identity and unbounded output.
func Sanitize(line []byte) []byte {
	var value any
	if json.Unmarshal(line, &value) != nil {
		return append([]byte(nil), line...)
	}
	var scrub func(any)
	scrub = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				switch key {
				case "rawOutput", "signature", "env", "hostname", "agentId", "agentInstanceId",
					"subscription_tier_display", "announcements", "campaigns", "tips":
					typed[key] = map[string]any{"redacted": true}
				default:
					scrub(child)
				}
			}
		case []any:
			for _, child := range typed {
				scrub(child)
			}
		}
	}
	scrub(value)
	redacted, err := json.Marshal(value)
	if err != nil {
		return append([]byte(nil), line...)
	}
	return redacted
}
