// claude_control.go — the Claude control channel: the second protocol carried
// on the same stream-json stdio as the data messages (claude.go). It is what
// makes a hosted Claude session steerable rather than merely observable:
// interrupt, model switching, and the operator dialogs the CLI raises.
//
// Framing (probed against claude CLI 2.1.220, slice 3a §1 and re-verified
// 2026-07-31). Client→CLI is one line:
//
//	{"type":"control_request","request_id":"<string>","request":{"subtype":…}}
//
// and the reply is
//
//	{"type":"control_response","response":{"subtype":"success","request_id":…,"response":{…}}}
//
// or `{"subtype":"error","request_id":…,"error":"<sentence>"}`. Frames
// interleave freely with the data stream, so nothing about the existing reducer
// or wire recording changes: every control frame is recorded like any other line
// and shows up in the Event Feed.
//
// The channel runs in BOTH directions. Inbound `control_request`s are the CLI
// asking the operator something — a tool permission, an AskUserQuestion, an MCP
// elicitation — and they park as the same server-owned pendingInteraction
// objects codex's server requests park as (interactions.go). One card renders
// both providers.
//
// Two hazards this file exists to handle:
//
//   - **Replay.** The shim spools every line in both directions and replays the
//     tail after the re-attach floor, which never passes an open inbound
//     control_request (claude_replay_floor.go), so it remains in that tail. A
//     replayed inbound control_request
//     is a request from a PREVIOUS nano's session, and treating it as live would
//     resurrect a dialog nobody is waiting on. Replayed requests are therefore
//     held aside rather than parked, and re-attach decides once the replay ends:
//     see rehydratePending and replayedControl, which also record why the CLI's
//     documented pending_* primitive turned out not to fire here.
//   - **Correlation across nano restarts.** Request ids carry a per-process
//     nonce, so a replayed control_response minted by an earlier nano can never
//     resolve a call this process is waiting on.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// Wire vocabulary.
const (
	claudeControlRequestType  = "control_request"
	claudeControlResponseType = "control_response"

	claudeControlSuccess = "success"
	claudeControlError   = "error"
)

// Outbound subtypes nano uses. All five were verified to work on a child
// started with nano's own argv — the control channel needs no spawn-time flag.
const (
	claudeSubtypeInitialize = "initialize"
	claudeSubtypeInterrupt  = "interrupt"
	claudeSubtypeSetModel   = "set_model"
)

// Inbound subtypes: the CLI asking the client for a decision.
const (
	claudeSubtypeCanUseTool        = "can_use_tool"
	claudeSubtypeElicitation       = "elicitation"
	claudeSubtypeRequestUserDialog = "request_user_dialog"
)

// claudeAskUserQuestionTool is the interactive-question tool. It arrives as a
// `can_use_tool` permission ask rather than as its own subtype (verified
// 2026-07-31), which is why the router keys on the tool name: the question card
// and a command-approval card are the same mechanism with different bodies.
const claudeAskUserQuestionTool = "AskUserQuestion"

// claudeSetModelDefault resets to the session's spawn-time model. The CLI's own
// wording: "Omitted, null, or 'default' resets to the session default model" —
// so nano's empty-override convention maps onto it exactly.
const claudeSetModelDefault = "default"

const (
	// claudeControlTimeout bounds an ordinary control call. `initialize` is the
	// slow one (~1.1s on a cold child); everything else answers in single-digit
	// milliseconds.
	claudeControlTimeout = 20 * time.Second
	// claudeInterruptTimeout is shorter on purpose: a live turn acks in ~1ms and
	// an idle session acks just as fast, so a wait this long already means the
	// child is not reading its stdin.
	claudeInterruptTimeout = 10 * time.Second
)

var errClaudeControlClosed = errors.New("claude session ended before the control response arrived")

// claudeControl correlates outbound control requests with their responses, and
// remembers what a spool replay contained so an unanswered request can be
// recovered (see replayedControl).
type claudeControl struct {
	mu      sync.Mutex
	pending map[string]chan claudeControlReply
	nonce   string
	seq     uint64

	replayed replayedControl
}

