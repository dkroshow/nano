// interactions.go — server→client requests as first-class, server-owned
// pending interactions (the settled 2026-07-09 decision: "interactive prompts
// are first-class objects that UI and external clients can answer
// idempotently").
//
// Nano used to blanket-deny all ten server requests. Three things make that
// wrong: a `{"decision":"denied"}` sent to a connection-level request like an
// auth-token refresh is malformed, an operator has no way to answer anything,
// and the legacy conversationId-keyed pair was not routed to a session at all.
//
// The shape of the fix:
//
//   - Decision-shaped, thread-scoped requests are PARKED. The handler returns
//     protocol.Deferred, which frees the shared daemon read loop immediately —
//     blocking it while a human decides would stall every codex session in
//     nano (internal/wire/codex/client.go).
//   - Connection-level requests get a per-method correct answer right there on
//     the read loop, and never appear as a dialog.
//   - Pending interactions live in session state, not in wire inference: the
//     wire buffer is bounded, so a tab opened late cannot rediscover a parked
//     request by replay. /state carries the truth; a synthetic feed event is
//     the nudge for tabs that are already open.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dkroshow/nano/internal/httpapi"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/providers"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

const (
	methodCommandApproval     = "item/commandExecution/requestApproval"
	methodFileChangeApproval  = "item/fileChange/requestApproval"
	methodPermissionsApproval = "item/permissions/requestApproval"
	methodRequestUserInput    = "item/tool/requestUserInput"
	methodElicitation         = "mcpServer/elicitation/request"
	methodDynamicToolCall     = "item/tool/call"
	methodAuthTokenRefresh    = "account/chatgptAuthTokens/refresh"
	methodAttestationGenerate = "attestation/generate"
	methodLegacyExecApproval  = "execCommandApproval"
	methodLegacyPatchApproval = "applyPatchApproval"
)

// interactionKind is what the one dialog card switches its body on. Provider-
// generic on purpose: slice 3 renders claude's dialogs through the same card.
const (
	kindCommandApproval    = "commandApproval"
	kindFileChangeApproval = "fileChangeApproval"
	kindPermissions        = "permissions"
	kindUserInput          = "userInput"
	kindElicitation        = "elicitation"
	// kindToolPermission is claude's `can_use_tool` ask for anything that is not
	// a question: one tool, one input, approve or deny. It is deliberately not
	// kindCommandApproval — that kind offers a session-scoped "Always allow",
	// and claude's control channel has no such grant to make.
	kindToolPermission = "toolPermission"
)

// interactionKinds maps each decision-shaped method onto the card kind that
// renders it. A method absent here is not a dialog.
var interactionKinds = map[string]string{
	methodCommandApproval:     kindCommandApproval,
	methodFileChangeApproval:  kindFileChangeApproval,
	methodPermissionsApproval: kindPermissions,
	methodRequestUserInput:    kindUserInput,
	methodElicitation:         kindElicitation,
	methodLegacyExecApproval:  kindCommandApproval,
	methodLegacyPatchApproval: kindFileChangeApproval,
}

// Uniform decisions the card offers. Each method translates them into its own
// wire vocabulary (interactionDecisionResult) — the card never speaks a
// provider verb.
const (
	decisionAccept           = "accept"
	decisionAcceptForSession = "acceptForSession"
	decisionDecline          = "decline"
	decisionCancel           = "cancel"
)

// pendingInteraction is one parked server request, as the UI and any external
// client see it. Params are carried verbatim: the card renders from the wire
// payload rather than from a nano re-description of it.
type pendingInteraction struct {
	ID        string          `json:"id"`     // the JSON-RPC request id, verbatim
	Method    string          `json:"method"` // the schema method name
	Kind      string          `json:"kind"`
	ThreadID  string          `json:"threadId"`
	TurnID    string          `json:"turnId,omitempty"`
	ItemID    string          `json:"itemId,omitempty"`
	CreatedAt string          `json:"createdAt"`
	ExpiresAt string          `json:"expiresAt,omitempty"` // autoResolutionMs deadline
	Params    json.RawMessage `json:"params"`
}

