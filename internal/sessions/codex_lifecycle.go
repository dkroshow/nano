package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	coresession "github.com/dkroshow/nano/internal/core/session"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"github.com/dkroshow/nano/internal/wire"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func codexWorkEvidence(dir wire.Direction, line []byte) (active bool, turnID string, startedAtMs int64, ok bool) {
	if dir != protocol.Recv {
		return false, "", 0, false
	}
	var msg protocol.Message
	if json.Unmarshal(line, &msg) != nil || !msg.IsNotification() {
		return false, "", 0, false
	}
	switch msg.Method {
	case codexTurnStartedMethod:
		turn := codexTurnFromParams(msg.Params)
		return true, turn.ID, turn.startedAtMs(), true
	case codexTurnCompletedMethod:
		return false, codexTurnFromParams(msg.Params).ID, 0, true
	}
	return false, "", 0, false
}
func codexLifecycleNudge(dir wire.Direction, line []byte) bool {
	if dir != protocol.Recv {
		return false
	}
	var msg protocol.Message
	if json.Unmarshal(line, &msg) != nil || !msg.IsNotification() {
		return false
	}
	// turn/completed is the only terminal turn notification: failure and
	// interruption arrive as turn/completed with turn.status failed|
	// interrupted (schema-verified, see runtime.go's lifecycle constants).
	switch msg.Method {
	case codexTurnStartedMethod, codexTurnCompletedMethod, codexThreadStartedMethod:
		return true
	}
	return false
}

// The response is captured from app-server ingress before replay processing.
// It is never read back from Nano's stored thread or attachment record.
type codexLifecycleAdapter struct {
	epoch, sequence uint64
	response        *codexResumeResponse
	nativeID        string
	settled         bool
}

// Capabilities reports the provider guarantees used to qualify lifecycle evidence.
func (codexLifecycleAdapter) Capabilities() lifecycle.Capabilities {
	return lifecycle.Capabilities{NativeTurnID: true, NativeTurnStart: true, ResumeWorkSnapshot: true, NativeRecovery: true, ResumeWithoutTranscript: true}
}

// Current returns qualified current lifecycle evidence, when the provider supplies it.
func (a codexLifecycleAdapter) Current() (lifecycle.Observation, bool) {
	if !a.settled || a.epoch == 0 || a.sequence == 0 || a.response == nil {
		return lifecycle.Observation{}, false
	}
	id := codexObjectString(a.response.thread, "id")
	if id == "" || (a.nativeID != "" && a.nativeID != id) {
		return lifecycle.Observation{}, false
	}
	event := lifecycle.EventReadyIdle
	if _, active := a.response.inFlightTurn(); active {
		event = lifecycle.EventReadyWorking
	}
	return lifecycle.Observation{Event: event, Epoch: a.epoch, Sequence: a.sequence}, true
}

