package hosted

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	prompts "github.com/dkroshow/nano/internal/core/prompts"
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
	Attribution TurnAttribution
	CreateGuard agentTurnCreationGuard
}

// TurnAttributionFor snapshots a complete sender identity at admission time.
// A public agent turn cannot proceed on partial identity: the persisted footer
// is immutable, so deferring a missing provider-native id would make a
// permanently untraceable message. A name is presentation, not identity; an
// unnamed but otherwise complete sender gets a deterministic label.
func (s *Controller) TurnAttributionFor(sender agentSessionRecord) (TurnAttribution, error) {
	provider := strings.TrimSpace(sender.Provider)
	native := strings.TrimSpace(sender.NativeThreadID)
	if sender.ID == "" || provider == "" || native == "" || NativeThreadIDProvisional(provider, native) {
		return TurnAttribution{}, errors.New("senderThreadRef resolves to a hosted thread without a confirmed provider-native thread identity")
	}
	identity, err := s.ResolveIdentity(context.Background(), sender.ID)
	if err != nil {
		return TurnAttribution{}, fmt.Errorf("resolve sender identity: %w", err)
	}
	// Names and native identities must come from the same answer as the public
	// resolver. Refuse a changed binding rather than signing it with stale facts.
	if identity.MatchCount != 1 || identity.HostedThreadID != sender.ID || identity.Provider != provider || identity.NativeThreadID != native || identity.NativeThreadIDProvisional {
		return TurnAttribution{}, errors.New("senderThreadRef does not resolve to the same confirmed hosted provider-native identity")
	}
	return TurnAttribution{
		Origin: turnOriginAgent, SenderHostedThreadID: sender.ID, SenderNativeThreadID: native,
		SenderProvider: provider, SenderName: strings.TrimSpace(identity.DisplayName), SenderSubmittedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

func senderPresentationName(a TurnAttribution) string {
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
func senderFooter(a TurnAttribution) string {
	if a.Origin == turnOriginSystem {
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
func persistedSenderFooter(a TurnAttribution) string {
	if footer := senderFooter(a); footer != "" {
		return footer
	}
	if a.Origin != turnOriginAgent || strings.TrimSpace(a.SenderSubmittedAt) != "" {
		return ""
	}
	name, native := strings.TrimSpace(a.SenderName), strings.TrimSpace(a.SenderNativeThreadID)
	if name == "" || native == "" {
		return ""
	}
	return "[from " + name + " · " + native + "]"
}

type hostedTurnAdmissionError struct{ Reason string }

func (e *hostedTurnAdmissionError) Error() string { return "target is not idle: " + e.Reason }

func hostedTurnPromptHash(prompt string, requireIdle bool) string {
	if !requireIdle {
		return AgentHash([]byte(prompt))
	}
	return AgentHash([]byte(prompt + "\x00requireIdle"))
}

func (s *Controller) submitHostedTurnFull(hostedThreadID, actionID string, sub hostedTurnSubmission) (agentTurnRecord, bool, error) {
	prompt, requireIdle := sub.Prompt, sub.RequireIdle
	if sub.Delivered == "" {
		sub.Delivered = prompt
	}
	var err error
	sub.Delivered, err = s.PromptWithUnreadFooter(hostedThreadID, sub.Delivered)
	if err != nil {
		return agentTurnRecord{}, false, fmt.Errorf("%w: %v", s.UnreadFooterUnavailable, err)
	}
	sub.Origin = string(prompts.NormalizeOrigin(prompts.Origin(sub.Origin)))
	sub.Attribution.Origin = sub.Origin
	hash := hostedTurnPromptHash(prompt, requireIdle)
	if sub.NextTurn {
		hash = TurnRequestHash(prompt, true)
	}
	waited := s.Runtime.Waiting(hostedThreadID)
	s.Runtime.Reattach(hostedThreadID)
	_, found := s.Executions.turnForAction(hostedThreadID, actionID)
	if !found && s.Runtime.Admission(hostedThreadID) == nil {
		if requireIdle {
			return agentTurnRecord{}, false, &hostedTurnAdmissionError{Reason: "detached"}
		}
		return agentTurnRecord{}, false, errHostedThreadNotAttached
	}
	if found {
		return s.submitExistingHostedTurn(hostedThreadID, actionID, sub, hash)
	}
	sess := s.Runtime.Admission(hostedThreadID)
	if sess == nil {
		if requireIdle {
			return agentTurnRecord{}, false, &hostedTurnAdmissionError{Reason: "detached"}
		}
		return agentTurnRecord{}, false, errHostedThreadNotAttached
	}
	if requireIdle {
		var result agentTurnRecord
		var replay bool
		reason, err := sess.WithIdleDelivery(waited == "resting", func() error {
			record, created, err := s.Executions.beginTurnAttributedGuarded(hostedThreadID, actionID, hash, sub.Attribution, sub.CreateGuard)
			if err != nil {
				return err
			}
			op, responseStatus, reason, err := sess.StartLockedPrepared(record.TransportActionID, sub.Delivered, sub.Prompt, hash, sub.Origin, false)
			result, replay, err = s.finishHostedTurnSubmission(sess, record, created, op, responseStatus, reason, err)
			return err
		})
		if reason != "" {
			return agentTurnRecord{}, false, &hostedTurnAdmissionError{Reason: reason}
		}
		return result, replay, err
	}

	record, created, err := s.Executions.beginTurnAttributedGuarded(hostedThreadID, actionID, hash, sub.Attribution, sub.CreateGuard)
	if err != nil {
		if errors.Is(err, errAgentTurnIdempotencyConflict) {
			return agentTurnRecord{}, false, errAgentTurnIdempotencyConflict
		}
		return agentTurnRecord{}, false, err
	}
	if !created && s.turnSubmissionDeliveryUnknown(record) {
		_ = s.Executions.MarkTurnDelivery(record.ID, "", agentTurnDeliveryUnknown, &agentExecutionError{
			Source: "delivery", Code: "nano_restarted", Message: "Nano restarted after provider delivery began",
		})
		latest, _ := s.Executions.Turn(record.ID)
		return latest, true, nil
	}
	if !created && !s.needsTurnSubmissionReconciliation(record) {
		return record, true, nil
	}
	sess = s.Runtime.Admission(hostedThreadID)
	if sess == nil {
		return record, !created, errHostedThreadNotAttached
	}
	op, responseStatus, reason, err := sess.StartPrepared(record.TransportActionID, sub.Delivered, sub.Prompt, hash, sub.Origin, sub.NextTurn)
	return s.finishHostedTurnSubmission(sess, record, created, op, responseStatus, reason, err)
}

func (s *Controller) submitExistingHostedTurn(hostedThreadID, actionID string, sub hostedTurnSubmission, hash string) (agentTurnRecord, bool, error) {
	record, created, err := s.Executions.beginTurnAttributed(hostedThreadID, actionID, hash, sub.Attribution)
	if err == nil {
		// A retry of the same clientActionId must replay the text Nano already
		// persisted: the footer is never rewritten, and a thread rename between
		// retries must not trip the queue's text-hash guard.
		if record.Origin == turnOriginAgent {
			footer := persistedSenderFooter(TurnAttribution{
				Origin: record.Origin, SenderHostedThreadID: record.SenderHostedThreadID,
				SenderNativeThreadID: record.SenderNativeThreadID, SenderProvider: record.SenderProvider,
				SenderName: record.SenderName, SenderSubmittedAt: record.SenderSubmittedAt,
			})
			if footer != "" {
				sub.Delivered = sub.Prompt + "\n\n" + footer
				var footerErr error
				sub.Delivered, footerErr = s.PromptWithUnreadFooter(hostedThreadID, sub.Delivered)
				if footerErr != nil {
					return agentTurnRecord{}, false, fmt.Errorf("%w: %v", s.UnreadFooterUnavailable, footerErr)
				}
			}
		}
		if sess := s.Runtime.Admission(hostedThreadID); sess != nil {
			if text, ok := sess.QueuedText(record.TransportActionID); ok && text != "" {
				sub.Delivered = text
			}
		}
	}
	if err != nil {
		return agentTurnRecord{}, false, err
	}
	if !created && s.turnSubmissionDeliveryUnknown(record) {
		_ = s.Executions.MarkTurnDelivery(record.ID, "", agentTurnDeliveryUnknown, &agentExecutionError{Source: "delivery", Code: "nano_restarted", Message: "Nano restarted after provider delivery began"})
		latest, _ := s.Executions.Turn(record.ID)
		return latest, true, nil
	}
	if !created && !s.needsTurnSubmissionReconciliation(record) {
		return record, true, nil
	}
	sess := s.Runtime.Admission(hostedThreadID)
	if sess == nil {
		return record, true, errHostedThreadNotAttached
	}
	op, responseStatus, reason, err := sess.StartPrepared(record.TransportActionID, sub.Delivered, sub.Prompt, hash, sub.Origin, sub.NextTurn)
	return s.finishHostedTurnSubmission(sess, record, created, op, responseStatus, reason, err)
}

func (s *Controller) finishHostedTurnSubmission(sess AdmissionSession, record agentTurnRecord, created bool, started bool, responseStatus int, reason string, err error) (agentTurnRecord, bool, error) {
	if err != nil {
		_ = s.Executions.MarkTurnDelivery(record.ID, "", agentTurnFailed, &agentExecutionError{Source: "delivery", Message: err.Error()})
		sess.NotifyRuntime()
		return record, false, err
	}
	if responseStatus == http.StatusAccepted && reason == "queued_for_next_turn" {
		_ = s.Executions.MarkTurnDelivery(record.ID, "", agentTurnQueued, nil)
	} else if responseStatus != 0 || !started {
		state := agentTurnDeliveryUnknown
		if responseStatus >= http.StatusBadRequest {
			state = agentTurnFailed
		}
		code := "existing_delivery_state"
		if reason == "reconnect_failed" {
			code = reason
		}
		_ = s.Executions.MarkTurnDelivery(record.ID, "", state, &agentExecutionError{Source: "delivery", Code: code, Message: reason})
		if state == agentTurnFailed {
			sess.NotifyRuntime()
		}
	}
	latest, _ := s.Executions.Turn(record.ID)
	return latest, !created, nil
}

var errHostedThreadNotAttached = errors.New("hosted thread is not attached")

func (s *Controller) needsTurnSubmissionReconciliation(record agentTurnRecord) bool {
	if record.Status != agentTurnDeliveryPending {
		return false
	}
	return !s.Runtime.HasDeliveryEvidence(record.HostedThreadID, record.TransportActionID)
}

func (s *Controller) turnSubmissionDeliveryUnknown(record agentTurnRecord) bool {
	if record.Status != agentTurnDeliveryPending || record.RunID == s.Executions.runID {
		return false
	}
	return s.Runtime.DeliveryWasAttempted(record.HostedThreadID, record.TransportActionID)
}