// replayedControl is what a re-attach's spool replay says about requests the
// CLI raised before this nano existed.
//
// It exists because the documented rehydration primitive does not fire here.
// The CLI's schema says `initialize` answers with pending_permission_requests /
// pending_user_dialog_requests "so a client joining an already-initialized
// session learns about in-flight prompts", and slice 3a recorded that as the
// authoritative re-attach path. **Probed live 2026-07-31 with a genuinely open
// `can_use_tool`, the initialize response carried neither field** — those arms
// appear to belong to the CLI's remote-control bridge, not to the `-p` stdio
// bridge. Without a fallback, a dialog raised before a nano restart is lost and
// its turn hangs forever.
//
// The spool tail is a complete substitute here, not an inference: the replay
// floor never passes an open request, and the shim records every line in both
// directions while allowing exactly one attached client. If a replayed inbound
// request has no replayed response carrying its id, and neither a cancel nor
// its own turn's result retired it, the CLI is still waiting. Settled requests
// are skipped, which keeps "render once" true when initialize also reports a
// pending request.
type replayedControl struct {
	requests map[string]json.RawMessage
	answered map[string]bool
}

// noteReplayedRequest remembers an inbound request seen during replay. It is
// deliberately not parked yet: the responses that may answer it are still
// arriving further down the same replay.
func (cc *claudeControl) noteReplayedRequest(id string, request json.RawMessage) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.replayed.requests == nil {
		cc.replayed.requests = map[string]json.RawMessage{}
	}
	cc.replayed.requests[id] = append(json.RawMessage(nil), request...)
}

func (cc *claudeControl) noteReplayedAnswer(id string) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.replayed.answered == nil {
		cc.replayed.answered = map[string]bool{}
	}
	cc.replayed.answered[id] = true
}

// takeUnansweredReplayed drains the replayed requests nothing ever answered.
func (cc *claudeControl) takeUnansweredReplayed() map[string]json.RawMessage {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	out := map[string]json.RawMessage{}
	for id, request := range cc.replayed.requests {
		if !cc.replayed.answered[id] {
			out[id] = request
		}
	}
	cc.replayed.requests = nil
	cc.replayed.answered = nil
	return out
}

// claudeControlReply is one decoded control_response.
type claudeControlReply struct {
	// Payload is the inner `response` object on a success; nil on an error.
	Payload json.RawMessage
	// ErrText is the CLI's own sentence on an error subtype, e.g.
	// `Model "not-a-real-model" is not a recognized model id.`
	ErrText string
}

func newClaudeControl() *claudeControl {
	buf := make([]byte, 6)
	rand.Read(buf)
	return &claudeControl{pending: map[string]chan claudeControlReply{}, nonce: hex.EncodeToString(buf)}
}

// nextID mints a request id unique to this nano process. The nonce is what
// keeps a replayed frame from an earlier nano out of this process's pending map.
func (cc *claudeControl) nextID() string {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.seq++
	return fmt.Sprintf("nano-%s-%d", cc.nonce, cc.seq)
}

func (cc *claudeControl) register(id string) chan claudeControlReply {
	ch := make(chan claudeControlReply, 1)
	cc.mu.Lock()
	cc.pending[id] = ch
	cc.mu.Unlock()
	return ch
}

func (cc *claudeControl) release(id string) {
	cc.mu.Lock()
	delete(cc.pending, id)
	cc.mu.Unlock()
}

// resolve hands a decoded reply to whoever is waiting. An id nobody is waiting
// on — a replayed frame, or a call that already timed out — is dropped, which
// is the whole point of keying by id rather than by arrival order.
func (cc *claudeControl) resolve(id string, reply claudeControlReply) {
	cc.mu.Lock()
	ch := cc.pending[id]
	delete(cc.pending, id)
	cc.mu.Unlock()
	if ch != nil {
		ch <- reply
	}
}