func observeCodexWork(sess *Session, dir wire.Direction, line []byte, live bool) {
	if live && dir == protocol.Recv {
		sess.runtime.observeCodexTurnInput(line)
	}
	active, turnID, startedAtMs, ok := codexWorkEvidence(dir, line)
	if !ok || !live {
		return
	}
	var envelope struct {
		Params struct {
			ThreadID string `json:"threadId"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return
	}
	if threadID := sess.stateOwner.ThreadID(); envelope.Params.ThreadID != "" && threadID != "" && envelope.Params.ThreadID != threadID {
		return
	}
	if active && startedAtMs == 0 {
		startedAtMs = time.Now().UnixMilli()
	}
	sess.stateOwner.setTurnActive(active, turnID, startedAtMs, true)
}

func codexRuntimeEvidence(sess *Session, dir wire.Direction, line []byte, at time.Time) {
	if dir != protocol.Recv {
		return
	}
	var envelope struct {
		Method string                     `json:"method"`
		Params map[string]json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return
	}
	switch envelope.Method {
	case "item/started", "item/completed":
		// Codex compacts a turn's context as a contextCompaction item, before
		// the turn takes its prompt or between steps.
		var item struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(envelope.Params["item"], &item) == nil && item.Type == "contextCompaction" {
			started := wire.Int64(envelope.Params["startedAtMs"])
			if started == 0 {
				started = at.UnixMilli()
			}
			sess.stateOwner.markCompacting(envelope.Method == "item/started", started)
		}
	case "thread/tokenUsage/updated":
		sess.setLiveTokenUsageForTurn(wire.String(envelope.Params["turnId"]), envelope.Params["tokenUsage"])
	case "error":
		var payload map[string]json.RawMessage
		if json.Unmarshal(envelope.Params["error"], &payload) == nil {
			sess.setLiveError(wire.String(payload["message"]))
		} else {
			sess.setLiveError(wire.String(envelope.Params["message"]))
		}
	case "turn/completed":
		var turn map[string]json.RawMessage
		if json.Unmarshal(envelope.Params["turn"], &turn) == nil {
			status := wire.String(turn["status"])
			turnID := wire.String(turn["id"])
			sess.projection.updateCompletedTurn(turnID, status, wire.Int64(turn["durationMs"]), at.UnixMilli(), turn["error"])
			if status == "interrupted" {
				// A turn interrupted before it recorded its input saves nothing,
				// so no saved commit may follow to settle a prompt it dropped.
				go sess.delivery.SettleWaitingPrompts(context.Background())
			}
		}
	}
	return

}

func codexAcceptedEvidence(sess *Session) { sess.runtime.startCodexZeroTurnRecovery() }

// codexTurnInput is what one attachment saw of a Codex turn it watched from
// its start: the client ids of the prompts the turn took, and whether the
// turn ended interrupted. It holds no text.
type codexTurnInput struct {
	clientIDs   map[string]bool
	interrupted bool
}

// codexTurnInputCap bounds the turns one attachment remembers. Forgetting a
// turn only forgoes resending a prompt it dropped.
const codexTurnInputCap = 256

// observeCodexTurnInput records a live frame's turn input evidence. Only a
// turn whose start this attachment saw is recorded: Codex echoes each prompt
// it takes as a userMessage item carrying the prompt's client id, so such a
// turn ending interrupted without that echo proves Codex dropped the prompt.
func (sess *Runtime) observeCodexTurnInput(line []byte) {
	if !bytes.Contains(line, []byte(`"`+codexTurnStartedMethod+`"`)) && !bytes.Contains(line, []byte(`"`+codexTurnCompletedMethod+`"`)) &&
		!bytes.Contains(line, []byte(`"userMessage"`)) {
		return // most frames, deltas among them, carry no turn input evidence
	}
	var frame struct {
		Method string `json:"method"`
		Params struct {
			TurnID string `json:"turnId"`
			Turn   struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
			Item struct {
				Type     string `json:"type"`
				ClientID string `json:"clientId"`
			} `json:"item"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &frame) != nil {
		return
	}
	sess.session.stateOwner.stateMu.Lock()
	defer sess.session.stateOwner.stateMu.Unlock()
	switch frame.Method {
	case codexTurnStartedMethod:
		id := frame.Params.Turn.ID
		if id == "" || sess.session.stateOwner.codexTurnInputs[id] != nil {
			return
		}
		if sess.session.stateOwner.codexTurnInputs == nil {
			sess.session.stateOwner.codexTurnInputs = make(map[string]*codexTurnInput)
		}
		if len(sess.session.stateOwner.codexTurnInputs) >= codexTurnInputCap {
			for old := range sess.session.stateOwner.codexTurnInputs {
				delete(sess.session.stateOwner.codexTurnInputs, old)
				break
			}
		}
		sess.session.stateOwner.codexTurnInputs[id] = &codexTurnInput{clientIDs: map[string]bool{}}
	case "item/started", "item/completed":
		clientID := frame.Params.Item.ClientID
		if frame.Params.Item.Type != "userMessage" || clientID == "" {
			return
		}
		if turn := sess.session.stateOwner.codexTurnInputs[frame.Params.TurnID]; turn != nil {
			turn.clientIDs[clientID] = true
		}
		// An echo is positive proof in any turn, including one this
		// attachment did not see start.
		if wait := sess.session.stateOwner.codexEchoWaits[clientID]; wait != nil && wait.turnID == "" && frame.Params.TurnID != "" {
			wait.turnID = frame.Params.TurnID
			if wait.uncertain {
				delete(sess.session.stateOwner.codexEchoWaits, clientID)
				echoed := *wait
				go func() { // off the shared wire reader
					if sess.bindCodexEcho(echoed) {
						// Its turn may be saved and ended already, so no
						// later saved commit would settle it.
						sess.session.delivery.SettleWaitingPrompts(context.Background())
					}
				}()
			}
		}
	case codexTurnCompletedMethod:
		if turn := sess.session.stateOwner.codexTurnInputs[frame.Params.Turn.ID]; turn != nil {
			turn.interrupted = frame.Params.Turn.Status == "interrupted"
		}
	}
}

// forgetCodexTurnInputs starts a new attachment's turn evidence empty: a
// restart or reconnect gap never proves that a prompt was dropped. The epoch
// advances at once, so proof from the previous connection is unusable from
// this moment; the reset runs in order with the session's wire frames, so no
// frame from that connection lands after it, and makes the evidence usable
// again only if no later link loss or handshake advanced the epoch.
func (sess *Runtime) forgetCodexTurnInputs() {
	sess.session.stateOwner.stateMu.Lock()
	sess.session.stateOwner.codexTurnEpoch++
	epoch := sess.session.stateOwner.codexTurnEpoch
	sess.session.stateOwner.stateMu.Unlock()
	forget := func() error {
		sess.session.stateOwner.stateMu.Lock()
		sess.session.stateOwner.codexTurnInputs, sess.session.stateOwner.codexTurnInputsEpoch = nil, epoch
		sess.session.stateOwner.stateMu.Unlock()
		return nil
	}
	if core := sess.coreSession(); core != nil {
		core.ObserveAsync(coresession.AdapterEvent{LegacyApply: forget})
		return
	}
	_ = forget()
}

// codexEchoWait is one Codex delivery whose prompt's echo can bind it. Codex
// echoes each prompt it takes with the prompt's own client id, in the turn
// that took it, so the echo is the receipt a turn/start reply would have
// been. It matters when that reply never came in time: the delivery ended
// uncertain with no binding, and no saved row could settle it.
type codexEchoWait struct {
	actionID, claimToken string
	claimEpoch           uint64
	scope                model.Scope
	turnID               string // the turn that echoed it, once seen
	uncertain            bool   // its delivery ended uncertain with no binding
}

// codexEchoWaitCap bounds the waits one session keeps for prompts Codex never
// echoed. Forgetting one only leaves that prompt for the person to discard.
const codexEchoWaitCap = 64

// expectCodexEcho starts waiting for a delivery's echo before its provider
// call, so an echo read before the outcome is recorded is not lost.
func (sess *Runtime) expectCodexEcho(wait codexEchoWait) {
	sess.session.stateOwner.stateMu.Lock()
	defer sess.session.stateOwner.stateMu.Unlock()
	if sess.session.stateOwner.codexEchoWaits == nil {
		sess.session.stateOwner.codexEchoWaits = make(map[string]*codexEchoWait)
	}
	if len(sess.session.stateOwner.codexEchoWaits) >= codexEchoWaitCap {
		for old := range sess.session.stateOwner.codexEchoWaits {
			delete(sess.session.stateOwner.codexEchoWaits, old)
			break
		}
	}
	sess.session.stateOwner.codexEchoWaits[SubmissionEventID(sess.session.hostedThreadID, wait.actionID)] = &wait
}

// deliveredCodexEcho takes a delivery's outcome. Only one left uncertain with
// no binding keeps waiting, and an echo already seen binds it at once.
func (sess *Runtime) deliveredCodexEcho(actionID string, unbound bool) {
	clientID := SubmissionEventID(sess.session.hostedThreadID, actionID)
	sess.session.stateOwner.stateMu.Lock()
	wait := sess.session.stateOwner.codexEchoWaits[clientID]
	if wait == nil {
		sess.session.stateOwner.stateMu.Unlock()
		return
	}
	wait.uncertain = unbound
	echoed := *wait // the echo handler binds it otherwise, once seen
	if !unbound || echoed.turnID != "" {
		delete(sess.session.stateOwner.codexEchoWaits, clientID)
	}
	sess.session.stateOwner.stateMu.Unlock()
	if unbound && echoed.turnID != "" {
		sess.bindCodexEcho(echoed)
	}
}

// bindCodexEcho records an echoed prompt as accepted into the turn that took
// it, exactly as an on-time reply would have, so its saved row can settle it.
func (sess *Runtime) bindCodexEcho(wait codexEchoWait) bool {
	binding, _ := sess.submissionBinding(wait.scope, wait.turnID, SubmissionEventID(sess.session.hostedThreadID, wait.actionID))
	record, err := sess.session.delivery.turns.Accepted(context.Background(), prompts.Acceptance{
		ActionID: wait.actionID, ClaimToken: wait.claimToken, AttachmentEpoch: wait.claimEpoch, Binding: binding,
	})
	if err != nil {
		log.Printf("bind echoed prompt %s: %v", sess.session.hostedThreadID, err)
		return false
	}
	sess.session.delivery.publishTransactionalOutbox(record.Outbox)
	sess.session.delivery.awaitSavedPrompt(wait.actionID)
	return true
}

// codexTurnDropped reports that this attachment, ready and current, saw
// turnID from its start end interrupted without taking the prompt with
// clientID.
func (sess *Runtime) codexTurnDropped(turnID, clientID string) bool {
	sess.session.stateOwner.stateMu.Lock()
	defer sess.session.stateOwner.stateMu.Unlock()
	if sess.session.stateOwner.codexTurnInputsEpoch != sess.session.stateOwner.codexTurnEpoch || sess.session.stateOwner.turnRestoreInProgress ||
		sess.session.stateOwner.lifecycleState.State.Connection != lifecycle.Ready {
		return false
	}
	turn := sess.session.stateOwner.codexTurnInputs[turnID]
	return turn != nil && turn.interrupted && !turn.clientIDs[clientID]
}