// interactionRouting is what one server request says about where it belongs.
// threadID is empty for connection-level requests, which never become dialogs.
type interactionRouting struct {
	threadID string
	turnID   string
	itemID   string
	expires  string
}

// routeInteraction reads the routing fields out of a request's params. The
// legacy pair (execCommandApproval / applyPatchApproval) carries
// `conversationId` instead of `threadId`, which is exactly why nano never
// routed it — threadIDFromParams looked for the wrong key.
func routeInteraction(params json.RawMessage) interactionRouting {
	var p struct {
		ThreadID         string  `json:"threadId"`
		ConversationID   string  `json:"conversationId"`
		TurnID           string  `json:"turnId"`
		ItemID           string  `json:"itemId"`
		CallID           string  `json:"callId"`
		AutoResolutionMs *uint64 `json:"autoResolutionMs"`
	}
	if json.Unmarshal(params, &p) != nil {
		return interactionRouting{}
	}
	out := interactionRouting{threadID: p.ThreadID, turnID: p.TurnID, itemID: p.ItemID}
	if out.threadID == "" {
		out.threadID = p.ConversationID
	}
	if out.itemID == "" {
		out.itemID = p.CallID
	}
	if p.AutoResolutionMs != nil {
		out.expires = time.Now().Add(time.Duration(*p.AutoResolutionMs) * time.Millisecond).Format(time.RFC3339Nano)
	}
	return out
}

// handleServerRequest is the daemon's OnServerRequest. It runs on the shared
// read loop, so every branch either answers in microseconds or defers.
func (d *codexDaemon) handleServerRequest(requestID string, method string, params json.RawMessage) (any, *protocol.RPCError) {
	if result, rpcErr, handled := connectionLevelResponse(method); handled {
		return result, rpcErr
	}
	kind, isDialog := interactionKinds[method]
	if !isDialog {
		// A thread-scoped request nano has no answer for. Refusing it by name
		// beats a decision verb that does not belong to its schema.
		log.Printf("server request %s: unsupported by nano", method)
		return nil, &protocol.RPCError{Code: -32601, Message: "nano does not implement " + method}
	}
	routing := routeInteraction(params)
	sess := d.sessionForThreadID(routing.threadID)
	if sess == nil {
		// Nobody can answer this. Declining immediately is the only honest
		// move: parking it would hold a turn open with no operator attached.
		log.Printf("server request %s for unrouted thread %q — declining", method, routing.threadID)
		return interactionDecisionResult(method, decisionDecline, params, nil)
	}
	pending := pendingInteraction{
		ID: requestID, Method: method, Kind: kind,
		ThreadID: routing.threadID, TurnID: routing.turnID, ItemID: routing.itemID,
		CreatedAt: time.Now().Format(time.RFC3339Nano), ExpiresAt: routing.expires,
		Params: append(json.RawMessage(nil), params...),
	}
	if sess.rejectStatusInteraction(pending) {
		return interactionDecisionResult(method, decisionDecline, params, nil)
	}
	sess.addPendingInteraction(pending)
	return protocol.Deferred, nil
}

// connectionLevelResponse answers the two requests that belong to the
// connection rather than to any thread. Neither is decision-shaped: an auth
// refresh wants {accessToken, chatgptAccountId} and attestation wants {token},
// so replying `{"decision":"denied"}` — what nano used to send — is malformed
// and, for the auth arm, could break the daemon's own auth handling. Nano holds
// neither credential, so the correct answer is a clean method-level refusal.
func connectionLevelResponse(method string) (any, *protocol.RPCError, bool) {
	switch method {
	case methodAuthTokenRefresh:
		return nil, &protocol.RPCError{
			Code:    -32601,
			Message: "nano holds no ChatGPT credentials and cannot refresh auth tokens",
		}, true
	case methodAttestationGenerate:
		return nil, &protocol.RPCError{
			Code:    -32601,
			Message: "nano cannot generate a client attestation token",
		}, true
	case methodDynamicToolCall:
		// Thread-scoped, but not a decision: the server is asking the CLIENT to
		// run a tool. Nano hosts no client-side tools, so it reports the failure
		// in the response's own vocabulary rather than erroring the turn.
		return map[string]any{
			"success": false,
			"contentItems": []map[string]any{
				{"type": "inputText", "text": "nano does not implement client-side dynamic tools"},
			},
		}, nil, true
	}
	return nil, nil, false
}

