package hosted

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	hostedprompts "github.com/dkroshow/nano/internal/core/prompts"
	prompts "github.com/dkroshow/nano/internal/core/prompts"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// hostedTurnSubmission carries one agent-API prompt through admission.
// Prompt is the caller's text and is what the idempotency hash covers;
// Delivered is what the provider receives (Prompt plus Nano's sender footer).
// Origin and Attribution are persisted on the operation.
type hostedTurnSubmission struct {
	Prompt      string
	Delivered   string
	RequireIdle bool
	// NextTurn holds the prompt until the target's running turn ends.
	NextTurn    bool
	Origin      string
	Attribution hostedstore.TurnAttribution
	CreateGuard hostedstore.AgentTurnCreationGuard
}

// TurnAttributionFor snapshots a complete sender identity at admission time.
// A public agent turn cannot proceed on partial identity: the persisted footer
// is immutable, so deferring a missing provider-native id would make a
// permanently untraceable message. A name is presentation, not identity; an
// unnamed but otherwise complete sender gets a deterministic label.
func (s *Controller) TurnAttributionFor(sender ownedthreads.SessionRecord) (hostedstore.TurnAttribution, error) {
	provider := strings.TrimSpace(sender.Provider)
	native := strings.TrimSpace(sender.NativeThreadID)
	if sender.ID == "" || provider == "" || native == "" || hostedstore.NativeThreadIDProvisional(provider, native) {
		return hostedstore.TurnAttribution{}, errors.New("senderThreadRef resolves to a hosted thread without a confirmed provider-native thread identity")
	}
	identity, err := s.ResolveIdentity(context.Background(), sender.ID)
	if err != nil {
		return hostedstore.TurnAttribution{}, fmt.Errorf("resolve sender identity: %w", err)
	}
	// Names and native identities must come from the same answer as the public
	// resolver. Refuse a changed binding rather than signing it with stale facts.
	if identity.MatchCount != 1 || identity.HostedThreadID != sender.ID || identity.Provider != provider || identity.NativeThreadID != native || identity.NativeThreadIDProvisional {
		return hostedstore.TurnAttribution{}, errors.New("senderThreadRef does not resolve to the same confirmed hosted provider-native identity")
	}
	return hostedstore.TurnAttribution{
		Origin: string(hostedprompts.AgentOrigin), SenderHostedThreadID: sender.ID, SenderNativeThreadID: native,
		SenderProvider: provider, SenderName: strings.TrimSpace(identity.DisplayName), SenderSubmittedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

func senderPresentationName(a hostedstore.TurnAttribution) string {
	if name := strings.TrimSpace(a.SenderName); name != "" {
		return name
	}
	provider, native := strings.TrimSpace(a.SenderProvider), strings.TrimSpace(a.SenderNativeThreadID)
	if provider == "" || native == "" {
		return ""
	}
	prefix := native
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return provider + " " + prefix + " (unnamed)"
}

// senderFooter is the one trailing line Nano appends to every agent-origin
// prompt so it never reads as a human message. Agent footers identify the
// sender and Nano's original UTC admission time, before any queue delay.
// Agent submissions reach this function only after complete attribution
// admission. System prompts retain their distinct Nano footer.
func senderFooter(a hostedstore.TurnAttribution) string {
	if a.Origin == string(hostedprompts.SystemOrigin) {
		return "[from nano]"
	}
	name := senderPresentationName(a)
	native := strings.TrimSpace(a.SenderNativeThreadID)
	submittedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(a.SenderSubmittedAt))
	if name == "" || native == "" || err != nil {
		return ""
	}
	return "[from " + name + " · " + native + " · submitted " + submittedAt.UTC().Format(time.RFC3339) + "]"
}

// persistedSenderFooter replays an operation's original attribution snapshot.
// Older operations have no recorded admission timestamp; retain their
// historical footer form rather than fabricating a new time during recovery.
// New public admissions always use senderFooter above and therefore always
// carry a submission timestamp.
func persistedSenderFooter(a hostedstore.TurnAttribution) string {
	if footer := senderFooter(a); footer != "" {
		return footer
	}
	if a.Origin != string(hostedprompts.AgentOrigin) || strings.TrimSpace(a.SenderSubmittedAt) != "" {
		return ""
	}
	name, native := strings.TrimSpace(a.SenderName), strings.TrimSpace(a.SenderNativeThreadID)
	if name == "" || native == "" {
		return ""
	}
	return "[from " + name + " · " + native + "]"
}

type hostedTurnAdmissionError struct{ Reason string }

// Error returns the admission failure message.
func (e *hostedTurnAdmissionError) Error() string { return "target is not idle: " + e.Reason }

func hostedTurnPromptHash(prompt string, requireIdle bool) string {
	if !requireIdle {
		return hostedstore.AgentHash([]byte(prompt))
	}
	return hostedstore.AgentHash([]byte(prompt + "\x00requireIdle"))
}

func (s *Controller) submitHostedTurnFull(hostedThreadID, actionID string, sub hostedTurnSubmission) (ownedthreads.TurnRecord, bool, error) {
	prompt, requireIdle := sub.Prompt, sub.RequireIdle
	if sub.Delivered == "" {
		sub.Delivered = prompt
	}
	var err error
	sub.Delivered, err = s.PromptWithUnreadFooter(hostedThreadID, sub.Delivered)
	if err != nil {
		return ownedthreads.TurnRecord{}, false, fmt.Errorf("%w: %v", s.UnreadFooterUnavailable, err)
	}
	sub.Origin = string(prompts.NormalizeOrigin(prompts.Origin(sub.Origin)))
	sub.Attribution.Origin = sub.Origin
	hash := hostedTurnPromptHash(prompt, requireIdle)
	if sub.NextTurn {
		hash = hostedstore.TurnRequestHash(prompt, true)
	}
	_, waited := s.Boot.WaitingHosted(hostedThreadID)
	s.AttachmentsControl.reattachWaitingHosted(hostedThreadID)
	_, found := s.Executions.TurnForAction(hostedThreadID, actionID)
	if !found && s.sessionForHosted(hostedThreadID) == nil {
		if requireIdle {
			return ownedthreads.TurnRecord{}, false, &hostedTurnAdmissionError{Reason: "detached"}
		}
		return ownedthreads.TurnRecord{}, false, errHostedThreadNotAttached
	}
	if found {
		return s.submitExistingHostedTurn(hostedThreadID, actionID, sub, hash)
	}
	sess := s.sessionForHosted(hostedThreadID)
	if sess == nil {
		if requireIdle {
			return ownedthreads.TurnRecord{}, false, &hostedTurnAdmissionError{Reason: "detached"}
		}
		return ownedthreads.TurnRecord{}, false, errHostedThreadNotAttached
	}
	if requireIdle {
		var result ownedthreads.TurnRecord
		var replay bool
		reason, err := sess.WithIdleDelivery(waited == "resting", func() error {
			record, created, err := s.Executions.BeginTurnAttributedGuarded(hostedThreadID, actionID, hash, sub.Attribution, sub.CreateGuard)
			if err != nil {
				return err
			}
			op, responseStatus, reason, err := sess.QueueOrStartTurnSendLockedPreparedPresentation(record.TransportActionID, sub.Delivered, sub.Prompt, hash, sub.Origin, false)
			result, replay, err = s.finishHostedTurnSubmission(sess, record, created, op != nil, responseStatus, reason, err)
			return err
		})
		if reason != "" {
			return ownedthreads.TurnRecord{}, false, &hostedTurnAdmissionError{Reason: reason}
		}
		return result, replay, err
	}

	record, created, err := s.Executions.BeginTurnAttributedGuarded(hostedThreadID, actionID, hash, sub.Attribution, sub.CreateGuard)
	if err != nil {
		if errors.Is(err, hostedstore.ErrAgentTurnIdempotencyConflict) {
			return ownedthreads.TurnRecord{}, false, hostedstore.ErrAgentTurnIdempotencyConflict
		}
		return ownedthreads.TurnRecord{}, false, err
	}
	if !created && s.turnSubmissionDeliveryUnknown(record) {
		_ = s.Executions.MarkTurnDelivery(record.ID, "", ownedthreads.AgentTurnDeliveryUnknown, &ownedthreads.ExecutionError{
			Source: "delivery", Code: "nano_restarted", Message: "Nano restarted after provider delivery began",
		})
		latest, _ := s.Executions.Turn(record.ID)
		return latest, true, nil
	}
	if !created && !s.needsTurnSubmissionReconciliation(record) {
		return record, true, nil
	}
	sess = s.sessionForHosted(hostedThreadID)
	if sess == nil {
		return record, !created, errHostedThreadNotAttached
	}
	op, responseStatus, reason, err := sess.QueueOrStartTurnSendPreparedPresentation(record.TransportActionID, sub.Delivered, sub.Prompt, hash, sub.Origin, sub.NextTurn)
	return s.finishHostedTurnSubmission(sess, record, created, op != nil, responseStatus, reason, err)
}

func (s *Controller) submitExistingHostedTurn(hostedThreadID, actionID string, sub hostedTurnSubmission, hash string) (ownedthreads.TurnRecord, bool, error) {
	record, created, err := s.Executions.BeginTurnAttributed(hostedThreadID, actionID, hash, sub.Attribution)
	if err == nil {
		// A retry of the same clientActionId must replay the text Nano already
		// persisted: the footer is never rewritten, and a thread rename between
		// retries must not trip the queue's text-hash guard.
		if record.Origin == string(hostedprompts.AgentOrigin) {
			footer := persistedSenderFooter(hostedstore.TurnAttribution{
				Origin: record.Origin, SenderHostedThreadID: record.SenderHostedThreadID,
				SenderNativeThreadID: record.SenderNativeThreadID, SenderProvider: record.SenderProvider,
				SenderName: record.SenderName, SenderSubmittedAt: record.SenderSubmittedAt,
			})
			if footer != "" {
				sub.Delivered = sub.Prompt + "\n\n" + footer
				var footerErr error
				sub.Delivered, footerErr = s.PromptWithUnreadFooter(hostedThreadID, sub.Delivered)
				if footerErr != nil {
					return ownedthreads.TurnRecord{}, false, fmt.Errorf("%w: %v", s.UnreadFooterUnavailable, footerErr)
				}
			}
		}
		if sess := s.sessionForHosted(hostedThreadID); sess != nil {
			if row, ok := sess.QueuedPrompt(record.TransportActionID); ok && row.Text != "" {
				sub.Delivered = row.Text
			}
		}
	}
	if err != nil {
		return ownedthreads.TurnRecord{}, false, err
	}
	if !created && s.turnSubmissionDeliveryUnknown(record) {
		_ = s.Executions.MarkTurnDelivery(record.ID, "", ownedthreads.AgentTurnDeliveryUnknown, &ownedthreads.ExecutionError{Source: "delivery", Code: "nano_restarted", Message: "Nano restarted after provider delivery began"})
		latest, _ := s.Executions.Turn(record.ID)
		return latest, true, nil
	}
	if !created && !s.needsTurnSubmissionReconciliation(record) {
		return record, true, nil
	}
	sess := s.sessionForHosted(hostedThreadID)
	if sess == nil {
		return record, true, errHostedThreadNotAttached
	}
	op, responseStatus, reason, err := sess.QueueOrStartTurnSendPreparedPresentation(record.TransportActionID, sub.Delivered, sub.Prompt, hash, sub.Origin, sub.NextTurn)
	return s.finishHostedTurnSubmission(sess, record, created, op != nil, responseStatus, reason, err)
}

func (s *Controller) finishHostedTurnSubmission(sess *sessions.Session, record ownedthreads.TurnRecord, created bool, started bool, responseStatus int, reason string, err error) (ownedthreads.TurnRecord, bool, error) {
	if err != nil {
		_ = s.Executions.MarkTurnDelivery(record.ID, "", ownedthreads.AgentTurnFailed, &ownedthreads.ExecutionError{Source: "delivery", Message: err.Error()})
		sess.NotifyRuntime()
		return record, false, err
	}
	if responseStatus == http.StatusAccepted && reason == "queued_for_next_turn" {
		_ = s.Executions.MarkTurnDelivery(record.ID, "", ownedthreads.AgentTurnQueued, nil)
	} else if responseStatus != 0 || !started {
		state := ownedthreads.AgentTurnDeliveryUnknown
		if responseStatus >= http.StatusBadRequest {
			state = ownedthreads.AgentTurnFailed
		}
		code := "existing_delivery_state"
		if reason == "reconnect_failed" {
			code = reason
		}
		_ = s.Executions.MarkTurnDelivery(record.ID, "", state, &ownedthreads.ExecutionError{Source: "delivery", Code: code, Message: reason})
		if state == ownedthreads.AgentTurnFailed {
			sess.NotifyRuntime()
		}
	}
	latest, _ := s.Executions.Turn(record.ID)
	return latest, !created, nil
}

var errHostedThreadNotAttached = errors.New("hosted thread is not attached")

func (s *Controller) needsTurnSubmissionReconciliation(record ownedthreads.TurnRecord) bool {
	if record.Status != ownedthreads.AgentTurnDeliveryPending {
		return false
	}
	return !s.sessionForHosted(record.HostedThreadID).HasDeliveryEvidence(record.TransportActionID, s.Queue, s.SendAttempts)
}

func (s *Controller) turnSubmissionDeliveryUnknown(record ownedthreads.TurnRecord) bool {
	if record.Status != ownedthreads.AgentTurnDeliveryPending || record.RunID == s.Executions.RunID() {
		return false
	}
	return s.sessionForHosted(record.HostedThreadID).DeliveryWasAttempted(record.TransportActionID, s.SendAttempts)
}
