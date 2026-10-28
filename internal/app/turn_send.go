package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/transcript"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"log"
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sendAttemptQueued                      = "queued"
	sendAttemptSending                     = "sending"
	sendAttemptAccepted                    = "accepted"
	sendAttemptFailed                      = "failed"
	sendAttemptRefused                     = "refused"
	sendAttemptTimeout                     = "timeout"
	sendAttemptInterruptedBeforeTranscript = "interrupted_before_transcript"
	maxSendAttempts                        = 100
	turnDeliveryTimeout                    = 30 * time.Second
	// promptSlowDelivery is how long a prompt may wait for its provider to
	// take it before its row warns that sending it again delivers it twice.
	// A healthy provider takes a prompt well within it.
	promptSlowDelivery = 5 * time.Second
	// deliveryExpiryWindow is how long one expired delivery deadline keeps
	// its provider marked slow in every pane of that provider.
	deliveryExpiryWindow = 5 * time.Minute
)

var (
	errQueuedTurnNotFound   = errors.New("queued turn not found")
	errQueuedTurnDelivering = errors.New("queued turn is already being delivered")
	// errPromptEnded answers a cancel for a prompt already saved, discarded or
	// refused: nothing is left to end. The person's Discard takes it as done.
	errPromptEnded = errors.New("the prompt has already ended")
)

// sendAttemptRecord is the schema-20 compatibility shape exposed by the read
// proxy below. Production delivery writes only through internal/core/prompts.Store;
// this shape deliberately contains no prompt text.
type sendAttemptRecord = prompts.SendAttempt

// sendAttemptStore is the schema-20 compatibility read view of the journal;
// like queuedTurnStore it reads through the transactional store.
type sendAttemptStore struct {
	path  string
	db    *sql.DB
	runID string
	turns *prompts.Store
}

func newSendAttemptStore(db *sql.DB, path, runID string, store *prompts.Store) (*sendAttemptStore, error) {
	if store == nil {
		return nil, fmt.Errorf("threads: send attempt view requires the prompt store")
	}
	return &sendAttemptStore{db: db, path: path, runID: runID, turns: store}, nil
}

func (s *sendAttemptStore) get(actionID string) (sendAttemptRecord, bool) {
	if s == nil {
		return sendAttemptRecord{}, false
	}
	record, attempt, err := s.turns.Get(context.Background(), actionID)
	if err == nil {
		return sendAttemptRecordFromTransactional(record, attempt), true
	}
	attempt, err = s.turns.GetAttempt(context.Background(), actionID)
	return sendAttemptRecordFromTransactional(prompts.Record{}, attempt), err == nil
}

// promptJournal is the prompt store's two listing reads, served by the
// writer-backed store or by a read-only prompts.Journal.
type promptJournal interface {
	List(context.Context, string) ([]prompts.Record, error)
	ListThreadAttempts(context.Context, model.Provider, string) ([]prompts.AttemptStatus, error)
}

func (s *sendAttemptStore) listThread(provider, nativeThreadID string) []sendAttemptRecord {
	if s == nil || nativeThreadID == "" {
		return nil
	}
	rows := make([]sendAttemptRecord, 0)
	attempts, err := s.turns.ListThreadAttempts(context.Background(), model.Provider(provider), nativeThreadID)
	if err != nil {
		return nil
	}
	for _, entry := range attempts {
		if entry.OutboxState == prompts.Expired {
			continue // hidden for good; its journal stays in the store
		}
		record := prompts.Record{Outbox: prompts.OutboxEntry{State: entry.OutboxState}}
		rows = append(rows, sendAttemptRecordFromTransactional(record, entry.Attempt))
	}
	sort.Slice(rows, func(i, j int) bool { return timestampBefore(rows[j].UpdatedAt, rows[i].UpdatedAt) })
	return rows
}

func sendAttemptRecordFromTransactional(record prompts.Record, attempt prompts.Attempt) sendAttemptRecord {
	status := string(attempt.State)
	switch attempt.State {
	case prompts.JournalUnknown:
		status = "unknown"
	case prompts.JournalConfirmed:
		status = sendAttemptAccepted
	}
	if attempt.State == prompts.JournalRefused && attempt.Reason == sendAttemptInterruptedBeforeTranscript {
		status = sendAttemptInterruptedBeforeTranscript
	}
	if record.Outbox.State == prompts.Parked {
		status = "parked"
	}
	if record.Outbox.State == prompts.Uncertain && attempt.Reason == prompts.ReasonStopped {
		status = "unknown" // held by a Stop, for the person to resend or discard
	}
	return sendAttemptRecord{
		ClientActionID: attempt.ActionID, Provider: string(attempt.Provider), NativeThreadID: attempt.NativeThreadID,
		TextHash: attempt.TextHash, Status: status, RunID: attempt.RunID, Reason: attempt.Reason, CreatedAt: attempt.CreatedAt, UpdatedAt: attempt.UpdatedAt,
		Confirmed: attempt.Confirmed, ConfirmedAt: attempt.ConfirmedAt, ProviderTurnID: attempt.ProviderTurnID,
		ProviderTerminalStatus: attempt.ProviderTerminal, ProviderTerminalAt: attempt.ProviderTerminalAt,
		CanonicalMessageID: attempt.CanonicalItemID, IdentityConflict: attempt.IdentityConflict,
	}
}

func sendTextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// turnRequestHash is the idempotency hash of one submission. Holding a prompt
// for the next turn is part of the request, so a retry that changes it
// conflicts; an ordinary prompt keeps its plain text hash. The prefix keeps
// the two namespaces apart: no text hashes to a next-turn hash.
func turnRequestHash(text string, nextTurn bool) string {
	if nextTurn {
		return "next_turn:" + sendTextHash(text)
	}
	return sendTextHash(text)
}

type turnSendOperation struct {
	actionID         string
	textHash         string
	providerText     string
	presentationText string
	claimToken       string
	attachmentEpoch  uint64
	wasQueued        bool
	done             chan struct{}
	status           string
	reason           string
	// runningTurnID is the turn this prompt joins; empty starts a turn.
	runningTurnID string
}

