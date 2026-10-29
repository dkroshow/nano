package sessions

import (
	"encoding/json"
	"time"

	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/wire"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func grokWorkEvidence(dir wire.Direction, line []byte) (active bool, turnID string, startedAtMs int64, ok bool) {
	if dir != protocol.Recv {
		return false, "", 0, false
	}
	if promptID, _, _, terminal := grokTerminalFromWire(line); terminal {
		return false, promptID, 0, true
	}
	var envelope struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return false, "", 0, false
	}
	if envelope.Method == "_x.ai/queue/changed" {
		if promptID := grokPromptIDFromWire(line); promptID != "" {
			return true, promptID, 0, true
		}
		return false, "", 0, false
	}
	if prompt, atMS := grokEvidencePrompt(grokWireEvidence(line)); prompt != "" {
		return true, prompt, atMS, true
	}
	return false, "", 0, false
}
func grokLifecycleNudge(dir wire.Direction, line []byte) bool {
	if dir != protocol.Recv {
		return false
	}
	if _, _, _, ok := grokTerminalFromWire(line); ok {
		return true
	}
	var envelope struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return false
	}
	return envelope.Method == "_x.ai/queue/changed"
}

// Session/load reports the leader's currently running prompt. Historical
// Session/update replay is intentionally not a substitute for this response.
type grokLifecycleAdapter struct {
	epoch, sequence uint64
	response        json.RawMessage
	nativeID        string
	settled         bool
}

// Capabilities reports the provider guarantees used to qualify lifecycle evidence.
func (grokLifecycleAdapter) Capabilities() lifecycle.Capabilities {
	return lifecycle.Capabilities{
		NativeTurnID: true, ReadyWithoutInit: true, ResumeWorkSnapshot: true,
		BootHandshakeLimit: grokBootHandshakeLimit,
	}
}

// Current returns qualified current lifecycle evidence, when the provider supplies it.
func (a grokLifecycleAdapter) Current() (lifecycle.Observation, bool) {
	var response map[string]json.RawMessage
	if !a.settled || a.epoch == 0 || a.sequence == 0 || json.Unmarshal(a.response, &response) != nil || response == nil {
		return lifecycle.Observation{}, false
	}
	id := grokSessionID(a.response)
	// A successful load is evidence for its requested native thread even when
	// the provider does not repeat that identity in the response body.
	if id == "" {
		id = a.nativeID
	}
	if id == "" || (a.nativeID != "" && a.nativeID != id) {
		return lifecycle.Observation{}, false
	}
	event := lifecycle.EventReadyIdle
	if id := grokRunningPromptID(a.response); id != "" && !grok.IsSyntheticPromptID(id) {
		event = lifecycle.EventReadyWorking
	}
	return lifecycle.Observation{Event: event, Epoch: a.epoch, Sequence: a.sequence}, true
}

func observeGrokWork(sess *State, dir wire.Direction, line []byte, live bool) {
	active, turnID, startedAtMs, ok := grokWorkEvidence(dir, line)
	if !ok || !live {
		return
	}
	if active && startedAtMs == 0 {
		startedAtMs = time.Now().UnixMilli()
	}
	sess.setTurnActive(active, turnID, startedAtMs, true)
}

func grokRuntimeEvidence(sess *State, dir wire.Direction, line []byte, at time.Time) {
	if dir != protocol.Recv {
		return
	}
	if promptID, status, usage, terminal := grokTerminalFromWire(line); terminal {
		if len(usage) > 0 {
			sess.setLiveTokenUsageForTurn(promptID, usage)
		}
		if status == agentTurnFailed {
			sess.setLiveError("Grok turn failed")
		}
		duration := int64(0)
		var usageBody map[string]json.RawMessage
		if json.Unmarshal(usage, &usageBody) == nil {
			duration = wire.Int64(usageBody["apiDurationMs"])
		}
		sess.view.liveProjection().updateCompletedTurn(promptID, status, duration, at.UnixMilli())
		return
	}
	return

}
