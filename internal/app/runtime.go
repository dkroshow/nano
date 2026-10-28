package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/threads"
)

// Turn lifecycle wire vocabulary, verified against
// internal/wire/codex/schema/codex_app_server_protocol.v2.schemas.json (ServerNotification)
// and internal/wire/codex/captures/first-turn-annotated.md: the codex lifecycle is exactly
// turn/started and turn/completed — failure and interruption arrive as
// turn/completed with turn.status failed|interrupted, not as separate methods.
// On the Claude wire a turn opens with Nano's own sent user line OR received
// execution evidence (notification-started turns bypass Nano's send path), and
// closes with the received result line.
const (
	codexTurnStartedMethod   = "turn/started"
	codexTurnCompletedMethod = "turn/completed"
	claudeUserLineType       = "user"
	claudeResultLineType     = "result"
)

// sessionStatusExited is the terminal session status. An exited session lingers
// in s.sessions until someone closes it — its child is gone, so nothing that
// asks "is this thread alive?" may count it as live.
const sessionStatusExited = "exited"

type codexTurn struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	StartedAt *int64 `json:"startedAt"`
}

// codexTurnInProgressStatus is the TurnStatus value that means the turn is
// still running (enum: completed | interrupted | failed | inProgress).
const codexTurnInProgressStatus = "inProgress"

// msPerSecond converts codex's unix-seconds timestamps at the ingest boundary.
const msPerSecond = 1000

// residualMsThreshold is the D1 residual defense: seconds-since-epoch is ~1.8e9
// while milliseconds-since-epoch is ~1.8e12, so anything above 1e10 is already
// milliseconds and must not be multiplied again. It has never fired against the
// current schema — it exists because the version that stops honoring the schema
// is the one nobody re-probes.
const residualMsThreshold = int64(1e10)

// codexTurnFromParams pulls the Turn out of a turn/started or turn/completed
// notification (TurnStartedNotification: {threadId, turn:{id,…}}).
func codexTurnFromParams(params json.RawMessage) codexTurn {
	var p struct {
		Turn codexTurn `json:"turn"`
	}
	if json.Unmarshal(params, &p) != nil {
		return codexTurn{}
	}
	return p.Turn
}

func (t codexTurn) startedAtMs() int64 {
	if t.StartedAt == nil || *t.StartedAt <= 0 {
		return 0
	}
	if *t.StartedAt > residualMsThreshold {
		return *t.StartedAt
	}
	return *t.StartedAt * msPerSecond
}

// nativeThreadIDProvisional reports whether a recorded native id is a Claude
// shim stand-in (cs_<hex>, minted in cmd/claude-shim) rather than a provider
// id. The provisional handle survives in durable records when a thread never
// completes its first prompt, and a field named nativeThreadId must not pass
// one off as real (slice-1 design, ID-disambiguation directive).
func nativeThreadIDProvisional(provider, nativeID string) bool {
	if provisional := lifecycleProviders[provider].provisional; provisional != nil {
		return provisional(nativeID)
	}
	return false
}

func providerTurnIDFromStart(result json.RawMessage) string {
	var body struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(result, &body) != nil {
		return ""
	}
	return body.Turn.ID
}

func providerEventIDFromStart(result json.RawMessage) string {
	var body struct {
		Event struct {
			ID string `json:"id"`
		} `json:"event"`
	}
	if json.Unmarshal(result, &body) != nil {
		return ""
	}
	return body.Event.ID
}

func deliveryExecutionError(err error) *agentExecutionError {
	if err == nil {
		return nil
	}
	code := "delivery_unknown"
	if errors.Is(err, context.DeadlineExceeded) {
		code = "delivery_timeout"
	} else {
		var rpcErr *protocol.RPCError
		if errors.As(err, &rpcErr) {
			code = "delivery_refused"
		}
	}
	return &agentExecutionError{Source: "delivery", Code: code, Message: err.Error()}
}

func wireNudgesProjectionMeta(provider string, dir protocol.Direction, line []byte) bool {
	if dir != protocol.Recv {
		return false
	}
	if provider == "grok" {
		if _, _, _, ok := grokTerminalFromWire(line); ok {
			return true
		}
		var envelope struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(line, &envelope) != nil {
			return false
		}
		return envelope.Method == "_x.ai/models/update" || envelope.Method == "_x.ai/settings/update"
	}
	if provider == "claude" {
		var message struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(line, &message) != nil {
			return false
		}
		return message.Type == "result" || (message.Type == "system" && message.Subtype == "init")
	}
	var envelope struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return false
	}
	switch envelope.Method {
	case "turn/completed", "skills/changed", "mcpServer/startupStatus/updated":
		return true
	default:
		return false
	}
}

func claudeProjectionTerminal(line []byte) (string, json.RawMessage) {
	var message map[string]json.RawMessage
	if json.Unmarshal(line, &message) != nil {
		return agentTurnFailed, json.RawMessage(`{"message":"invalid Claude result"}`)
	}
	reason := protocol.String(message["terminal_reason"])
	if strings.HasPrefix(reason, "aborted") {
		return agentTurnInterrupted, nil
	}
	if protocol.Bool(message["is_error"]) {
		detail, _ := json.Marshal(map[string]any{
			"message":        threads.FirstNonEmpty(protocol.String(message["result"]), protocol.String(message["subtype"]), "Claude turn failed"),
			"terminalReason": reason,
		})
		return agentTurnFailed, detail
	}
	return agentTurnCompleted, nil
}