func (sess *session) handleTurn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text           string `json:"text"`
		ClientActionID string `json:"clientActionId"`
		NextTurn       bool   `json:"nextTurn,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		httpapi.WriteHTTPError(w, "body must be {\"text\": \"...\", \"clientActionId\": \"...\"}", http.StatusBadRequest)
		return
	}
	if req.ClientActionID == "" {
		req.ClientActionID = newSessionID()
	}
	if len(req.ClientActionID) > 200 {
		httpapi.WriteHTTPError(w, "clientActionId is too long", http.StatusBadRequest)
		return
	}
	// Action-id dedup is deliberately before readiness. A browser retry of a
	// request Nano already accepted must be answered idempotently even while the
	// provider is still in its boot window.
	if rec, ok := sess.sendAttempts.get(req.ClientActionID); ok {
		if rec.Provider != sess.provider || rec.TextHash != turnRequestHash(req.Text, req.NextTurn) {
			writeTurnDeliveryFailure(w, http.StatusConflict, req.ClientActionID, "clientActionId does not match this message")
			return
		}
		sess.sendMu.Lock()
		inFlight := sess.sendOps[req.ClientActionID]
		sess.sendMu.Unlock()
		if inFlight != nil {
			writeTurnOperationResult(w, r, inFlight)
			return
		}
		currentRunID := ""
		if sess.sendAttempts != nil {
			currentRunID = sess.sendAttempts.runID
		}
		if handled := writeExistingAttempt(w, rec, currentRunID); handled {
			return
		}
	}
	sess.stateMu.Lock()
	status := sess.connectionStatusLocked()
	sess.stateMu.Unlock()
	// Starting is a durable queueing state: provider setup and a boot replay
	// can both take time, but neither makes the operator's submitted text
	// invalid. queueOrStartTurnSend persists it and markTransportReady drains it.
	if sess.rejectFailedReconnect(w) {
		return
	}
	recoveringCodex := sess.codex != nil && (status == "reconnecting" || status == "reconnect_parked")
	if status != "ready" && status != "starting" && !recoveringCodex {
		httpapi.WriteHTTPError(w, "session is "+status, http.StatusConflict)
		return
	}

	delivered := ""
	if queued, ok := sess.queuedTurns.get(req.ClientActionID); ok && queued.Text != "" {
		// A retry replays the exact provider-bound text already committed to the
		// outbox. In particular, reading mail between attempts must not rewrite
		// or remove the original unread footer.
		delivered = queued.Text
	} else {
		var err error
		delivered, err = sess.promptWithUnreadFooter(req.Text)
		if err != nil {
			httpapi.WriteError(w, http.StatusServiceUnavailable, "unread_footer_unavailable", err.Error())
			return
		}
	}
	sess.noteHumanPrompt(time.Now().UTC())
	op, responseStatus, reason, err := sess.queueOrStartTurnSendPreparedPresentation(req.ClientActionID, delivered, delivered, turnRequestHash(req.Text, req.NextTurn), turnOriginHuman, req.NextTurn)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusConflict)
		return
	}
	if responseStatus != 0 {
		if responseStatus >= http.StatusBadRequest {
			httpapi.WriteHTTPError(w, reason, responseStatus)
			return
		}
		writeTurnSendResponse(w, turnSendResult{status: responseStatus, actionID: req.ClientActionID, reason: reason})
		return
	}
	writeTurnOperationResult(w, r, op)
}

func writeTurnOperationResult(w http.ResponseWriter, r *http.Request, op *turnSendOperation) {
	select {
	case <-r.Context().Done():
		// Browser navigation and the 15s UI watchdog do not own the provider
		// operation. A retry with this action id attaches to op.done.
		return
	case <-op.done:
		if op.status == sendAttemptAccepted {
			writeTurnSendResponse(w, turnSendResult{status: http.StatusOK, actionID: op.actionID})
			return
		}
		if op.status == sendAttemptTimeout {
			writeTurnSendResponse(w, turnSendResult{status: http.StatusAccepted, actionID: op.actionID, reason: "delivery_timeout"})
			return
		}
		if op.status == "parked" {
			// Nano holds it for the person, as a retry of this action is told.
			writeTurnSendResponse(w, turnSendResult{status: http.StatusAccepted, actionID: op.actionID, reason: "queued_delivery_failed"})
			return
		}
		writeTurnDeliveryFailure(w, http.StatusFailedDependency, op.actionID, op.reason)
	}
}

func writeExistingAttempt(w http.ResponseWriter, rec sendAttemptRecord, currentRunID string) bool {
	result := turnSendResult{
		actionID: rec.ClientActionID, confirmed: rec.Confirmed, confirmedAt: rec.ConfirmedAt,
	}
	if rec.Confirmed {
		result.status = http.StatusOK
		writeTurnSendResponse(w, result)
		return true
	}
	switch rec.Status {
	case sendAttemptAccepted:
		result.status = http.StatusOK
		writeTurnSendResponse(w, result)
		return true
	case sendAttemptQueued, sendAttemptSending, sendAttemptTimeout, "unknown", "parked":
		result.status = http.StatusAccepted
		result.reason = "delivery_pending"
		if rec.Status == sendAttemptQueued {
			result.reason = "queued_for_next_turn"
		} else if rec.Status == "parked" {
			result.reason = "queued_delivery_failed"
		}
		if rec.Status == sendAttemptSending && rec.RunID != "" && currentRunID != "" && rec.RunID != currentRunID {
			result.reason = "unknown_after_restart"
		}
		if rec.Status == sendAttemptTimeout {
			result.reason = "delivery_timeout"
		}
		writeTurnSendResponse(w, result)
		return true
	case sendAttemptRefused, sendAttemptFailed, sendAttemptInterruptedBeforeTranscript:
		reason := rec.Reason
		if rec.Status == sendAttemptInterruptedBeforeTranscript && reason == "" {
			reason = sendAttemptInterruptedBeforeTranscript
		}
		writeTurnDeliveryFailure(w, http.StatusFailedDependency, rec.ClientActionID, reason)
		return true
	default:
		return false
	}
}

// handleTurnDeliveryList only displays the journal, so it reads through the
// control store's read pool: after a restart every open tab reads every
// session's journal, and through the writer those reads queued in front of
// registrations and durable writes. A journal row with no prompt record
// predates the prompt store and has no state to show; a discarded or expired
// prompt is hidden for good.
func (sess *session) handleTurnDeliveryList(w http.ResponseWriter, r *http.Request) {
	threadID := r.URL.Query().Get("thread")
	if threadID == "" {
		threadID = sess.currentThreadID()
	}
	views := make([]promptView, 0)
	if sess.sendAttempts == nil || sess.sendAttempts.turns == nil || threadID == "" {
		httpapi.WriteJSON(w, map[string]any{"operations": views})
		return
	}
	var journal promptJournal = sess.sendAttempts.turns
	if sess.metadata != nil {
		if pool := sess.metadata.readPool(); pool != nil {
			journal = prompts.NewJournal(pool)
		}
	}
	rows, err := journal.ListThreadAttempts(r.Context(), model.Provider(sess.provider), threadID)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	texts := make(map[string]string)
	for _, queued := range sess.queuedTurns.listThreadFrom(journal, sess.provider, threadID, sess.hostedThreadID) {
		texts[queued.ClientActionID] = queued.Text
	}
	for _, row := range rows {
		if row.OutboxState == "" || row.OutboxState == prompts.Expired {
			continue
		}
		view := sess.promptView(prompts.OutboxEntry{ActionID: row.Attempt.ActionID, State: row.OutboxState, Reason: row.Attempt.Reason, UpdatedAt: row.Attempt.UpdatedAt}, row.Attempt)
		view.Text = texts[row.Attempt.ActionID]
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return timestampBefore(views[j].UpdatedAt, views[i].UpdatedAt) })
	httpapi.WriteJSON(w, map[string]any{"operations": views})
}

// handleTurnDeliveryRead reports one prompt without creating or retrying it,
// as the journal lists it. The browser's request watchdog is deliberately
// shorter than the provider-delivery deadline, so this is how a tab whose
// answer was lost learns what Nano did with its send.
func (sess *session) handleTurnDeliveryRead(w http.ResponseWriter, r *http.Request, actionID string) {
	if actionID == "" || len(actionID) > 200 {
		httpapi.WriteHTTPError(w, "clientActionId is required", http.StatusBadRequest)
		return
	}
	if sess.turns != nil {
		record, attempt, err := sess.turns.Get(r.Context(), actionID)
		if err == nil && string(record.Provider) == sess.provider {
			httpapi.WriteJSON(w, sess.promptView(record.Outbox, attempt))
			return
		}
	}
	httpapi.WriteError(w, http.StatusNotFound, "not_found", "Nano does not have this prompt")
}

func (sess *session) handleTurnDeliveryCancel(w http.ResponseWriter, _ *http.Request, actionID string) {
	sess.cancelQueuedTurn(w, actionID)
}

// Actions a person may take on a prompt.
const (
	promptResend  = "resend"
	promptDiscard = "discard"
)

// promptActionRefusal decides whether the person may take action on a prompt
// in its current stored state: "" when they may, otherwise the reason they
// are shown. Only Nano knows whether a request is still with the provider, so
// this is the one place that decides; the row's buttons are its allowed
// actions (promptPresentation) and the Resend and Discard routes ask it again
// under the send lock. Discard of a prompt that has already ended is not
// decided here: the end the person asked for already holds.
func promptActionRefusal(provider string, entry prompts.OutboxEntry, action string) string {
	name := providerDisplayName(provider)
	verb := map[string]string{promptResend: "resent", promptDiscard: "discarded"}[action]
	switch entry.State {
	case prompts.Uncertain, prompts.Parked:
		return ""
	case prompts.Queued:
		if action == promptDiscard {
			return ""
		}
		return "This prompt is queued: " + name + " gets it when the running turn ends."
	case prompts.Delivering:
		return "Nano is handing this prompt to " + name + " now, so it can't be " + verb + "."
	case prompts.Accepted, prompts.Confirmed:
		return name + " already has this prompt, so it can't be " + verb + "."
	}
	return "This prompt has ended, so it can't be " + verb + "."
}

func providerDisplayName(provider string) string {
	if name := map[string]string{"codex": "Codex", "claude": "Claude", "grok": "Grok"}[provider]; name != "" {
		return name
	}
	return "The provider"
}

// slowDeliveryAt is when a prompt Nano is still handing over becomes slow:
// promptSlowDelivery after delivery claimed it.
func slowDeliveryAt(entry prompts.OutboxEntry) (time.Time, bool) {
	if entry.State != prompts.Delivering {
		return time.Time{}, false
	}
	claimed, err := time.Parse(time.RFC3339Nano, entry.UpdatedAt)
	if err != nil {
		return time.Time{}, false
	}
	return claimed.Add(promptSlowDelivery), true
}

// promptPresentation is what a person sees of a prompt in its current stored
// state: the actions they may take and a short label saying why. The live
// stream and the delivery journal both apply it to the same stored record, so
// they cannot disagree. A prompt the provider has needs no label, nor does one
// Nano is handing over until the provider is slow to take it: then sending the
// same words again, the natural move, would deliver them twice.
func promptPresentation(provider string, entry prompts.OutboxEntry) (actions []string, label string) {
	for _, action := range []string{promptResend, promptDiscard} {
		if promptActionRefusal(provider, entry, action) == "" {
			actions = append(actions, action)
		}
	}
	name := providerDisplayName(provider)
	switch entry.State {
	case prompts.Queued:
		return actions, "Queued for the next turn"
	case prompts.Delivering:
		if slow, ok := slowDeliveryAt(entry); ok && !time.Now().Before(slow) {
			return actions, name + " hasn't taken it yet; sending again will deliver twice"
		}
		return actions, ""
	case prompts.Parked, prompts.Refused:
		label = map[prompts.DeliveryState]string{prompts.Parked: "Not delivered", prompts.Refused: "Not sent"}[entry.State]
		if entry.Reason != "" {
			label += ": " + entry.Reason
		}
		return actions, label
	case prompts.Uncertain:
		switch {
		case entry.Reason == prompts.ReasonStopped:
			return actions, "Stopped before " + name + " took it"
		case entry.Reason == prompts.ReasonTurnEnded:
			return actions, name + "'s turn ended without it"
		case strings.Contains(entry.Reason, context.DeadlineExceeded.Error()):
			// The warning a Resend of it needs: the provider may still take it.
			return actions, fmt.Sprintf("%s hasn't answered in %d s; resending may deliver twice", name, int(turnDeliveryTimeout.Seconds()))
		}
		return actions, "Delivery not confirmed"
	}
	return actions, ""
}

// promptActionRefused is the person's Resend or Discard refused from the
// prompt's stored state, with the reason they are shown.
type promptActionRefused struct {
	actionID string
	state    prompts.DeliveryState
	reason   string
}

func (e *promptActionRefused) Error() string { return e.reason }

func refusePromptAction(provider string, record prompts.Record, action string) error {
	if reason := promptActionRefusal(provider, record.Outbox, action); reason != "" {
		return &promptActionRefused{actionID: record.Outbox.ActionID, state: record.Outbox.State, reason: reason}
	}
	return nil
}

// writePromptActionRefusal answers a refused action: its reason, shown to the
// person verbatim, and the state it was refused in.
func writePromptActionRefusal(w http.ResponseWriter, refusal *promptActionRefused) {
	httpapi.WriteErrorEnvelope(w, http.StatusConflict, httpapi.ErrorEnvelope{Error: httpapi.Error{
		Code: "prompt_action_refused", Message: refusal.reason,
		Details: map[string]string{"clientActionId": refusal.actionID, "state": string(refusal.state)},
	}})
}

// promptView is one prompt as the delivery journal serves it: its stored
// state and the actions and label promptPresentation gives it, the fields the
// live stream carries, with the identity of its delivery.
type promptView struct {
	ClientActionID     string   `json:"clientActionId"`
	Provider           string   `json:"provider"`
	NativeThreadID     string   `json:"nativeThreadId"`
	TextHash           string   `json:"textHash"`
	Text               string   `json:"text,omitempty"`
	State              string   `json:"state"`
	AllowedActions     []string `json:"allowedActions,omitempty"`
	Label              string   `json:"label,omitempty"`
	CreatedAt          string   `json:"createdAt"`
	UpdatedAt          string   `json:"updatedAt"`
	ProviderTurnID     string   `json:"providerTurnId,omitempty"`
	CanonicalMessageID string   `json:"canonicalMessageId,omitempty"`
	IdentityConflict   string   `json:"identityConflict,omitempty"`
}

func (sess *session) promptView(entry prompts.OutboxEntry, attempt prompts.Attempt) promptView {
	view := promptView{
		ClientActionID: entry.ActionID, Provider: string(attempt.Provider), NativeThreadID: attempt.NativeThreadID,
		TextHash: attempt.TextHash, State: string(entry.State), CreatedAt: attempt.CreatedAt, UpdatedAt: attempt.UpdatedAt,
		ProviderTurnID: attempt.ProviderTurnID, CanonicalMessageID: attempt.CanonicalItemID, IdentityConflict: attempt.IdentityConflict,
	}
	view.AllowedActions, view.Label = promptPresentation(sess.provider, entry)
	return view
}

func (sess *session) queueOrStartTurnSend(actionID, text string) (*turnSendOperation, int, string, error) {
	return sess.queueOrStartTurnSendOrigin(actionID, text, turnOriginHuman)
}

// queueOrStartTurnSendOrigin is the origin-aware entry point. The browser
// path submits human-origin prompts; the agent API submits agent- or
// system-origin prompts. Origin decides queue class (queued_turn_store.go).
func (sess *session) queueOrStartTurnSendOrigin(actionID, text, origin string) (*turnSendOperation, int, string, error) {
	return sess.queueOrStartTurnSendPrepared(actionID, text, sendTextHash(text), origin)
}

func (sess *session) queueOrStartTurnSendPrepared(actionID, text, requestHash, origin string) (*turnSendOperation, int, string, error) {
	return sess.queueOrStartTurnSendPreparedPresentation(actionID, text, text, requestHash, origin, false)
}

// queueOrStartTurnSendPreparedPresentation separates the provider-bound text
// from Nano's browser row. Agent attribution and inbox markers belong only to
// the provider transport; the durable caller prompt remains the presentation.
// nextTurn holds an ordinary prompt until the running turn ends.
func (sess *session) queueOrStartTurnSendPreparedPresentation(actionID, text, presentationText, requestHash, origin string, nextTurn bool) (*turnSendOperation, int, string, error) {
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	return sess.queueOrStartTurnSendLockedPreparedPresentation(actionID, text, presentationText, requestHash, origin, nextTurn)
}

func (sess *session) queueOrStartTurnSendLocked(actionID, text string) (*turnSendOperation, int, string, error) {
	return sess.queueOrStartTurnSendLockedOrigin(actionID, text, turnOriginHuman)
}

func (sess *session) queueOrStartTurnSendLockedOrigin(actionID, text, origin string) (*turnSendOperation, int, string, error) {
	return sess.queueOrStartTurnSendLockedPrepared(actionID, text, sendTextHash(text), origin)
}

func (sess *session) queueOrStartTurnSendLockedPrepared(actionID, text, requestHash, origin string) (*turnSendOperation, int, string, error) {
	return sess.queueOrStartTurnSendLockedPreparedPresentation(actionID, text, text, requestHash, origin, false)
}

func (sess *session) queueOrStartTurnSendLockedPreparedPresentation(actionID, text, presentationText, requestHash, origin string, nextTurn bool) (*turnSendOperation, int, string, error) {
	if sess.discardedUnknown[actionID] {
		// The person discarded it before this attempt reached Nano.
		return nil, http.StatusConflict, "discarded", nil
	}
	origin = normalizeTurnOrigin(origin)
	if presentationText == "" {
		presentationText = text
	}
	deliveredHash := sendTextHash(text)
	if requestHash == "" {
		requestHash = deliveredHash
	}
	permission, refusal := sess.submissionPermissionLocked()
	if refusal != "" {
		return nil, http.StatusConflict, refusal, nil
	}
	return sess.admitTransactionalTurnLocked(actionID, text, presentationText, requestHash, origin, nextTurn, permission)
}

// submissionPermissionLocked is whether this session admits a prompt now: the
// lifecycle's submission permission, or the reason it does not.
func (sess *session) submissionPermissionLocked() (lifecycle.Submission, string) {
	sess.stateMu.Lock()
	status, permission := sess.connectionStatusLocked(), sess.lifecyclePermissionLocked()
	activationPending := sess.recoveryActivationPending
	sess.stateMu.Unlock()
	switch {
	case activationPending:
		return permission.Submit, "native_recovery_not_activated"
	case status == sessionStatusExited:
		return permission.Submit, "session is exited"
	case permission.Submit == lifecycle.RejectInvalidState:
		return permission.Submit, status
	}
	return permission.Submit, ""
}

func (sess *session) admitTransactionalTurnLocked(actionID, text, presentationText, requestHash, origin string, nextTurn bool, permission lifecycle.Submission) (*turnSendOperation, int, string, error) {
	if err := sess.ensureDeliveryCoordinator(); err != nil {
		return nil, 0, "", err
	}
	result, err := sess.core.Admit(context.Background(), prompts.Submission{
		HostedThreadID: sess.hostedThreadID, Provider: model.Provider(sess.provider), NativeThreadID: sess.currentThreadID(),
		ActionID: actionID, ProviderText: text, PresentationText: presentationText,
		TextHash: sendTextHash(text), RequestHash: requestHash, Origin: prompts.Origin(origin), NextTurn: nextTurn,
	}, permission)
	if err != nil {
		return nil, 0, "", fmt.Errorf("admit transactional turn: %w", err)
	}
	record := result.Record
	if result.Refused {
		return nil, http.StatusConflict, string(record.Outbox.State), nil
	}
	if record.Outbox.State != prompts.Queued {
		return nil, turnRecordHTTPStatus(record), string(record.Outbox.State), nil
	}
	return sess.startAdmittedTurnLocked(actionID, permission)
}

// startAdmittedTurnLocked starts delivery of a prompt just stored as queued,
// as the session's permission allows: now, into the running turn, or for the
// next turn.
func (sess *session) startAdmittedTurnLocked(actionID string, permission lifecycle.Submission) (*turnSendOperation, int, string, error) {
	if permission == lifecycle.QueuePrompt && !sess.corePromptInFlight() {
		if op, status, reason, err := sess.claimAndStartTransactionalTurnLocked(actionID, false, true); err != nil || op != nil {
			return op, status, reason, err
		}
	}
	if permission != lifecycle.SendNow || sess.corePromptInFlight() {
		if sess.codex != nil {
			sess.codex.requestSessionRecovery(sess)
		}
		return nil, http.StatusAccepted, "queued_for_next_turn", nil
	}
	return sess.claimAndStartTransactionalTurnLocked(actionID, false, false)
}

func (sess *session) nanoIsStopping() bool {
	return sess.nanoStopping != nil && sess.nanoStopping.Load()
}

func turnRecordHTTPStatus(record prompts.Record) int {
	switch record.Outbox.State {
	case prompts.Accepted, prompts.Confirmed:
		return http.StatusOK
	case prompts.Refused:
		return http.StatusFailedDependency
	default:
		return http.StatusAccepted
	}
}

// claimAndStartTransactionalTurnLocked claims the next prompt and starts its
// delivery. With runningTurn it claims only a prompt that may join the turn
// running now, and names that turn so the provider refuses any other.
func (sess *session) claimAndStartTransactionalTurnLocked(requestActionID string, queuedDelivery, runningTurn bool) (*turnSendOperation, int, string, error) {
	if err := sess.ensureDeliveryCoordinator(); err != nil {
		return nil, 0, "", err
	}
	// A stopping Nano starts no delivery its exit could cut off. The prompt
	// stays queued, and the next process delivers it once.
	if sess.nanoIsStopping() {
		return nil, http.StatusAccepted, "queued_for_next_turn", nil
	}
	sess.stateMu.Lock()
	epoch := sess.lifecycleState.Epoch
	sess.stateMu.Unlock()
	claim, runningTurnID := sess.core.Claim, ""
	if runningTurn {
		if _, runningTurnID = sess.activeTurn(); runningTurnID == "" {
			return nil, http.StatusAccepted, "queued_for_next_turn", nil
		}
		claim = sess.core.ClaimRunningTurn
	}
	record, claimed, err := claim(context.Background(), epoch)
	if err != nil {
		return nil, 0, "", err
	}
	if !claimed {
		return nil, http.StatusAccepted, "queued_for_next_turn", nil
	}
	op := sess.startTransactionalTurnLocked(record, queuedDelivery || record.Outbox.ActionID != requestActionID, runningTurnID)
	if record.Outbox.ActionID != requestActionID {
		return nil, http.StatusAccepted, "queued_for_next_turn", nil
	}
	return op, 0, "", nil
}

func (sess *session) startTransactionalTurnLocked(record prompts.Record, wasQueued bool, runningTurnID string) *turnSendOperation {
	if sess.sendOps == nil {
		sess.sendOps = map[string]*turnSendOperation{}
	}
	if len(sess.sendOps) >= maxSendAttempts {
		for id, previous := range sess.sendOps {
			select {
			case <-previous.done:
				delete(sess.sendOps, id)
			default:
			}
			if len(sess.sendOps) < maxSendAttempts {
				break
			}
		}
	}
	op := &turnSendOperation{
		actionID: record.Outbox.ActionID, textHash: record.RequestHash,
		providerText: record.ProviderText, presentationText: record.Outbox.PresentationText, claimToken: record.ClaimToken,
		attachmentEpoch: record.ClaimEpoch, wasQueued: wasQueued, runningTurnID: runningTurnID,
		done: make(chan struct{}), status: sendAttemptSending,
	}
	sess.sendOps[op.actionID] = op
	go sess.runTransactionalTurnSend(op, record)
	return op
}

func (sess *session) runTransactionalTurnSend(op *turnSendOperation, claimed prompts.Record) {
	ctx, cancel := context.WithTimeout(context.Background(), turnDeliveryTimeout)
	defer cancel()
	scope := sess.deliveryScope()
	if op.runningTurnID != "" {
		scope.NativeTurnID = &op.runningTurnID
	}
	codex := sess.provider == string(model.ProviderCodex)
	if codex {
		sess.expectCodexEcho(codexEchoWait{actionID: op.actionID, claimToken: op.claimToken, claimEpoch: op.attachmentEpoch, scope: scope})
	}
	delivery, deliveryErr := sess.core.Deliver(ctx, claimed, scope, sess.deliverySettings(), op.wasQueued)
	providerResult := delivery.Provider
	result := providerResult.Receipt
	nativeMissing := providerResult.FailureCode == codexNativeMissing
	accepted := deliveryErr == nil && delivery.Record.Outbox.State == prompts.Accepted
	// A running turn that ended first never saw the prompt: it waits again,
	// for the next turn, and its operation is still only queued.
	requeued := deliveryErr == nil && delivery.Record.Outbox.State == prompts.Queued

	switch delivery.Record.Outbox.State {
	case prompts.Queued:
		op.status, op.reason = sendAttemptQueued, providerResult.FailureCode
	case prompts.Accepted:
		op.status = sendAttemptAccepted
	case prompts.Parked:
		op.status = "parked"
		op.reason = providerResult.Reason
		if nativeMissing {
			op.reason = codexNativeMissing
		}
	case prompts.Refused:
		op.status, op.reason = sendAttemptRefused, providerResult.Reason
	case prompts.Uncertain:
		switch providerResult.State {
		case prompts.Refused:
			op.status, op.reason = sendAttemptRefused, providerResult.Reason
		case prompts.Parked:
			op.status, op.reason = "parked", providerResult.Reason
			if nativeMissing {
				op.reason = codexNativeMissing
			}
		default:
			op.status, op.reason = sendAttemptTimeout, "unknown_after_restart"
		}
	default:
		op.status, op.reason = sendAttemptTimeout, "unknown_after_restart"
	}
	if deliveryErr != nil {
		op.status, op.reason = sendAttemptTimeout, "unknown_after_restart"
	} else if !accepted && !requeued && providerResult.Reason != "" {
		deliveryErr = errors.New(providerResult.Reason)
		if ctx.Err() != nil {
			deliveryErr = ctx.Err() // the provider did not answer in time
		}
	}
	if codex {
		sess.deliveredCodexEcho(op.actionID, delivery.Record.Outbox.State == prompts.Uncertain && delivery.Record.Outbox.Binding == nil)
	}
	// Any attempt may have reached the provider; a pass drops what cannot settle.
	sess.awaitSavedPrompt(op.actionID)

	turnID := providerTurnIDFromStart(result)
	if accepted {
		if op.runningTurnID == "" {
			sess.acceptProviderTurn(turnID)
		}
		sess.stateMu.Lock()
		sess.codexZeroTurnEligible = false
		sess.stateMu.Unlock()
		if observe := lifecycleProviders[sess.provider].accepted; observe != nil && op.runningTurnID == "" {
			observe(sess)
		}
	}
	definiteFailure := delivery.Record.Outbox.State == prompts.Refused || delivery.Record.Outbox.State == prompts.Parked
	if !requeued {
		sess.recordAgentTurnDelivery(op.actionID, result, deliveryErr, definiteFailure)
	}
	if release := lifecycleProviders[sess.provider].release; release != nil {
		release(sess)
	}
	if nativeMissing {
		if err := sess.containCodexNativeMissing(deliveryErr.Error()); err != nil {
			log.Printf("settle native-missing session %s: %v", sess.id, err)
		}
	}
	close(op.done)
	sess.kickQueuedTurnDrain()
	// The provider can save the whole turn before its receipt returns, and
	// then no later commit settles it: catch up once, off the reply path.
	sess.settleWaitingPrompts(context.Background())
}

func (sess *session) publishTransactionalOutbox(entry prompts.OutboxEntry) {
	sess.liveText.outboxChanged(entry)
}

// restoreSubmissionOutbox recreates the presentation rows owned by unsettled
// prompt records, after settling those the saved transcript already proves.
func (sess *session) restoreSubmissionOutbox() {
	sess.restoreQueuedTurnBootDiagnostics()
	if err := sess.ensureDeliveryCoordinator(); err != nil {
		log.Printf("restore transactional turn coordinator %s: %v", sess.hostedThreadID, err)
		return
	}
	settleCtx, cancel := context.WithTimeout(context.Background(), savedPromptSettleTimeout)
	sess.settleSavedPrompts(settleCtx)
	cancel()
	sess.stateMu.Lock()
	epoch := sess.lifecycleState.Epoch
	sess.stateMu.Unlock()
	restored, err := sess.core.Restore(context.Background(), epoch)
	if err != nil {
		log.Printf("restore transactional turn outbox %s: %v", sess.hostedThreadID, err)
		return
	}
	for _, diagnostic := range restored.Diagnostics {
		log.Printf("restore transactional turn %s: %s", sess.hostedThreadID, diagnostic.Code)
	}
}

// savedPromptSettleTimeout bounds the exact-key saved reads one settlement
// makes; on timeout every prompt stays published.
const savedPromptSettleTimeout = 10 * time.Second

// awaitSavedPrompt marks a delivered prompt for settlement on a later saved
// commit.
func (sess *session) awaitSavedPrompt(actionID string) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	if sess.settleWaiting == nil {
		sess.settleWaiting = make(map[string]bool)
	}
	sess.settleWaiting[actionID] = true
}

// settleWaitingPrompts settles saved prompts after a saved commit, but only
// while a delivered prompt waits for its saved row or a Claude prompt still
// owes its member completion: a thread with neither reads nothing.
func (sess *session) settleWaitingPrompts(ctx context.Context) {
	sess.stateMu.Lock()
	waiting := len(sess.settleWaiting) > 0
	sess.stateMu.Unlock()
	// A Claude member that still owes its completion also needs the pass:
	// its own saved commit may have been the last ordinary waiter's.
	if !waiting && !sess.owedHint.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, savedPromptSettleTimeout)
	defer cancel()
	// Commits come often while a turn runs; log only the ones that settle.
	outcomes, err := sess.settlePrompts(ctx)
	if err != nil {
		log.Printf("settle saved prompts %s: %v", sess.hostedThreadID, err)
	} else if outcomes[transcript.SettlementSettled] > 0 {
		log.Printf("settle saved prompts %s: outcomes=%v", sess.hostedThreadID, outcomes)
	}
}

// settleSavedPrompts settles each delivered prompt whose user row the
// provider's saved transcript holds, proven by the prompt's own exact provider
// identity. A settled prompt is never published again; a prompt that cannot
// be proven, for any reason, stays published. It reads only this thread's
// saved rows that carry those identities, and logs counts, never text. The
// error is why the saved rows could not be read.
func (sess *session) settleSavedPrompts(ctx context.Context) error {
	outcomes, err := sess.settlePrompts(ctx)
	if err != nil {
		log.Printf("settle saved prompts %s: %v", sess.hostedThreadID, err)
	} else if outcomes != nil {
		log.Printf("settle saved prompts %s: outcomes=%v", sess.hostedThreadID, outcomes)
	}
	return err
}

// settlePrompts is one settlement pass; it reports each outcome's count, nil
// when the thread had no prompt to settle, or why its evidence was unreadable.
//
// Afterwards a prompt waits for a later saved commit only while one could
// still settle it: its saved turn is open, or this attachment delivered it
// and the provider has not saved it yet. Any other unproven prompt, such as
// one that predates the attachment, waits for the next attach instead, so it
// adds no saved commit of its own; while any Claude prompt owes its member
// completion, though, every saved commit runs a whole pass, which reads it too.
func (sess *session) settlePrompts(ctx context.Context) (map[transcript.Settlement]int, error) {
	if sess == nil || sess.turns == nil || sess.savedPrompts == nil {
		return nil, nil
	}
	sess.settleMu.Lock()
	defer sess.releaseSettle()
	sess.stateMu.Lock()
	waiting := maps.Clone(sess.settleWaiting)
	sess.stateMu.Unlock()
	savedIn := func(ctx context.Context, native string, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
		if native == sess.currentThreadID() || sess.savedPromptsIn == nil {
			return sess.savedPrompts(ctx, keys)
		}
		return sess.savedPromptsIn(ctx, native, keys)
	}
	var resolve func(string) string
	if sess.provider == string(model.ProviderClaude) {
		resolve = sess.claudeResolver(ctx)
	}
	live := &liveSettlement{watched: waiting, sess: sess}
	results, err := settleThreadPromptsResolving(ctx, sess.turns, savedIn, sess.hostedThreadID, sess.provider, sess.currentThreadID(), resolve, live)
	if err != nil {
		return nil, err
	}
	outcomes := map[transcript.Settlement]int{}
	keep := map[string]bool{}
	for actionID, outcome := range results {
		if outcome == settlementChanged || outcome == transcript.SettlementNotTerminal || outcome == transcript.SettlementNotSaved && waiting[actionID] {
			keep[actionID] = true
		}
		if outcome != settlementChanged {
			outcomes[outcome]++
		}
	}
	sess.keepWaiting(waiting, keep)
	if outcomes[settlementResent] > 0 {
		sess.kickQueuedTurnDrain() // a resent prompt waits for the next turn
	}
	claude := sess.provider == string(model.ProviderClaude)
	for actionID, outcome := range results {
		if outcome != transcript.SettlementSettled {
			continue
		}
		if claude {
			sess.owedHint.Store(true) // Settle recorded its owed member completion
			continue
		}
		if record, _, err := sess.turns.Get(ctx, actionID); err == nil && record.ProviderTurnID != "" {
			sess.finishJoinedAgentTurns(ctx, record.ProviderTurnID)
		}
	}
	if claude {
		sess.runMemberStepHeld()
	}
	if len(outcomes) == 0 {
		return nil, nil
	}
	return outcomes, nil
}

// settlementChanged reports a prompt another writer changed during the pass;
// the next pass decides it again.
const settlementChanged transcript.Settlement = "changed"

// settleThreadPrompts settles, by exact identity, the delivered prompts of one
// thread whose user rows saved reads, and reports each candidate's outcome. A
// Claude thread's current native thread names the session a provisional
// binding stood for. It needs no live session.
func settleThreadPrompts(ctx context.Context, store *prompts.Store, saved savedPromptReader, hostedThreadID, provider, current string) (map[string]transcript.Settlement, error) {
	// Only the current thread's rows are read here; a prompt of another
	// native finds none.
	savedIn := func(ctx context.Context, native string, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
		if native != current {
			return nil, nil
		}
		return saved(ctx, keys)
	}
	return settleThreadPromptsResolving(ctx, store, savedIn, hostedThreadID, provider, current, nil, nil)
}

// settleThreadPromptsResolving settles as settleThreadPrompts does, reading
// each Claude prompt's rows in its own exact native thread. With resolve, a
// provisional handle means only the native resolve proves for it; without,
// it means the current native thread. With live, a person's prompt the live
// attachment delivered and still waits on is resent for the next turn if its
// turn is proven to have dropped it (liveSettlement.dropped), unless the
// operator stopped that turn: then it is held for the person. Any other stays
// as it is, for the person to resend or discard.
func settleThreadPromptsResolving(ctx context.Context, store *prompts.Store, savedIn func(context.Context, string, history.SavedUserKeys) ([]history.SavedUserRow, error), hostedThreadID, provider, current string, resolve func(string) string, live *liveSettlement) (map[string]transcript.Settlement, error) {
	records, err := store.List(ctx, hostedThreadID)
	if err != nil {
		return nil, err
	}
	results, err := classifySavedPrompts(ctx, records, savedIn, provider, current, resolve)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		entry := record.Outbox
		outcome, candidate := results[entry.ActionID]
		if !candidate {
			continue
		}
		switch {
		case outcome == transcript.SettlementSettled:
			if _, settled, err := store.Settle(ctx, entry.ActionID); err != nil {
				return nil, err
			} else if !settled {
				outcome = settlementChanged
			}
		case outcome == transcript.SettlementNotSaved && live.dropped(provider, record):
			// One loss decision, then whether the operator stopped the turn,
			// asked now: a Stop is recorded before the interrupt that made
			// the loss provable.
			if live.stoppedTurn(record) {
				held, ok, err := store.HoldUntaken(ctx, entry.ActionID, record.Revision, prompts.ReasonStopped)
				if err != nil {
					return nil, err
				}
				if !ok {
					outcome = settlementChanged
					break
				}
				outcome = settlementHeld
				live.sess.liveText.outboxChanged(held.Outbox)
				break
			}
			// Nano resends by itself only a prompt still accepted; one held
			// already waits for the person.
			if record.Outbox.State != prompts.Accepted {
				outcome = settlementChanged
				break
			}
			retired, resent, ok, err := store.Resend(ctx, entry.ActionID, record.Revision, true)
			if err != nil {
				return nil, err
			}
			if !ok {
				outcome = settlementChanged
				break
			}
			outcome = settlementResent
			live.sess.publishResent(retired, resent)
		case outcome == transcript.SettlementNotSaved && record.Outbox.State == prompts.Accepted &&
			(live.turnEnded(ctx, record) || claudeTakenAfter(provider, record, records, results, current, resolve)):
			held, ok, err := store.HoldUntaken(ctx, entry.ActionID, record.Revision, prompts.ReasonTurnEnded)
			if err != nil {
				return nil, err
			}
			if !ok {
				outcome = settlementChanged
				break
			}
			outcome = settlementHeld
			if live != nil {
				live.sess.liveText.outboxChanged(held.Outbox)
			}
		}
		results[entry.ActionID] = outcome
	}
	return results, nil
}

// classifySavedPrompts reads exact saved-input evidence without changing the
// journal. NotTerminal proves input storage, but never completion authority.
func classifySavedPrompts(ctx context.Context, records []prompts.Record, savedIn savedPromptNativeReader, provider, current string, resolve func(string) string) (map[string]transcript.Settlement, error) {
	stored := make([]prompts.OutboxEntry, 0, len(records))
	var candidates []prompts.OutboxEntry
	keys := map[string]*history.SavedUserKeys{}
	for _, record := range records {
		entry := record.Outbox
		native := current
		if provider == string(model.ProviderClaude) {
			entry = claudeSettlementEntry(entry, record.NativeThreadID, current, resolve)
			if entry.Binding != nil && entry.Binding.Scope.NativeThreadID != nil {
				native = *entry.Binding.Scope.NativeThreadID
			}
		}
		stored = append(stored, entry)
		if record.Outbox.State != prompts.Accepted && record.Outbox.State != prompts.Uncertain || record.ClaimToken != "" {
			continue
		}
		candidates = append(candidates, entry)
		if keys[native] == nil {
			keys[native] = &history.SavedUserKeys{}
		}
		addSavedUserKey(keys[native], entry.Binding, provider)
	}
	results := make(map[string]transcript.Settlement, len(candidates))
	var savedRows []transcript.SavedRow
	for native, nativeKeys := range keys {
		if len(nativeKeys.NativeTurnIDs) == 0 && len(nativeKeys.NativeEventIDs) == 0 {
			continue
		}
		rows, err := savedIn(ctx, native, *nativeKeys)
		if err != nil {
			return nil, fmt.Errorf("saved rows unavailable: %w", err)
		}
		for _, row := range rows {
			savedRows = append(savedRows, transcript.SavedRow{Item: row.Item, Terminal: durableTerminalStatus(string(row.TurnStatus))})
		}
	}
	for _, entry := range candidates {
		results[entry.ActionID] = transcript.SavedSettlement(entry, stored, savedRows)
	}
	return results, nil
}

// addSavedUserKey adds the lookup for the saved user rows a prompt's own
// binding can match. Codex saves a prompt's event id on the second copy of its
// user row, not the copy that locates the row, so its rows are read by saved
// turn; SavedSettlement still matches each prompt by its own event.
func addSavedUserKey(keys *history.SavedUserKeys, binding *prompts.Binding, provider string) {
	switch {
	case binding == nil:
	case binding.EventID != "" && provider != string(model.ProviderCodex):
		keys.NativeEventIDs = append(keys.NativeEventIDs, binding.EventID)
	case binding.Scope.NativeTurnID != nil:
		keys.NativeTurnIDs = append(keys.NativeTurnIDs, *binding.Scope.NativeTurnID)
	}
}

// settlementResent reports a prompt its provider never took, retired and
// queued again as a new prompt for the next turn.
const settlementResent transcript.Settlement = "resent"

// settlementHeld reports a prompt its provider never took from a turn the
// operator stopped, held for the person to resend or discard.
const settlementHeld transcript.Settlement = "held"

// liveSettlement is what a live attachment adds to a settlement pass: the
// prompts it delivered and still waits on, and the session that saw their
// turns, knows which the operator stopped, and publishes a prompt resent for
// the next turn or held.
type liveSettlement struct {
	watched map[string]bool
	sess    *session
}

// dropped reports a person's prompt this attachment delivered that its Codex
// turn dropped. Codex holds a steered prompt until the turn's next step, and
// records the prompt a turn starts with only once the turn begins work, after
// any context compaction; an interruption before then drops the prompt. Codex
// echoes each prompt it takes, so the proof is this attachment seeing the turn
// from its start end interrupted without an echo of the prompt's client id. A
// restart or reconnect gap proves nothing, nor does a turn that is still open
// or that completed. An agent's prompt is not resent: its operation is bound
// to its own action.
func (l *liveSettlement) dropped(provider string, record prompts.Record) bool {
	binding := record.Outbox.Binding
	return l != nil && provider == string(model.ProviderCodex) && l.watched[record.Outbox.ActionID] &&
		record.Origin == prompts.HumanOrigin && binding != nil && binding.EventID != "" && binding.Scope.NativeTurnID != nil &&
		l.sess.codexTurnDropped(*binding.Scope.NativeTurnID, binding.EventID)
}

// turnEnded reports an accepted prompt, submitted since this process
// started, whose turn the saved transcript shows has ended. The file is
// written in order, so a turn it records as ended holds every prompt that turn
// took: one whose row is missing was not taken, or not yet, and the person
// decides. Live turn ends prove nothing here, since the saved file can trail
// them. A prompt already stored at boot is left as it is: older records may
// have been taken by turns their saved file never named, and turning them all
// into Resend and Discard rows at once would invite duplicates. Claude's saved
// turns carry no id its live turns share, so a Claude prompt is never decided
// this way; claudeTakenAfter is Claude's proof.
func (l *liveSettlement) turnEnded(ctx context.Context, record prompts.Record) bool {
	binding := record.Outbox.Binding
	if l == nil || l.sess.savedTurn == nil || binding == nil || binding.Scope.NativeTurnID == nil {
		return false
	}
	if created, err := time.Parse(time.RFC3339Nano, record.CreatedAt); err != nil || created.Before(processStarted) {
		return false
	}
	saved, found := l.sess.savedTurn(ctx, *binding.Scope.NativeTurnID)
	return found && (saved.Newer || durableTerminalStatus(saved.Turn.Status))
}

// stoppedTurn reports a prompt delivered to a turn the operator stopped. It is
// asked at the decision, not before the pass's saved reads: Stop is recorded
// before the interrupt that produces the evidence.
func (l *liveSettlement) stoppedTurn(record prompts.Record) bool {
	binding := record.Outbox.Binding
	return l != nil && binding != nil && binding.Scope.NativeTurnID != nil && l.sess.turnStopped(*binding.Scope.NativeTurnID)
}

// claudeTakenAfter reports a Claude prompt Claude never took: no saved row
// carries it, yet a prompt Nano handed to the same session after it has one.
// Claude takes the prompts it is handed in order, into the running turn or as
// the next turn's, so one taken after it means this one was lost, as a prompt
// still queued when its process stopped is. Claude's saved file names every
// prompt it took by the prompt's own id, so unlike turnEnded this needs no
// live attachment and decides a prompt stored before this process started.
// Both prompts' sessions are read as settlement reads them: a shim handle
// means the session its recorded resolution names, and an unresolved one
// decides nothing.
func claudeTakenAfter(provider string, record prompts.Record, records []prompts.Record, results map[string]transcript.Settlement, current string, resolve func(string) string) bool {
	spool, handed, ok := claudeHandedAt(record.ProviderTurnID)
	if provider != string(model.ProviderClaude) || !ok {
		return false
	}
	native, ok := exactClaudeNative(claudeSettlementEntry(record.Outbox, record.NativeThreadID, current, resolve))
	if !ok {
		return false
	}
	for _, later := range records {
		laterSpool, laterHanded, ok := claudeHandedAt(later.ProviderTurnID)
		if !ok || laterSpool != spool || laterHanded <= handed {
			continue
		}
		if laterNative, ok := exactClaudeNative(claudeSettlementEntry(later.Outbox, later.NativeThreadID, current, resolve)); !ok || laterNative != native {
			continue
		}
		switch results[later.Outbox.ActionID] {
		case transcript.SettlementSettled, transcript.SettlementNotTerminal:
			return true
		}
		if later.Outbox.State == prompts.Confirmed {
			return true
		}
	}
	return false
}

// claudeHandedAt is where in its shim's spool Nano handed Claude a prompt:
// the shim names the prompt's turn claude:<spool key>:<spool sequence>. A
// legacy Nano-local ordinal (claude:<n>) names no spool.
func claudeHandedAt(turnID string) (spool string, sequence int64, ok bool) {
	cut := strings.LastIndexByte(turnID, ':')
	if cut < 0 || !strings.HasPrefix(turnID, "claude:") || turnID[:cut] == "claude" {
		return "", 0, false
	}
	sequence, err := strconv.ParseInt(turnID[cut+1:], 10, 64)
	return turnID[:cut], sequence, err == nil && sequence > 0
}

// publishResent shows a prompt resent for the next turn in place of the one
// it retires, both in one live-text change.
func (sess *session) publishResent(retired, resent prompts.Record) {
	sess.liveText.outboxChanged(retired.Outbox, resent.Outbox)
}

// keepWaiting records one settlement pass: of the prompts it saw waiting,
// only those it keeps still wait, and each kept prompt waits. A prompt
// delivered during the pass keeps waiting.
func (sess *session) keepWaiting(seen, keep map[string]bool) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	for actionID := range seen {
		if !keep[actionID] {
			delete(sess.settleWaiting, actionID)
		}
	}
	for actionID := range keep {
		if sess.settleWaiting == nil {
			sess.settleWaiting = make(map[string]bool)
		}
		sess.settleWaiting[actionID] = true
	}
	// With nothing waiting, no prompt of an ended stopped turn can be resent;
	// the turn still running may yet take a prompt sent after its Stop.
	if len(sess.settleWaiting) == 0 {
		for turnID := range sess.stoppedTurns {
			if turnID != sess.turnID || !sess.workActiveLocked() {
				delete(sess.stoppedTurns, turnID)
			}
		}
	}
}

// turnStopped reports whether the operator stopped this native turn.
func (sess *session) turnStopped(nativeTurnID string) bool {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sess.stoppedTurns[nativeTurnID]
}

// claudeSettlementEntry gives a Claude prompt its exact saved identity: the
// row UUID Nano stamped, in the prompt's session. A prompt accepted before
// Claude named its session is bound to the shim handle that stood in for it;
// as in the owner's native-thread rebind, that handle means the session that
// followed it, the attachment's current thread.
func claudeSettlementEntry(entry prompts.OutboxEntry, recordThread, current string, resolve func(string) string) prompts.OutboxEntry {
	thread := recordThread
	if entry.Binding != nil && entry.Binding.Scope.NativeThreadID != nil {
		thread = *entry.Binding.Scope.NativeThreadID
	}
	if claudeNativeIDProvisional(thread) {
		resolved := ""
		switch {
		case resolve != nil:
			resolved = resolve(thread)
		case current != "" && !claudeNativeIDProvisional(current):
			resolved = current
		}
		if resolved != "" {
			thread = resolved
			if entry.Binding != nil {
				binding := *entry.Binding
				binding.Scope.NativeThreadID = &thread
				entry.Binding = &binding
			}
		}
	}
	return claudeSubmissionBinding(entry, thread)
}

// claudeSubmissionBinding gives a Claude submission the exact event identity
// Nano stamped on its user frame, in nativeThreadID when the binding names no
// thread. The UUID is derived from durable action identity, so reconnect and
// retry reconstruct the same value without another store. A legacy binding
// with a different explicit event remains authoritative.
func claudeSubmissionBinding(entry prompts.OutboxEntry, nativeThreadID string) prompts.OutboxEntry {
	if entry.HostedThreadID == "" || entry.ActionID == "" {
		return entry
	}
	eventID := claudeSubmissionEventIDForOutbox(entry)
	if eventID == "" {
		return entry
	}
	if entry.Binding == nil {
		if nativeThreadID == "" {
			return entry
		}
		entry.Binding = &prompts.Binding{Scope: model.Scope{
			Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &nativeThreadID,
		}, EventID: eventID}
		return entry
	}
	binding := *entry.Binding
	if binding.Scope.Provider != "" && binding.Scope.Provider != model.ProviderClaude {
		return entry
	}
	binding.Scope.Provider = model.ProviderClaude
	if binding.Scope.NativeThreadID == nil && nativeThreadID != "" {
		binding.Scope.NativeThreadID = &nativeThreadID
	}
	binding.EventID = eventID
	entry.Binding = &binding
	return entry
}

// resolveClaudeHandle is the exact native a provisional handle's prompts
// landed in: the resolution the claim owner recorded, else this session's own
// child when it ran under that handle and announced the current native. Any
// other handle is unresolved, and its prompts wait.
func (sess *session) resolveClaudeHandle(ctx context.Context, handle string) string {
	if sess.metadata != nil {
		native, err := sess.metadata.resolvedNative(ctx, sess.hostedThreadID, sess.provider, handle)
		if err != nil {
			log.Printf("resolve provisional %s of %s: %v", handle, sess.hostedThreadID, err)
		} else if native != "" {
			return native
		}
	}
	current := sess.currentThreadID()
	if c := sess.claude; c != nil && c.tr != nil && c.initSeen.Load() && current != "" && !claudeNativeIDProvisional(current) {
		if rec, ok := c.tr.durableAttachment("", ""); ok && rec.ShimSessionID == handle {
			return current
		}
	}
	return ""
}

// releaseSettle ends a hold of settleMu. A member step a trigger asked for
// while the lock was held (memberDirty) runs after the release, under a new
// hold, so no trigger is lost and none waits on the lock.
func (sess *session) releaseSettle() {
	for {
		sess.settleMu.Unlock()
		if !sess.memberDirty.Load() || !sess.settleMu.TryLock() {
			return
		}
		sess.runMemberStepHeld()
	}
}

// requestMemberStep asks for a member step without waiting for settleMu: the
// request is recorded first, so a holder that releases after a failed TryLock
// here still sees it.
func (sess *session) requestMemberStep() {
	sess.memberDirty.Store(true)
	if sess.settleMu.TryLock() {
		sess.runMemberStepHeld()
		sess.releaseSettle()
	}
}

// runMemberStepHeld runs one member step under settleMu. The request flag is
// cleared before the step reads anything, so a trigger during the step asks
// again; a step that stops on an error or its deadline leaves its owed
// prompts for the next trigger.
func (sess *session) runMemberStepHeld() {
	sess.memberDirty.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), savedPromptSettleTimeout)
	defer cancel()
	sess.memberStepLocked(ctx)
}

func (sess *session) restoreQueuedTurnBootDiagnostics() {
	if sess == nil || sess.projection == nil || sess.queuedTurns == nil {
		return
	}
	for _, parked := range sess.queuedTurns.bootParksFor(sess.provider, sess.currentThreadID(), sess.hostedThreadID) {
		diagnostic := threadDiagnostic{
			Code:     "boot_incomplete_outbox",
			Message:  "durable queued turn was parked because required fields are missing: " + strings.Join(parked.Missing, ","),
			Source:   "turn-store-boot",
			ActionID: parked.ActionID,
		}
		sess.projection.recordDiagnostic(diagnostic, true)
	}
}

func (sess *session) kickQueuedTurnDrain() {
	if sess.queuedTurns != nil {
		go sess.drainQueuedTurn()
	}
}

func (sess *session) drainQueuedTurn() {
	if blocked := lifecycleProviders[sess.provider].ingressBlocked; blocked != nil && blocked(sess) {
		return
	}
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	sess.stateMu.Lock()
	permission := sess.lifecyclePermissionLocked()
	sess.stateMu.Unlock()
	runningTurn := permission.Submit == lifecycle.QueuePrompt
	if permission.Submit != lifecycle.SendNow && !runningTurn || sess.corePromptInFlight() {
		return
	}
	queued, ok := sess.queuedTurns.first(sess.provider, sess.currentThreadID(), sess.hostedThreadID)
	if !ok {
		return
	}
	if _, _, _, err := sess.claimAndStartTransactionalTurnLocked("", true, runningTurn); err != nil {
		log.Printf("drain transactional turn %s: %v", queued.ClientActionID, err)
	}
}

func (sess *session) cancelQueuedTurn(w http.ResponseWriter, actionID string) {
	if actionID == "" || len(actionID) > 200 {
		httpapi.WriteHTTPError(w, "clientActionId is required", http.StatusBadRequest)
		return
	}
	// The person's Discard is done when nothing is left to end: it ended
	// before, or a send whose acknowledgement was lost never reached Nano.
	queued, err := sess.cancelQueuedTurnAction(actionID, discardedByOperator)
	var refused *promptActionRefused
	switch {
	case errors.As(err, &refused):
		writePromptActionRefusal(w, refused)
		return
	case err != nil && !errors.Is(err, errPromptEnded) && !errors.Is(err, errQueuedTurnNotFound):
		status := http.StatusInternalServerError
		if errors.Is(err, errQueuedTurnDelivering) {
			status = http.StatusConflict
		}
		httpapi.WriteHTTPError(w, err.Error(), status)
		return
	}
	httpapi.WriteJSON(w, map[string]any{"canceled": true, "clientActionId": actionID, "state": queued.State})
}

func (sess *session) cancelQueuedTurnAction(actionID, by string) (queuedTurnRecord, error) {
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	queued, err := cancelQueuedTurnStores(sess.queuedTurns, sess.agentExecutions, actionID, by)
	if errors.Is(err, errQueuedTurnNotFound) {
		// An attempt may still be on its way; it must not land after this.
		if sess.discardedUnknown == nil {
			sess.discardedUnknown = make(map[string]bool)
		}
		sess.discardedUnknown[actionID] = true
	}
	if err != nil {
		return queued, err
	}
	// Live text, which shows the prompt until a saved row settles it, drops
	// it when told.
	if record, _, err := sess.queuedTurns.turns.Get(context.Background(), actionID); err == nil {
		sess.liveText.outboxChanged(record.Outbox)
	}
	return queued, nil
}

// Who ends a prompt for good: its operation's caller, or a person.
const (
	discardedByCaller   = "client"
	discardedByOperator = "operator"
)

// cancelQueuedTurnStores ends a prompt for good, keeping its text and journal
// in the store, if promptActionRefusal allows its Discard: a queued prompt is
// canceled, an uncertain or parked one expired. The operation that sent it
// ends in the same transaction (endDiscardedOperation).
func cancelQueuedTurnStores(queue *queuedTurnStore, ops sessionExecutions, actionID, by string) (queuedTurnRecord, error) {
	if queue == nil || queue.turns == nil {
		return queuedTurnRecord{}, fmt.Errorf("transactional turns store is unavailable")
	}
	ctx := context.Background()
	record, attempt, err := queue.turns.Get(ctx, actionID)
	if errors.Is(err, sql.ErrNoRows) {
		return queuedTurnRecord{}, errQueuedTurnNotFound
	}
	if err != nil {
		return queuedTurnRecord{}, err
	}
	if record.Outbox.State == prompts.Expired || record.Outbox.State == prompts.Refused {
		return queuedTurnRecordFromTransactional(record), errPromptEnded
	}
	if err := refusePromptAction(string(record.Provider), record, promptDiscard); err != nil {
		return queuedTurnRecordFromTransactional(record), err
	}
	if record.Outbox.State == prompts.Queued && attempt.State != prompts.JournalQueued {
		return queuedTurnRecord{}, errQueuedTurnDelivering
	}
	var ended prompts.Record
	end := func(tx *sql.Tx) (err error) {
		if record.Outbox.State == prompts.Queued {
			ended, err = prompts.CancelIn(ctx, tx, actionID)
			return err
		}
		var expired bool
		if ended, expired, err = prompts.ExpireIn(ctx, tx, actionID, time.Now()); err == nil && !expired {
			err = errQueuedTurnDelivering
		}
		return err
	}
	if err := endDiscardedOperation(queue, ops, record, by, end); err != nil {
		return queuedTurnRecord{}, err
	}
	return queuedTurnRecordFromTransactional(ended), nil
}

// endDiscardedOperation makes end, the prompt's own ending, and ends the
// operation that sent the prompt in the same transaction, so its sender sees
// that no answer will come: both commit, or neither does, and a Discard that
// fails can be asked again. prior is the prompt as it was: one still queued
// never reached its provider. An operation already ended keeps its outcome,
// and one waiting on its saved answer still takes it (markTurnDelivery); a
// prompt no operation sent, such as a person's, has none.
func endDiscardedOperation(queue *queuedTurnStore, ops sessionExecutions, prior prompts.Record, by string, end func(*sql.Tx) error) error {
	op, found, err := ops.persistedTurnForTransportAction(context.Background(), prior.Outbox.HostedThreadID, prior.Outbox.ActionID)
	if err != nil {
		return err
	}
	if !found {
		tx, err := queue.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := end(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	who := "the caller"
	if by == discardedByOperator {
		who = "a person"
	}
	ended := &agentExecutionError{Source: by, Code: "queued_turn_canceled", Message: who + " canceled the prompt before provider delivery"}
	if prior.Outbox.State != prompts.Queued {
		ended.Code = "prompt_discarded"
		ended.Message = who + " discarded the prompt; its provider is not known to have taken it, and Nano will not wait for an answer"
	}
	return ops.markTurnDeliveryWith(op.ID, "", agentTurnInterrupted, ended, end)
}

// handleTurnDeliveryResend is the person's Resend of a prompt Nano holds: if
// promptActionRefusal allows it, the prompt is retired and its exact text sent
// once as a new prompt. A repeated Resend (a second click, another tab)
// answers with the prompt the first one made.
func (sess *session) handleTurnDeliveryResend(w http.ResponseWriter, _ *http.Request, actionID string) {
	if actionID == "" || len(actionID) > 200 {
		httpapi.WriteHTTPError(w, "clientActionId is required", http.StatusBadRequest)
		return
	}
	if sess.turns == nil {
		httpapi.WriteHTTPError(w, "the prompt store is unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx := context.Background()
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	record, _, err := sess.turns.Get(ctx, actionID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && record.Outbox.HostedThreadID != sess.hostedThreadID {
		httpapi.WriteError(w, http.StatusNotFound, "not_found", "Nano does not have this prompt")
		return
	}
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if record.Outbox.State == prompts.Expired {
		if resent, attempt, err := sess.turns.Get(ctx, prompts.ResendActionID(actionID)); err == nil {
			httpapi.WriteJSON(w, sess.promptView(resent.Outbox, attempt))
			return
		}
	}
	if err := refusePromptAction(sess.provider, record, promptResend); err != nil {
		writePromptActionRefusal(w, err.(*promptActionRefused))
		return
	}
	permission, reason := sess.submissionPermissionLocked()
	if reason != "" {
		httpapi.WriteError(w, http.StatusConflict, "prompt_action_refused", "Nano can't send to this thread now ("+reason+"), so the prompt was not resent.")
		return
	}
	retired, resent, ok, err := sess.turns.Resend(ctx, actionID, record.Revision, false)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		// It changed since it was read (saved, taken or ended meanwhile).
		refusal := &promptActionRefused{actionID: actionID, state: retired.Outbox.State, reason: "This prompt changed just now, so it was not resent."}
		if current, _, err := sess.turns.Get(ctx, actionID); err == nil {
			refusal.state = current.Outbox.State
			if reason := promptActionRefusal(sess.provider, current.Outbox, promptResend); reason != "" {
				refusal.reason = reason
			}
		}
		writePromptActionRefusal(w, refusal)
		return
	}
	if resent.Origin == prompts.HumanOrigin {
		sess.noteHumanPrompt(time.Now().UTC())
	}
	sess.publishResent(retired, resent)
	if _, _, _, err := sess.startAdmittedTurnLocked(resent.Outbox.ActionID, permission); err != nil {
		log.Printf("start resent prompt %s: %v", resent.Outbox.ActionID, err)
	}
	_, attempt, _ := sess.turns.Get(ctx, resent.Outbox.ActionID)
	httpapi.WriteJSON(w, sess.promptView(resent.Outbox, attempt))
}

// codexTurnParams builds a turn/start request without overriding the selected
// thread profile. Codex resolves an explicit turn permissions id against its
// ambient config, but the execution profile exists only in the per-thread
// config supplied and confirmed at start or resume. Omitting the optional turn
// override preserves that confirmed thread-scoped profile.
func codexTurnParams(sess *session, text string) (map[string]any, map[string]string) {
	params := map[string]any{
		"threadId": sess.currentThreadID(),
		"input":    []map[string]any{{"type": "text", "text": text}},
	}
	model, effort, serviceTier := sess.turnOptions()
	for k, v := range codexTurnOverrides(model, effort, serviceTier, sess.reasoningSummary) {
		params[k] = v
	}
	facts := map[string]string{}
	if model != "" {
		facts[postureFieldModel] = model
	}
	if effort != "" {
		facts[postureFieldEffort] = effort
	}
	if serviceTier != "" {
		facts[postureFieldServiceTier] = serviceTier
	}
	return params, facts
}

// turnSendResult is one delivery answer. confirmed carries the journal's
// provider-evidence bit so a reloaded pane learns from the response itself that
// a textless accepted record already reached the transcript.
type turnSendResult struct {
	status      int
	actionID    string
	reason      string
	confirmed   bool
	confirmedAt string
}

func writeTurnSendResponse(w http.ResponseWriter, result turnSendResult) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(result.status)
	operationStatus := "accepted"
	if result.status == http.StatusAccepted {
		operationStatus = "sending"
		if result.reason == "queued_for_next_turn" {
			operationStatus = sendAttemptQueued
		} else if result.reason == "queued_delivery_failed" {
			operationStatus = "parked"
		} else if result.reason == "unknown_after_restart" {
			operationStatus = "unknown"
		} else if result.reason == "delivery_timeout" {
			operationStatus = "timeout"
		}
	}
	body := map[string]any{
		"accepted":       result.status == http.StatusOK,
		"pending":        result.status == http.StatusAccepted,
		"status":         operationStatus,
		"clientActionId": result.actionID,
		"reason":         result.reason,
		"confirmed":      result.confirmed,
	}
	if result.confirmedAt != "" {
		body["confirmedAt"] = result.confirmedAt
	}
	json.NewEncoder(w).Encode(body)
}

func writeTurnDeliveryFailure(w http.ResponseWriter, status int, actionID, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	operationStatus := sendAttemptRefused
	if status == http.StatusBadGateway {
		operationStatus = sendAttemptFailed
	}
	body := map[string]any{
		"accepted": false, "pending": false, "status": operationStatus,
		"source": "delivery", "clientActionId": actionID, "reason": reason,
	}
	if reason == sendAttemptInterruptedBeforeTranscript {
		body["reasonCode"] = sendAttemptInterruptedBeforeTranscript
	}
	_ = json.NewEncoder(w).Encode(body)
}

// deliveryExpiries counts, per provider, the turn deliveries whose deadline
// expired within the window: while a provider's count is above zero, its
// panes say it is slow. It is process memory only.
type deliveryExpiries struct {
	window time.Duration
	// changed republishes the runtime status of the provider's sessions; it
	// runs once when an expiry is recorded and once when that expiry ages out.
	changed func(provider string)
	mu      sync.Mutex
	at      map[string][]time.Time
}

func newDeliveryExpiries(window time.Duration, changed func(provider string)) *deliveryExpiries {
	return &deliveryExpiries{window: window, changed: changed, at: map[string][]time.Time{}}
}

func (d *deliveryExpiries) record(provider string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.at[provider] = append(d.recentLocked(provider), time.Now())
	d.mu.Unlock()
	go d.changed(provider)
	time.AfterFunc(d.window, func() { d.changed(provider) })
}

func (d *deliveryExpiries) count(provider string) int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.at[provider] = d.recentLocked(provider)
	return len(d.at[provider])
}

func (d *deliveryExpiries) recentLocked(provider string) []time.Time {
	times := d.at[provider]
	cutoff := time.Now().Add(-d.window)
	for len(times) > 0 && !times[0].After(cutoff) {
		times = times[1:]
	}
	return times
}

// publishProviderRuntime republishes the runtime status of every session of
// provider, so each pane sees that provider's delivery expiry count.
func (s *server) publishProviderRuntime(provider string) {
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		if sess.provider == provider {
			sessions = append(sessions, sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.publishProjectionRuntime()
	}
}