func (sess *session) respondInteraction(ctx context.Context, p pendingInteraction, decision string, answers json.RawMessage) (model.Batch, error) {
	if err := sess.ensureProviderCoordinator(); err != nil {
		return model.Batch{}, err
	}
	scope := sess.deliveryScope()
	if p.ThreadID != "" {
		scope.NativeThreadID = &p.ThreadID
	}
	if p.TurnID != "" {
		scope.NativeTurnID = &p.TurnID
	}
	request := model.Item{ID: p.ID, Scope: scope, Detail: append(json.RawMessage(nil), p.Params...)}
	return sess.core.RespondInteraction(ctx, providers.InteractionResponse{
		Request: request, RequestID: p.ID, Method: p.Method, Params: p.Params,
		Kind: providers.InteractionKind(p.Kind), Decision: decision, Answers: answers,
	})
}

// claudeInteractionResult builds the control response for one answered claude
// request. Verified end-to-end against claude CLI 2.1.220 on 2026-07-31.
//
// The one shape worth stating plainly: AskUserQuestion's answers are keyed by
// the question's own TEXT, not by an id — the tool has no question ids, and the
// CLI reads `answers[question.question]`. Answering with the wrong key produces
// a perfectly successful turn in which the model is told "The user did not
// answer the questions", which is the failure mode this comment exists to
// prevent someone rediscovering.
func claudeInteractionResult(p pendingInteraction, decision string, answers json.RawMessage) (any, *protocol.RPCError) {
	switch p.Kind {
	case kindUserInput:
		input, questions := claudeToolInput(p.Params)
		if decision == decisionAccept || decision == decisionAcceptForSession {
			chosen, err := claudeQuestionAnswers(answers, questions)
			if err != nil {
				return nil, &protocol.RPCError{Code: -32602, Message: err.Error()}
			}
			input["answers"] = chosen
		} else {
			// Skip. Allowing the tool with no answers is what produces the CLI's
			// own "The user did not answer the questions" result — a denial would
			// instead surface as a tool error, which is not what skipping a
			// question means.
			input["answers"] = map[string]any{}
		}
		return map[string]any{"behavior": "allow", "updatedInput": input}, nil
	case kindToolPermission:
		if decision == decisionAccept || decision == decisionAcceptForSession {
			input, _ := claudeToolInput(p.Params)
			return map[string]any{"behavior": "allow", "updatedInput": input}, nil
		}
		return map[string]any{"behavior": "deny", "message": "The operator declined this tool use."}, nil
	case kindElicitation:
		action := map[string]string{
			decisionAccept:           "accept",
			decisionAcceptForSession: "accept",
			decisionDecline:          "decline",
			decisionCancel:           "cancel",
		}[decision]
		if action == "" {
			return nil, &protocol.RPCError{Code: -32602, Message: "unknown decision " + decision}
		}
		out := map[string]any{"action": action}
		if action == "accept" && len(answers) > 0 {
			var content any
			if json.Unmarshal(answers, &content) == nil {
				out["content"] = content
			}
		}
		return out, nil
	}
	return nil, &protocol.RPCError{Code: -32601, Message: "no response shape for claude " + p.Kind}
}

// claudeToolInput returns the request's tool input verbatim — the object the
// response must echo back as updatedInput — plus its questions, when the tool
// is one that asks any. The input is copied rather than rebuilt: the CLI
// round-trips fields nano has no business reinterpreting, and dropping one
// would change what the tool ran with.
func claudeToolInput(params json.RawMessage) (map[string]any, []claudeQuestion) {
	var p struct {
		Input json.RawMessage `json:"input"`
	}
	input := map[string]any{}
	if json.Unmarshal(params, &p) != nil || len(p.Input) == 0 {
		return input, nil
	}
	_ = json.Unmarshal(p.Input, &input)
	var decoded struct {
		Questions []claudeQuestion `json:"questions"`
	}
	_ = json.Unmarshal(p.Input, &decoded)
	return input, decoded.Questions
}

