package sessions

import (
	"encoding/json"
	"strings"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/lifecycle"
	threadvalues "github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/wire"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// claudeBootHandshakeLimit bounds concurrent Claude boot attaches. Eight sit
// between the two measured points: four made visible threads' attaches queue
// behind one another, and twelve slowed the other providers' registrations.
// Each attach replays only the unsaved tail (docs/decisions/claude-shim-daemon.md).
const claudeBootHandshakeLimit = 8

func claudeWorkEvidence(dir wire.Direction, line []byte) (active bool, turnID string, startedAtMs int64, ok bool) {
	if active, ok := claudeTurnBoundaryTransition(dir, line); ok {
		return active, "", 0, true
	}
	if dir == protocol.Recv && ClaudeExecutionEvidence(line) {
		return true, "", 0, true
	}
	return false, "", 0, false
}
func claudeLifecycleNudge(dir wire.Direction, line []byte) bool {
	var probe struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if json.Unmarshal(line, &probe) != nil {
		return false
	}
	if dir == protocol.Send {
		return probe.Type == claudeUserLineType
	}
	return probe.Type == claudeResultLineType ||
		(probe.Type == claudeSystemLineType && probe.Subtype == claudeInitSubtype)
}

// claudeTurnBoundaryTransition is the hard prompt/result boundary persisted in
// the shim spool. Keep it separate from flowing execution evidence: replay
// reconstruction needs the exact opening user-line sequence, while a live
// assistant/stream line can assert activity for turns started by a harness.
func claudeTurnBoundaryTransition(dir wire.Direction, line []byte) (active, ok bool) {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &probe) != nil {
		return false, false
	}
	if dir == protocol.Send && probe.Type == claudeUserLineType {
		return true, true
	}
	if dir == protocol.Recv && probe.Type == claudeResultLineType {
		return false, true
	}
	return false, false
}

// ClaudeExecutionEvidence names provider output that can only occur while the
// long-lived child is executing a turn. system.init and control traffic are
// intentionally excluded because both also occur while an idle child attaches.
func ClaudeExecutionEvidence(line []byte) bool {
	var probe struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Status  string `json:"status"`
	}
	if json.Unmarshal(line, &probe) != nil {
		return false
	}
	switch probe.Type {
	case "assistant", "stream_event", "user":
		return true
	case claudeSystemLineType:
		// Compacting is the turn's work, and it can come before any other.
		return probe.Subtype == "status" && probe.Status == "compacting"
	default:
		return false
	}
}

// ClaudeNativeIDProvisional reports whether an identity is an unresolved Claude shim handle.
func ClaudeNativeIDProvisional(id string) bool {
	return hostedstore.NativeThreadIDProvisional("claude", id)
}

// A shim response and its fully drained ordered spool are provider evidence.
// A persisted timestamp or provisional shim identity is never activity evidence.
type claudeLifecycleAdapter struct {
	epoch, sequence uint64
	state           string
	active          bool
	settled         bool
}

// Capabilities reports the provider guarantees used to qualify lifecycle evidence.
func (claudeLifecycleAdapter) Capabilities() lifecycle.Capabilities {
	return lifecycle.Capabilities{ReadyWithoutInit: true, ResumeWorkSnapshot: true, BootHandshakeLimit: claudeBootHandshakeLimit}
}

// Current returns qualified current lifecycle evidence, when the provider supplies it.
func (a claudeLifecycleAdapter) Current() (lifecycle.Observation, bool) {
	if !a.settled || a.epoch == 0 || a.sequence == 0 || a.state != "running" {
		return lifecycle.Observation{}, false
	}
	event := lifecycle.EventReadyIdle
	if a.active {
		event = lifecycle.EventReadyWorking
	}
	return lifecycle.Observation{Event: event, Epoch: a.epoch, Sequence: a.sequence}, true
}

func observeClaudeWork(sess *Session, dir wire.Direction, line []byte, live bool) {
	active, turnID, startedAtMs, ok := claudeWorkEvidence(dir, line)
	if !ok || (!live && dir == protocol.Recv && active) {
		return
	}
	// Outbound user text is a local send, not evidence of provider execution.
	// Historical outbound boundaries can reconstruct the shim's ordered spool.
	if live && dir == protocol.Send {
		return
	}
	if active && startedAtMs == 0 && live {
		startedAtMs = time.Now().UnixMilli()
	}
	if active && sess.view.projection != nil {
		turnID = sess.view.projection.ensureClaudeActiveTurn(startedAtMs)
	}
	// A live result names the turn it ended, so a send acknowledged after it
	// cannot reopen that turn (acceptProviderTurn).
	if !active && live && dir == protocol.Recv && sess.view.projection != nil {
		turnID = sess.view.projection.claudeResultTurn()
	}
	if !live {
		sess.stateOwner.restoreTurnActive(active, turnID, startedAtMs)
	} else {
		sess.stateOwner.setTurnActive(active, turnID, startedAtMs, true)
	}
}

func claudeRuntimeEvidence(sess *Session, dir wire.Direction, line []byte, at time.Time) {
	var message map[string]json.RawMessage
	if json.Unmarshal(line, &message) != nil {
		return
	}
	switch wire.String(message["type"]) {
	case "system":
		// Claude reports status "compacting" every 30 s while it compacts,
		// then status null (with compact_result) when it has finished.
		if status, ok := message["status"]; ok && wire.String(message["subtype"]) == "status" {
			sess.stateOwner.markCompacting(wire.String(status) == "compacting", at.UnixMilli())
		}
	case "result":
		if usagePayload := claudeResultUsagePayload(message); usagePayload != nil {
			sess.view.setLiveTokenUsageForTurn(sess.view.projection.latestTerminalTurnID(), usagePayload)
		}
		if wire.Bool(message["is_error"]) && !strings.HasPrefix(wire.String(message["terminal_reason"]), "aborted") {
			sess.view.setLiveError(threadvalues.FirstNonEmpty(wire.String(message["result"]), wire.String(message["subtype"]), "turn failed"))
		}
	}
}

// claudeResultUsagePayload is the token usage one result line reports, in the
// live usage shape; nil when the line carries none.
func claudeResultUsagePayload(message map[string]json.RawMessage) json.RawMessage {
	var usage map[string]json.RawMessage
	_ = json.Unmarshal(message["usage"], &usage)
	if len(usage) == 0 {
		return nil
	}
	input := wire.Int64(usage["input_tokens"])
	cachedInput := wire.Int64(usage["cache_creation_input_tokens"]) + wire.Int64(usage["cache_read_input_tokens"])
	output := wire.Int64(usage["output_tokens"])
	payload, _ := json.Marshal(map[string]any{
		"last":               map[string]int64{"inputTokens": input, "cachedInputTokens": cachedInput, "outputTokens": output},
		"total":              map[string]int64{"totalTokens": input + cachedInput + output, "inputTokens": input + cachedInput, "outputTokens": output},
		"modelContextWindow": 200000,
	})
	return payload
}

func claudeReleaseAcceptedEvidence(sess *Session) {
	if child := sess.runtime.claudeHandle(); child != nil {
		child.releaseTurnIngress()
	}
}
func claudeEvidenceIngressBlocked(sess *Session) bool {
	child := sess.runtime.claudeHandle()
	return child != nil && child.turnIngressActive()
}