// controlCall sends one control request and waits for its response. Every frame
// is wire-recorded in both directions, so the Event Feed shows the control
// channel exactly as it shows the data stream.
func (c *claudeSession) controlCall(ctx context.Context, subtype string, request map[string]any) (json.RawMessage, error) {
	id := c.control.nextID()
	frame := map[string]any{"subtype": subtype}
	for k, v := range request {
		frame[k] = v
	}
	line, err := json.Marshal(map[string]any{
		"type": claudeControlRequestType, "request_id": id, "request": frame,
	})
	if err != nil {
		return nil, err
	}
	ch := c.control.register(id)
	defer c.control.release(id)

	c.sess.recordWire(protocol.Send, line)
	if err := c.tr.sendLine(line); err != nil {
		return nil, fmt.Errorf("claude %s: %w", subtype, err)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.exited:
		return nil, errClaudeControlClosed
	case reply := <-ch:
		if reply.ErrText != "" {
			return nil, fmt.Errorf("claude %s: %s", subtype, reply.ErrText)
		}
		return reply.Payload, nil
	}
}

// applyControlResponse decodes one control_response and resolves the call
// waiting on it.
//
// It handles both directions on purpose. A live inbound response answers one of
// nano's outbound calls; a REPLAYED one may instead be a previous nano's answer
// to a request the CLI raised, which is exactly the evidence that makes that
// request safe to skip when the spool is scanned for survivors.
func (c *claudeSession) applyControlResponse(tr claudeTransport, line []byte) {
	var frame struct {
		Response struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Response  json.RawMessage `json:"response"`
			Error     string          `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(line, &frame) != nil || frame.Response.RequestID == "" {
		return
	}
	if tr.isReplaying() {
		c.control.noteReplayedAnswer(frame.Response.RequestID)
		return
	}
	c.control.resolve(frame.Response.RequestID, controlReplyOf(frame.Response.Subtype, frame.Response.Response, frame.Response.Error))
}

// captureReplayedSend records a control_response nano itself sent in a previous
// life, replayed out of the shim's spool.
func (c *claudeSession) captureReplayedSend(line []byte) {
	var frame struct {
		Type     string `json:"type"`
		Response struct {
			RequestID string `json:"request_id"`
		} `json:"response"`
	}
	if json.Unmarshal(line, &frame) != nil || frame.Type != claudeControlResponseType {
		return
	}
	if frame.Response.RequestID != "" {
		c.control.noteReplayedAnswer(frame.Response.RequestID)
	}
}

// controlReplyOf normalizes the success/error arms into one reply value.
func controlReplyOf(subtype string, payload json.RawMessage, errText string) claudeControlReply {
	reply := claudeControlReply{Payload: payload}
	if subtype == claudeControlError {
		reply.ErrText = errText
		if reply.ErrText == "" {
			reply.ErrText = "control request failed"
		}
	}
	return reply
}

// --- outbound calls ----------------------------------------------------------

// claudeInterruptResult is the interrupt ack (`interrupt_receipt_v1`). It is
// identical for a live turn and an idle session — the CLI answers `success`
// either way and simply emits no `result` line when there was no turn — which is
// why the caller must decide "is there a turn?" from nano's own turn state
// before asking. That is the claude analog of codex's completion race.
type claudeInterruptResult struct {
	StillQueued []string `json:"still_queued"`
}

func (c *claudeSession) interrupt(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, claudeInterruptTimeout)
	defer cancel()
	_, err := c.controlCall(ctx, claudeSubtypeInterrupt, nil)
	return err
}

// setModel switches the model for subsequent turns without restarting the
// child: the session id, the transport and the whole conversation survive
// (verified 2026-07-30). An empty model resets to the session default.
func (c *claudeSession) setModel(ctx context.Context, model string) error {
	ctx, cancel := context.WithTimeout(ctx, claudeControlTimeout)
	defer cancel()
	if model = strings.TrimSpace(model); model == "" {
		model = claudeSetModelDefault
	}
	_, err := c.controlCall(ctx, claudeSubtypeSetModel, map[string]any{"model": model})
	return err
}

// claudeInitState is the part of the `initialize` response nano keeps: the model
// catalog the pane's switcher renders, the command catalog, and the two pending
// arrays that make re-attach rehydration authoritative rather than inferred.
//
// Both pending arrays are ABSENT when empty (observed 2026-07-31) — the schema
// documents them as "sent on the initialize response", not as always present, so
// their absence is "nothing parked", never "unsupported".
type claudeInitState struct {
	Models   json.RawMessage `json:"models,omitempty"`
	Commands json.RawMessage `json:"commands,omitempty"`

	PendingPermissionRequests []json.RawMessage `json:"pending_permission_requests,omitempty"`
	PendingUserDialogRequests []json.RawMessage `json:"pending_user_dialog_requests,omitempty"`
}

// initializeControl runs the handshake that turns a live transport into a
// controllable session. It is sent on every attach as well as every start:
// re-sending it is how a re-attached nano relearns what is parked, and the CLI
// treats later initializes as idempotent.
func (c *claudeSession) initializeControl(ctx context.Context) (*claudeInitState, error) {
	ctx, cancel := context.WithTimeout(ctx, claudeControlTimeout)
	defer cancel()
	raw, err := c.controlCall(ctx, claudeSubtypeInitialize, nil)
	if err != nil {
		return nil, err
	}
	var state claudeInitState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("claude initialize: %w", err)
	}
	c.sess.setState("controlInit", raw)
	return &state, nil
}

// startControlChannel is the post-transport handshake: initialize, publish the
// catalogs, and re-park anything the CLI says is still awaiting an answer.
// Runs in its own goroutine — a child that never answers must not hold up a
// spawn, and the session is already usable without the control channel.
func (c *claudeSession) startControlChannel() {
	state, err := c.initializeControl(context.Background())
	if err != nil {
		// Not fatal: the data stream is unaffected, and the operator loses
		// only the control affordances. Loud, because a silent loss of the stop
		// button would read as a nano bug.
		log.Printf("claude session %s: control initialize failed (%v) — stop/model/dialogs unavailable", c.sess.id, err)
	}
	c.rehydratePending(state)
}

// rehydratePending re-parks every request that is still genuinely open, from
// both sources, deduplicated by request id — the CLI's own doc warns that "the
// same request_id also arriving as a live or replayed control_request frame …
// must render once", and addPendingInteraction is keyed by id, so a request
// named by both lands as one card.
//
// initialize goes first because when it does answer it is the CLI's own
// statement of truth; the spool survivors follow as the fallback the observed
// behavior forces (replayedControl explains why).
func (c *claudeSession) rehydratePending(state *claudeInitState) {
	if state != nil {
		pending := append(append([]json.RawMessage(nil), state.PendingPermissionRequests...), state.PendingUserDialogRequests...)
		for _, entry := range pending {
			requestID, request, ok := claudeControlRequestParts(entry)
			if !ok {
				log.Printf("claude session %s: unrecognized pending control request %s", c.sess.id, truncateForLog(entry))
				continue
			}
			c.parkControlRequest(requestID, request)
		}
	}
	for requestID, request := range c.control.takeUnansweredReplayed() {
		log.Printf("claude session %s: recovering unanswered control request %s from the spool", c.sess.id, requestID)
		c.parkControlRequest(requestID, request)
	}
}

// claudeControlRequestParts pulls (request_id, request) out of either a whole
// control_request frame or the bare request object. The pending_* arrays are
// documented but were never observed non-empty during the probe (nothing was
// parked), so both plausible shapes are accepted rather than guessed at.
func claudeControlRequestParts(raw json.RawMessage) (string, json.RawMessage, bool) {
	var frame struct {
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
		Subtype   string          `json:"subtype"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame.RequestID == "" {
		return "", nil, false
	}
	if len(frame.Request) > 0 {
		return frame.RequestID, frame.Request, true
	}
	if frame.Subtype != "" {
		return frame.RequestID, raw, true
	}
	return "", nil, false
}

func truncateForLog(raw []byte) string {
	const max = 200
	if len(raw) <= max {
		return string(raw)
	}
	return string(raw[:max]) + "…"
}

// --- inbound requests --------------------------------------------------------

// handleInboundControl routes a control_request the CLI sent us.
//
// Replay discipline: while the shim is replaying its spool, every inbound
// request belongs to a previous nano's session and may already have been
// answered — parking it would put a dead dialog in front of the operator, and
// answering it would write a response the CLI has long since stopped waiting
// for. Rehydration is initialize's job (rehydratePending), per the CLI's own
// guidance that the same request id may arrive both ways and must render once.
func (c *claudeSession) handleInboundControl(tr claudeTransport, line []byte) {
	var frame struct {
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
	}
	if json.Unmarshal(line, &frame) != nil || frame.RequestID == "" {
		return
	}
	if tr.isReplaying() {
		// Held, not parked: the response that answers it may still be further
		// down this same replay. startControlChannel decides once replay ends.
		c.control.noteReplayedRequest(frame.RequestID, frame.Request)
		return
	}
	c.parkControlRequest(frame.RequestID, frame.Request)
}

// parkControlRequest turns one inbound request into a pending interaction, or
// refuses it where nano has no honest answer.
func (c *claudeSession) parkControlRequest(requestID string, request json.RawMessage) {
	var probe struct {
		Subtype  string `json:"subtype"`
		ToolName string `json:"tool_name"`
	}
	if json.Unmarshal(request, &probe) != nil {
		return
	}
	kind, ok := claudeInteractionKind(probe.Subtype, probe.ToolName)
	if !ok {
		// request_user_dialog lands here by design: its payload shape is defined
		// per dialog_kind and carried opaquely, and nano declares no dialog kinds
		// precisely because it can render none of them. The CLI's documented
		// contract is to fail closed on an undeclared kind, so cancelling is the
		// behavior it expects — parking a dialog nano could only mishandle is
		// what its own doc warns against.
		log.Printf("claude session %s: declining control request %s (%s)", c.sess.id, requestID, probe.Subtype)
		if err := c.respondControl(requestID, claudeRefusal(probe.Subtype)); err != nil {
			log.Printf("claude session %s: declining %s: %v", c.sess.id, probe.Subtype, err)
		}
		return
	}
	pending := pendingInteraction{
		ID: requestID, Method: probe.Subtype, Kind: kind,
		ThreadID:  c.sess.ThreadID(),
		CreatedAt: time.Now().Format(time.RFC3339Nano),
		Params:    append(json.RawMessage(nil), request...),
	}
	if c.sess.rejectStatusInteraction(pending) {
		if err := c.respondControl(requestID, claudeRefusal(probe.Subtype)); err != nil {
			log.Printf("claude status session %s: declining %s: %v", c.sess.id, probe.Subtype, err)
		}
		return
	}
	c.sess.addPendingInteraction(pending)
}

// claudeInteractionKind maps an inbound subtype onto the card kind that renders
// it. AskUserQuestion rides `can_use_tool` like any other permission ask, but it
// is a question, not an approval — so it renders as one.
func claudeInteractionKind(subtype, toolName string) (string, bool) {
	switch subtype {
	case claudeSubtypeCanUseTool:
		if toolName == claudeAskUserQuestionTool {
			return kindUserInput, true
		}
		return kindToolPermission, true
	case claudeSubtypeElicitation:
		return kindElicitation, true
	}
	return "", false
}

// claudeRefusal is the "no" a given subtype's own response shape spells.
func claudeRefusal(subtype string) any {
	switch subtype {
	case claudeSubtypeRequestUserDialog:
		return map[string]any{"status": "cancelled"}
	case claudeSubtypeCanUseTool:
		return map[string]any{"behavior": "deny", "message": "nano cannot render this request"}
	}
	return map[string]any{"status": "cancelled"}
}

// respondControl answers an inbound control request. Wire-recorded like every
// other frame, so the Event Feed carries the whole exchange.
func (c *claudeSession) respondControl(requestID string, payload any) error {
	return c.writeControlResponse(map[string]any{
		"subtype": claudeControlSuccess, "request_id": requestID, "response": payload,
	})
}

func (c *claudeSession) respondControlError(requestID, message string) error {
	return c.writeControlResponse(map[string]any{
		"subtype": claudeControlError, "request_id": requestID, "error": message,
	})
}

func (c *claudeSession) writeControlResponse(response map[string]any) error {
	line, err := json.Marshal(map[string]any{"type": claudeControlResponseType, "response": response})
	if err != nil {
		return err
	}
	c.sess.recordWire(protocol.Send, line)
	return c.tr.sendLine(line)
}