type claudeQuestion struct {
	Question    string `json:"question"`
	MultiSelect bool   `json:"multiSelect"`
}

// claudeQuestionAnswers maps the card's uniform `{key: [labels]}` payload onto
// the CLI's `{question text: label | [labels]}`. A single-select question takes
// the bare label — an array there reads as an unrecognized answer.
func claudeQuestionAnswers(raw json.RawMessage, questions []claudeQuestion) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("answers are required to answer a question")
	}
	var loose map[string][]string
	if err := json.Unmarshal(raw, &loose); err != nil {
		return nil, fmt.Errorf("answers must be an object of label lists keyed by question")
	}
	multi := map[string]bool{}
	for _, q := range questions {
		multi[q.Question] = q.MultiSelect
	}
	out := make(map[string]any, len(loose))
	for question, labels := range loose {
		if len(labels) == 0 {
			continue
		}
		if multi[question] {
			out[question] = labels
			continue
		}
		out[question] = labels[0]
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("answers are required to answer a question")
	}
	return out, nil
}

// interactionDecisionResult translates one of the card's uniform decisions into
// the response body a given method actually requires. Verified against the
// current schemas — notably NOT the M1 verb map, whose `reject` is not in any
// of them: the new approvals use accept/acceptForSession/decline/cancel, while
// the legacy pair still uses ReviewDecision's approved/approved_for_session/
// denied/abort.
func interactionDecisionResult(method, decision string, params json.RawMessage, answers json.RawMessage) (any, *protocol.RPCError) {
	switch method {
	case methodCommandApproval, methodFileChangeApproval:
		verb, ok := map[string]string{
			decisionAccept:           "accept",
			decisionAcceptForSession: "acceptForSession",
			decisionDecline:          "decline",
			decisionCancel:           "cancel",
		}[decision]
		if !ok {
			return nil, &protocol.RPCError{Code: -32602, Message: "unknown decision " + decision}
		}
		return map[string]any{"decision": verb}, nil
	case methodLegacyExecApproval, methodLegacyPatchApproval:
		verb, ok := map[string]string{
			decisionAccept:           "approved",
			decisionAcceptForSession: "approved_for_session",
			decisionDecline:          "denied",
			decisionCancel:           "abort",
		}[decision]
		if !ok {
			return nil, &protocol.RPCError{Code: -32602, Message: "unknown decision " + decision}
		}
		return map[string]any{"decision": verb}, nil
	case methodPermissionsApproval:
		// This one has no decision verb at all: the response IS the granted
		// profile (PermissionsRequestApprovalResponse), whose shape matches the
		// requested profile field for field. Accepting echoes what was asked
		// for; declining grants an empty profile, which is how "no" is said
		// here. acceptForSession is the same grant with session scope.
		granted := map[string]any{}
		scope := "turn"
		switch decision {
		case decisionAccept, decisionAcceptForSession:
			var p struct {
				Permissions json.RawMessage `json:"permissions"`
			}
			if json.Unmarshal(params, &p) == nil && len(p.Permissions) > 0 {
				_ = json.Unmarshal(p.Permissions, &granted)
			}
			if decision == decisionAcceptForSession {
				scope = "session"
			}
		case decisionDecline, decisionCancel:
		default:
			return nil, &protocol.RPCError{Code: -32602, Message: "unknown decision " + decision}
		}
		return map[string]any{"permissions": granted, "scope": scope}, nil
	case methodElicitation:
		verb, ok := map[string]string{
			decisionAccept:           "accept",
			decisionAcceptForSession: "accept",
			decisionDecline:          "decline",
			decisionCancel:           "cancel",
		}[decision]
		if !ok {
			return nil, &protocol.RPCError{Code: -32602, Message: "unknown decision " + decision}
		}
		out := map[string]any{"action": verb}
		if verb == "accept" && len(answers) > 0 {
			var content any
			if json.Unmarshal(answers, &content) == nil {
				out["content"] = content
			}
		}
		return out, nil
	case methodRequestUserInput:
		// Not a decision at all — ToolRequestUserInputResponse is
		// {answers: {questionId: {answers: [string]}}}. Declining still has to
		// produce that shape, so it answers every question with nothing.
		if decision == decisionAccept || decision == decisionAcceptForSession {
			answerMap, err := userInputAnswers(answers)
			if err != nil {
				return nil, &protocol.RPCError{Code: -32602, Message: err.Error()}
			}
			return map[string]any{"answers": answerMap}, nil
		}
		return map[string]any{"answers": emptyUserInputAnswers(params)}, nil
	}
	return nil, &protocol.RPCError{Code: -32601, Message: "no response shape for " + method}
}

