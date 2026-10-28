package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	sessionruntime "github.com/dkroshow/nano/internal/sessions"
)

var agentMessageRefKindPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)+$`)

type agentMessageSendRequest struct {
	ClientMessageID string `json:"clientMessageId"`
	SenderThreadRef string `json:"senderThreadRef,omitempty"`
	// Operator marks mail written by the human operator in the Nano UI.
	// Mutually exclusive with senderThreadRef: exactly one identity per send.
	Operator            bool              `json:"operator,omitempty"`
	Subject             string            `json:"subject,omitempty"`
	Body                string            `json:"body"`
	Delivery            string            `json:"delivery,omitempty"`
	Refs                []agentMessageRef `json:"refs,omitempty"`
	DispositionFeedback bool              `json:"dispositionFeedback,omitempty"`
}

func (s *server) agentInboxAvailable(w http.ResponseWriter) bool {
	if s.mail != nil && s.mailErr == "" {
		return true
	}
	detail := "durable agent inbox unavailable"
	if s.mailErr != "" {
		detail += ": " + s.mailErr
	}
	httpapi.WriteError(w, http.StatusServiceUnavailable, "inbox_unavailable", detail)
	return false
}

func validAgentMessageRef(ref agentMessageRef) bool {
	return len(ref.Kind) <= 64 && len(ref.ID) > 0 && len(ref.ID) <= 200 && agentMessageRefKindPattern.MatchString(ref.Kind)
}

func (s *server) handleHostedThreadMessageSend(w http.ResponseWriter, r *http.Request, recipient agentSessionRecord) {
	if !s.agentInboxAvailable(w) {
		return
	}
	var req agentMessageSendRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_message_request", "invalid agent message request")
		return
	}
	req.ClientMessageID = strings.TrimSpace(req.ClientMessageID)
	if !validAgentClientID(req.ClientMessageID) || strings.HasPrefix(req.ClientMessageID, "nano:") {
		httpapi.WriteError(w, http.StatusBadRequest, "client_message_id_invalid", "clientMessageId must be 1-200 URL-safe characters and must not use the reserved nano: prefix")
		return
	}
	if req.Operator == (strings.TrimSpace(req.SenderThreadRef) != "") {
		httpapi.WriteError(w, http.StatusBadRequest, "sender_required", "exactly one of senderThreadRef or operator:true is required")
		return
	}
	if req.Delivery != "" && req.Delivery != "inbox" {
		httpapi.WriteError(w, http.StatusBadRequest, "delivery_mode_unavailable", "V1 supports inbox delivery only")
		return
	}
	if len(req.Subject) > maxAgentMessageSubjectBytes {
		httpapi.WriteError(w, http.StatusBadRequest, "subject_too_long", "subject must be at most 120 UTF-8 bytes")
		return
	}
	if len(req.Body) == 0 || len(req.Body) > maxAgentMessageBodyBytes || strings.TrimSpace(req.Body) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "body_invalid", "body must be 1-16384 UTF-8 bytes and contain non-whitespace text")
		return
	}
	if len(req.Refs) > maxAgentMessageRefs {
		httpapi.WriteError(w, http.StatusBadRequest, "refs_invalid", "at most 16 refs are allowed")
		return
	}
	for i := range req.Refs {
		req.Refs[i].Kind, req.Refs[i].ID = strings.TrimSpace(req.Refs[i].Kind), strings.TrimSpace(req.Refs[i].ID)
		if !validAgentMessageRef(req.Refs[i]) {
			httpapi.WriteError(w, http.StatusBadRequest, "refs_invalid", "ref kinds must be dotted client namespaces and ref ids must be 1-200 characters")
			return
		}
	}
	refs := normalizedAgentMessageRefs(req.Refs)
	var create agentMessageCreate
	if req.Operator {
		create = agentMessageCreate{ClientMessageID: req.ClientMessageID, SenderKey: operatorSenderKey,
			SenderKind: operatorSenderKind, Origin: operatorSenderKind, SenderName: operatorSenderName,
			RecipientHostedID: recipient.ID, Subject: req.Subject, Body: req.Body,
			Refs: refs, DispositionFeedback: req.DispositionFeedback, CreatedAt: time.Now().UTC()}
	} else {
		sender, ok := s.resolveHostedThreadRecord(strings.TrimSpace(req.SenderThreadRef))
		if !ok {
			httpapi.WriteError(w, http.StatusBadRequest, "sender_not_hosted", "senderThreadRef does not resolve to a hosted thread")
			return
		}
		attribution, err := s.turnAttributionFor(sender)
		if err != nil {
			httpapi.WriteError(w, http.StatusBadRequest, "sender_identity_incomplete", err.Error())
			return
		}
		create = agentMessageCreate{ClientMessageID: req.ClientMessageID, SenderKey: sender.ID, SenderKind: "thread", Origin: "agent",
			SenderHostedID: sender.ID, SenderNativeID: attribution.SenderNativeThreadID, SenderProvider: attribution.SenderProvider,
			SenderName: attribution.SenderName, RecipientHostedID: recipient.ID, Subject: req.Subject, Body: req.Body,
			Refs: refs, DispositionFeedback: req.DispositionFeedback, CreatedAt: time.Now().UTC()}
	}
	create.RequestHash = agentMessageRequestHashWithFeedback(recipient.ID, create.Subject, create.Body, refs, create.DispositionFeedback, "")
	message, created, err := s.mail.CreateMessage(create)
	if err != nil {
		var typed *agentMessageStoreError
		if errors.As(err, &typed) {
			httpapi.WriteError(w, http.StatusConflict, typed.Code, typed.Detail)
			return
		}
		httpapi.WriteError(w, http.StatusServiceUnavailable, "message_persistence_failed", err.Error())
		return
	}
	if created {
		s.notifyMailChanged(recipient.ID)
	}
	w.Header().Set("Content-Type", "application/json")
	if created {
		w.WriteHeader(http.StatusCreated)
	}
	_ = json.NewEncoder(w).Encode(message)
}

func parseAgentMessageListOptions(r *http.Request) (agentMessageListOptions, error) {
	query := r.URL.Query()
	opts := agentMessageListOptions{Limit: 50}
	if raw := strings.TrimSpace(query.Get("since")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			return opts, errors.New("since must be a non-negative integer")
		}
		opts.Since = value
	}
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return opts, errors.New("limit must be between 1 and 100")
		}
		opts.Limit = value
	}
	if raw := strings.TrimSpace(query.Get("status")); raw != "" {
		seen := map[string]bool{}
		for _, status := range strings.Split(raw, ",") {
			status = strings.TrimSpace(status)
			switch status {
			case agentMessageUnread, agentMessageSeen, agentMessageActed, agentMessageDeclined, agentMessageDeferred:
			default:
				return opts, errors.New("status filter is invalid")
			}
			if !seen[status] {
				seen[status] = true
				opts.Status = append(opts.Status, status)
			}
		}
	}
	if raw := strings.TrimSpace(query.Get("ref")); raw != "" {
		kind, id, ok := strings.Cut(raw, ":")
		ref := agentMessageRef{Kind: strings.TrimSpace(kind), ID: strings.TrimSpace(id)}
		if !ok || !validAgentMessageRef(ref) {
			return opts, errors.New("ref must be kind:id with a dotted client namespace")
		}
		opts.Ref = &ref
	}
	return opts, nil
}

func (s *server) handleHostedThreadInbox(w http.ResponseWriter, r *http.Request, recipient agentSessionRecord) {
	if !s.agentInboxAvailable(w) {
		return
	}
	actorRef := strings.TrimSpace(r.URL.Query().Get("actorThreadRef"))
	actor, ok := s.resolveHostedThreadRecord(actorRef)
	if actorRef == "" || !ok {
		httpapi.WriteError(w, http.StatusBadRequest, "actor_not_hosted", "actorThreadRef must resolve to the inbox owner")
		return
	}
	if actor.ID != recipient.ID {
		httpapi.WriteError(w, http.StatusForbidden, "not_recipient", "a hosted thread may read only its own inbox")
		return
	}
	opts, err := parseAgentMessageListOptions(r)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_inbox_filter", err.Error())
		return
	}
	peek := false
	if raw := strings.TrimSpace(r.URL.Query().Get("peek")); raw != "" {
		peek, err = strconv.ParseBool(raw)
		if err != nil {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_inbox_filter", "peek must be true or false")
			return
		}
	}
	items, marked, err := s.mail.ListInbox(recipient.ID, opts, !peek, time.Now().UTC())
	if err != nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "inbox_read_failed", err.Error())
		return
	}
	if marked > 0 {
		s.notifyMailChanged(recipient.ID)
	}
	next := opts.Since
	if len(items) > 0 {
		next = items[len(items)-1].Seq
	}
	httpapi.WriteJSON(w, map[string]any{"items": items, "nextSince": next})
}

func (s *server) handleHostedThreadOutbox(w http.ResponseWriter, r *http.Request, sender agentSessionRecord) {
	if !s.agentInboxAvailable(w) {
		return
	}
	opts, err := parseAgentMessageListOptions(r)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_outbox_filter", err.Error())
		return
	}
	items, err := s.mail.ListOutbox(sender.ID, opts)
	if err != nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "outbox_read_failed", err.Error())
		return
	}
	next := opts.Since
	if len(items) > 0 {
		next = items[len(items)-1].MessageOrder
	}
	httpapi.WriteJSON(w, map[string]any{"items": items, "nextSince": next})
}

func (s *server) agentMessageID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.agentInboxAvailable(w) {
		return "", false
	}
	messageID := strings.TrimSpace(r.PathValue("messageId"))
	if messageID == "" {
		httpapi.WriteNotFound(w)
		return "", false
	}
	return messageID, true
}

func (s *server) handleAgentMessageRead(w http.ResponseWriter, r *http.Request) {
	messageID, ok := s.agentMessageID(w, r)
	if !ok {
		return
	}
	detail, err := s.mail.MessageDetail(messageID)
	if errors.Is(err, errAgentMessageNotFound) {
		httpapi.WriteError(w, http.StatusNotFound, "message_not_found", "message not found")
		return
	}
	if err != nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "message_read_failed", err.Error())
		return
	}
	httpapi.WriteJSON(w, detail)
}

func (s *server) handleAgentMessageEditRoute(w http.ResponseWriter, r *http.Request) {
	messageID, ok := s.agentMessageID(w, r)
	if ok {
		s.handleAgentMessageEdit(w, r, messageID)
	}
}

func (s *server) handleAgentMessageDisposition(w http.ResponseWriter, r *http.Request) {
	messageID, ok := s.agentMessageID(w, r)
	if !ok {
		return
	}
	var req struct {
		ActorThreadRef string `json:"actorThreadRef"`
		Status         string `json:"status"`
		Note           string `json:"note,omitempty"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_status_request", "invalid message status request")
		return
	}
	req.ActorThreadRef, req.Status = strings.TrimSpace(req.ActorThreadRef), strings.TrimSpace(req.Status)
	if req.Status != agentMessageActed && req.Status != agentMessageDeclined && req.Status != agentMessageDeferred {
		httpapi.WriteError(w, http.StatusBadRequest, "status_invalid", "status must be acted, declined, or deferred")
		return
	}
	if len(req.Note) > maxAgentMessageNoteBytes {
		httpapi.WriteError(w, http.StatusBadRequest, "status_note_too_long", "note must be at most 500 UTF-8 bytes")
		return
	}
	actor, ok := s.resolveHostedThreadRecord(req.ActorThreadRef)
	if req.ActorThreadRef == "" || !ok {
		httpapi.WriteError(w, http.StatusBadRequest, "actor_not_hosted", "actorThreadRef must resolve to a hosted thread")
		return
	}
	name := strings.TrimSpace(s.displayNameForHosted(context.Background(), actor.Provider, actor.ID, actor.NativeThreadID))
	native := strings.TrimSpace(actor.NativeThreadID)
	if sessionruntime.NativeThreadIDProvisional(actor.Provider, native) {
		native = ""
	}
	message, feedback, err := s.mail.DispositionMessage(messageID, actor.ID, name, native, req.Status, req.Note, time.Now().UTC())
	if errors.Is(err, errAgentMessageNotFound) || errors.Is(err, sql.ErrNoRows) {
		httpapi.WriteError(w, http.StatusNotFound, "message_not_found", "message not found")
		return
	}
	if err != nil {
		var typed *agentMessageStoreError
		if errors.As(err, &typed) {
			status := http.StatusConflict
			if typed.Code == "not_recipient" {
				status = http.StatusForbidden
			}
			httpapi.WriteError(w, status, typed.Code, typed.Detail)
			return
		}
		httpapi.WriteError(w, http.StatusServiceUnavailable, "status_persistence_failed", err.Error())
		return
	}
	s.notifyMailChanged(actor.ID, message.Sender.HostedThreadID)
	httpapi.WriteJSON(w, map[string]any{"message": message, "feedback": feedback})
}