// userInputAnswers validates the client's answers into the response shape.
// Accepts either the full {qid: {answers: [...]}} envelope or the flat
// {qid: [...]} the card finds natural, so the browser never has to build a
// nested wrapper by hand.
func userInputAnswers(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("answers are required to accept %s", methodRequestUserInput)
	}
	var loose map[string]json.RawMessage
	if err := json.Unmarshal(raw, &loose); err != nil {
		return nil, fmt.Errorf("answers must be an object keyed by question id")
	}
	out := make(map[string]any, len(loose))
	for questionID, value := range loose {
		var wrapped struct {
			Answers []string `json:"answers"`
		}
		if json.Unmarshal(value, &wrapped) == nil && wrapped.Answers != nil {
			out[questionID] = map[string]any{"answers": wrapped.Answers}
			continue
		}
		var flat []string
		if err := json.Unmarshal(value, &flat); err != nil {
			return nil, fmt.Errorf("answers for question %s must be a list of strings", questionID)
		}
		out[questionID] = map[string]any{"answers": flat}
	}
	return out, nil
}

// emptyUserInputAnswers builds a well-formed "no answer" for every question the
// request asked, so a declined prompt still satisfies the required shape.
func emptyUserInputAnswers(params json.RawMessage) map[string]any {
	var p struct {
		Questions []struct {
			ID string `json:"id"`
		} `json:"questions"`
	}
	out := map[string]any{}
	if json.Unmarshal(params, &p) != nil {
		return out
	}
	for _, question := range p.Questions {
		if question.ID != "" {
			out[question.ID] = map[string]any{"answers": []string{}}
		}
	}
	return out
}

// serverRequestResolvedMethod is how the app-server announces that a server
// request it sent is finished — observed live 2026-07-30, immediately after
// nano's own answer, and it is also what arrives when the request is resolved
// WITHOUT nano: the autoResolutionMs timeout on requestUserInput, or another
// client answering. That makes it the protocol's own silent-collapse signal, so
// the card never has to guess from a clock.
const serverRequestResolvedMethod = "serverRequest/resolved"

// handleNotification watches for resolutions of requests nano is holding. It
// runs on the read loop, so it does map work only.
func (d *codexDaemon) handleNotification(method string, params json.RawMessage) {
	if method == "thread/settings/updated" {
		var p struct {
			ThreadID       string `json:"threadId"`
			ThreadSettings struct {
				Model          string          `json:"model"`
				Effort         string          `json:"effort"`
				ServiceTier    string          `json:"serviceTier"`
				Cwd            string          `json:"cwd"`
				ApprovalPolicy json.RawMessage `json:"approvalPolicy"`
			} `json:"threadSettings"`
		}
		if json.Unmarshal(params, &p) != nil {
			return
		}
		sess := d.sessionForThreadID(p.ThreadID)
		if sess == nil {
			return
		}
		values := map[string]string{
			postureFieldModel:       p.ThreadSettings.Model,
			postureFieldEffort:      p.ThreadSettings.Effort,
			postureFieldServiceTier: p.ThreadSettings.ServiceTier,
			"cwd":                   p.ThreadSettings.Cwd,
		}
		var approval string
		_ = json.Unmarshal(p.ThreadSettings.ApprovalPolicy, &approval)
		if approval != "" {
			values[postureFieldPermissionMode] = approval
		}
		entry, known := sess.takeSettingsCausation(settingsSemanticKey(values))
		var snapshot providerCausationSnapshot
		if known {
			snapshot = entry.Snapshot
		}
		sess.recordProviderEvidence("thread_settings_updated", values, metadataFreshnessLive, snapshot)
		return
	}
	if method != serverRequestResolvedMethod {
		return
	}
	var p struct {
		ThreadID  string          `json:"threadId"`
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.RequestID) == 0 {
		return
	}
	sess := d.sessionForThreadID(p.ThreadID)
	if sess == nil {
		return
	}
	// Already claimed by our own answer in the normal case — this is the
	// no-op half of an idempotent resolution, and the real work only when
	// something else resolved it.
	if pending, ok := sess.claimPendingInteraction(string(p.RequestID)); ok {
		sess.noteInteractionResolved(pending, "", "resolvedElsewhere")
	}
}

// settingsSemanticKey is intentionally derived from decoded schema fields,
// not raw JSON bytes: key ordering and whitespace are transport accidents and
// must not turn a retried notification into new provider evidence.
func settingsSemanticKey(values map[string]string) string {
	return fmt.Sprintf("model=%q\x00effort=%q\x00tier=%q\x00cwd=%q\x00approval=%q",
		values[postureFieldModel], values[postureFieldEffort], values[postureFieldServiceTier], values["cwd"], values[postureFieldPermissionMode])
}

// sessionForThreadID resolves the session a thread-scoped request belongs to.
func (d *codexDaemon) sessionForThreadID(threadID string) *session {
	if threadID == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessions[threadID]
}

// respondToInteraction writes a deferred answer back to the app-server. The
// idempotency lives one layer down: protocol.Client claims the id, so a second
// tab (or a teardown racing a click) gets ErrRequestNotDeferred instead of
// writing a second response.
func (d *codexDaemon) respondToInteraction(requestID string, result any, rpcErr *protocol.RPCError) error {
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	if client == nil {
		return protocol.ErrClientClosed
	}
	return client.RespondToServerRequest(requestID, result, rpcErr)
}

// --- session-owned pending state --------------------------------------------

// nano-origin feed events. They ride the session's own wire stream so an open
// tab learns about a request the instant it lands, without polling /state.
const (
	interactionPendingEvent  = "nano/interaction/pending"
	interactionResolvedEvent = "nano/interaction/resolved"
)

// Parking, claiming and draining all change `waiting` — whether the thread is
// blocked on the operator rather than working — and that is runtime state the
// catalog stream carries (runtimeInfo.Waiting). The session-scoped feed events
// below reach an OPEN PANE only; without the runtime nudge the compact rail
// would keep ticking an elapsed timer at a thread that is standing still,
// which is exactly what D15 forbids.
func (sess *session) addPendingInteraction(p pendingInteraction) {
	if err := sess.recordPermissionRequest(p); err != nil {
		log.Printf("session %s: record permission request %s: %v", sess.id, p.ID, err)
	}
	sess.stateMu.Lock()
	if sess.interactions == nil {
		sess.interactions = map[string]pendingInteraction{}
	}
	sess.interactions[p.ID] = p
	sess.observeInteractionLocked(lifecycle.EventWaiting, p.TurnID)
	sess.stateMu.Unlock()
	if sess.projection != nil {
		sess.projection.upsertInteraction(p)
	}
	sess.recordNanoEvent(interactionPendingEvent, map[string]any{"interaction": p})
	sess.notifyRuntime()
	if sess.agentExecutions != nil && sess.hostedThreadID != "" {
		if err := sess.agentExecutions.markWaiting(sess.hostedThreadID, p.TurnID, true); err != nil {
			log.Printf("mark hosted thread %s waiting: %v", sess.hostedThreadID, err)
		}
	}
}

func (sess *session) configureStatusClassifier() {
	sess.stateMu.Lock()
	sess.autoRejectInteractions = true
	sess.stateMu.Unlock()
}

func (sess *session) statusInteractionEpoch() int64 {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.rejectedInteractionSeq
}