// handleAgentMessageEdit rewrites one operator-authored message while it is
// still unread. The store enforces both constraints; the previous
// content survives as an "edited" event on the message.
func (s *server) handleAgentMessageEdit(w http.ResponseWriter, r *http.Request, messageID string) {
	var req struct {
		Subject string `json:"subject,omitempty"`
		Body    string `json:"body"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_edit_request", "invalid message edit request")
		return
	}
	if len(req.Subject) > maxAgentMessageSubjectBytes {
		httpapi.WriteError(w, http.StatusBadRequest, "subject_too_long", "subject must be at most 120 UTF-8 bytes")
		return
	}
	if len(req.Body) == 0 || len(req.Body) > maxAgentMessageBodyBytes || strings.TrimSpace(req.Body) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "body_invalid", "body must be 1-16384 UTF-8 bytes and contain non-whitespace text")
		return
	}
	message, err := s.mail.EditMessage(messageID, req.Subject, req.Body, time.Now().UTC())
	if errors.Is(err, errAgentMessageNotFound) {
		httpapi.WriteError(w, http.StatusNotFound, "message_not_found", "message not found")
		return
	}
	if err != nil {
		var typed *agentMessageStoreError
		if errors.As(err, &typed) {
			httpapi.WriteError(w, http.StatusConflict, typed.Code, typed.Detail)
			return
		}
		httpapi.WriteError(w, http.StatusServiceUnavailable, "edit_persistence_failed", err.Error())
		return
	}
	s.notifyMailChanged(message.Recipient.HostedThreadID)
	httpapi.WriteJSON(w, message)
}