// rejectStatusInteraction records the abnormal request without ever adding it
// to sess.interactions or the live projection. The provider-specific ingress
// returns its own decline shape immediately; the status dispatcher observes
// the epoch change and interrupts the classifier turn.
func (sess *session) rejectStatusInteraction(p pendingInteraction) bool {
	sess.stateMu.Lock()
	if !sess.autoRejectInteractions {
		sess.stateMu.Unlock()
		return false
	}
	sess.rejectedInteractionSeq++
	sess.stateMu.Unlock()
	if err := sess.recordPermissionRequest(p); err != nil {
		log.Printf("session %s: record hidden status interaction %s: %v", sess.id, p.ID, err)
	}
	if err := sess.recordPermissionResolution(p, decisionDecline, "nano_status_classifier", "autoDeclined"); err != nil {
		log.Printf("session %s: record hidden status resolution %s: %v", sess.id, p.ID, err)
	}
	return true
}

// takePendingInteraction claims one pending interaction. The claim is the
// idempotency point on nano's side: a second answer finds nothing and is told
// so, which is what makes a second tab's click collapse rather than double-send.
func (sess *session) takePendingInteraction(id string) (pendingInteraction, bool) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	p, ok := sess.interactions[id]
	if ok {
		delete(sess.interactions, id)
		if len(sess.interactions) == 0 {
			sess.observeInteractionLocked(lifecycle.EventAnswered, "")
		}
	}
	if ok && sess.projection != nil {
		sess.projection.removeInteraction(id)
	}
	return p, ok
}

// claimPendingInteraction is takePendingInteraction plus the runtime nudge a
// successful claim owes the catalog: the thread stops waiting and goes back to
// working, and the rail's timer must resume with it.
func (sess *session) claimPendingInteraction(id string) (pendingInteraction, bool) {
	p, ok := sess.takePendingInteraction(id)
	if ok {
		sess.notifyRuntime()
		if sess.agentExecutions != nil && sess.hostedThreadID != "" {
			remaining := len(sess.pendingInteractionList()) > 0
			if err := sess.agentExecutions.markWaiting(sess.hostedThreadID, p.TurnID, remaining); err != nil {
				log.Printf("resume hosted thread %s after interaction: %v", sess.hostedThreadID, err)
			}
		}
	}
	return p, ok
}

// drainPendingInteractions removes and returns everything still parked —
// teardown's list of requests it must answer before unsubscribing.
func (sess *session) drainPendingInteractions() []pendingInteraction {
	sess.stateMu.Lock()
	out := make([]pendingInteraction, 0, len(sess.interactions))
	for _, p := range sess.interactions {
		out = append(out, p)
	}
	sess.interactions = nil
	sess.observeInteractionLocked(lifecycle.EventAnswered, "")
	sess.stateMu.Unlock()
	if sess.projection != nil {
		for _, interaction := range out {
			sess.projection.removeInteraction(interaction.ID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return timestampBefore(out[i].CreatedAt, out[j].CreatedAt) })
	if len(out) > 0 {
		sess.notifyRuntime()
		if sess.agentExecutions != nil && sess.hostedThreadID != "" {
			if err := sess.agentExecutions.markWaiting(sess.hostedThreadID, "", false); err != nil {
				log.Printf("clear hosted thread %s interactions: %v", sess.hostedThreadID, err)
			}
		}
	}
	return out
}

// pendingInteractionList is the /state snapshot — the authoritative copy for a
// tab that opened after the request landed and so cannot find it by replay.
func (sess *session) pendingInteractionList() []pendingInteraction {
	sess.stateMu.Lock()
	out := make([]pendingInteraction, 0, len(sess.interactions))
	for _, p := range sess.interactions {
		out = append(out, p)
	}
	sess.stateMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return timestampBefore(out[i].CreatedAt, out[j].CreatedAt) })
	return out
}

func (sess *session) noteInteractionResolved(p pendingInteraction, decision, outcome string) {
	if err := sess.recordPermissionResolution(p, decision, permissionDecisionSource(decision, outcome), outcome); err != nil {
		log.Printf("session %s: record permission resolution %s: %v", sess.id, p.ID, err)
	}
	sess.recordNanoEvent(interactionResolvedEvent, map[string]any{
		"id": p.ID, "method": p.Method, "kind": p.Kind, "outcome": outcome,
	})
}

// closeInteractions answers everything still parked before the session lets go
// of its thread. An unanswered request would otherwise hold the turn open in a
// daemon that outlives nano, with no operator anywhere able to reach it.
// "cancel" rather than "decline": the session is going away, so continuing the
// turn without the answer is not what the operator asked for.
func (sess *session) closeInteractions() {
	for _, p := range sess.drainPendingInteractions() {
		if _, err := sess.respondInteraction(context.Background(), p, decisionCancel, nil); err != nil {
			log.Printf("session %s: cancelling parked %s: %v", sess.id, p.Method, err)
		}
		sess.noteInteractionResolved(p, decisionCancel, "sessionClosed")
	}
}

// --- HTTP -------------------------------------------------------------------

func (sess *session) handleInteraction(w http.ResponseWriter, r *http.Request, requestID string) {
	if requestID = strings.TrimSpace(requestID); requestID == "" {
		httpapi.WriteHTTPError(w, "request id required", http.StatusBadRequest)
		return
	}
	var body struct {
		Decision string          `json:"decision"`
		Answers  json.RawMessage `json:"answers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteHTTPError(w, "body must be {\"decision\": \"…\", \"answers\": {…}}", http.StatusBadRequest)
		return
	}
	p, ok := sess.claimPendingInteraction(requestID)
	if !ok {
		httpapi.WriteErrorEnvelope(w, http.StatusConflict, httpapi.ErrorEnvelope{Error: httpapi.Error{
			Code: "already_resolved", Message: "Interaction is already resolved", Details: map[string]string{"requestId": requestID},
		}})

		return
	}
	if rpcErr := validateInteractionAnswer(sess.provider, p, body.Decision, body.Answers); rpcErr != nil {
		// Reject malformed input before reaching the provider edge. This also
		// keeps the parked request available when the runtime is still attaching.
		sess.addPendingInteraction(p)
		httpapi.WriteHTTPError(w, rpcErr.Message, http.StatusBadRequest)
		return
	}
	_, responseErr := sess.respondInteraction(r.Context(), p, body.Decision, body.Answers)
	var rpcErr *protocol.RPCError
	if errors.As(responseErr, &rpcErr) && rpcErr.Code == -32602 {
		// A malformed answer must not consume the request: put it back so the
		// operator can try again rather than losing the dialog to a typo.
		sess.addPendingInteraction(p)
		httpapi.WriteHTTPError(w, rpcErr.Message, http.StatusBadRequest)
		return
	}
	if responseErr != nil {
		// The far side already moved on (or the connection died). The
		// interaction is genuinely over either way, so report the same
		// collapse-silently status rather than resurrecting a dead dialog.
		sess.noteInteractionResolved(p, body.Decision, "unreachable")
		httpapi.WriteErrorEnvelope(w, http.StatusConflict, httpapi.ErrorEnvelope{Error: httpapi.Error{
			Code: "already_resolved", Message: responseErr.Error(), Details: map[string]string{"requestId": requestID},
		}})

		return
	}
	sess.noteInteractionResolved(p, body.Decision, body.Decision)
	httpapi.WriteJSON(w, map[string]any{"answered": true, "id": requestID, "decision": body.Decision})
}

func validateInteractionAnswer(provider string, pending pendingInteraction, decision string, answers json.RawMessage) *protocol.RPCError {
	var rpcErr *protocol.RPCError
	switch provider {
	case "codex":
		_, rpcErr = interactionDecisionResult(pending.Method, decision, pending.Params, answers)
	case "claude":
		_, rpcErr = claudeInteractionResult(pending, decision, answers)
	case "grok":
		_, rpcErr = grokInteractionResult(pending, decision, answers)
	}
	if rpcErr != nil && rpcErr.Code == -32602 {
		return rpcErr
	}
	return nil
}
