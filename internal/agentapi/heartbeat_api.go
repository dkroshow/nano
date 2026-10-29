package agentapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	featureheartbeat "github.com/dkroshow/nano/internal/features/heartbeat"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sessions"
)

// HandleHeartbeatAttachmentsList serves the configured heartbeat attachments.
func (s *Controller) HandleHeartbeatAttachmentsList(w http.ResponseWriter, r *http.Request) {
	if s.heartbeat == nil {
		httpapi.WriteHTTPError(w, "heartbeat store unavailable", http.StatusServiceUnavailable)
		return
	}
	rows, err := s.heartbeat.Store.ListAttachments(r.URL.Query().Get("includeRemoved") == "true")
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	httpapi.WriteJSON(w, map[string]any{"enabled": s.heartbeat.Enabled, "items": rows})
}

// HandleHeartbeatAttachmentCreate validates and creates one heartbeat attachment.
func (s *Controller) HandleHeartbeatAttachmentCreate(w http.ResponseWriter, r *http.Request) {
	if s.heartbeat == nil {
		httpapi.WriteHTTPError(w, "heartbeat store unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		TargetHostedThreadID string                           `json:"targetHostedThreadId"`
		CadenceMinutes       int                              `json:"cadenceMinutes"`
		OffsetMinutes        int                              `json:"offsetMinutes"`
		Prompt               string                           `json:"prompt"`
		PrePing              *heartbeatPrePingTemplateRequest `json:"prePing"`
		Authority            string                           `json:"authority"`
		Actor                featureheartbeat.Actor           `json:"actor"`
	}
	if err := decodeHeartbeatRequest(w, r, 128<<10, &body); err != nil {
		httpapi.WriteHTTPError(w, "invalid heartbeat attachment", http.StatusBadRequest)
		return
	}
	s.heartbeat.AttachmentMu.Lock()
	defer s.heartbeat.AttachmentMu.Unlock()
	target, ok := s.hosted.ResolveHostedRecord(body.TargetHostedThreadID)
	if !ok {
		httpapi.WriteHTTPError(w, "targetHostedThreadId does not resolve to a hosted thread", http.StatusBadRequest)
		return
	}
	if target.Status == sessions.StatusExited {
		httpapi.WriteHTTPError(w, "targetHostedThreadId resolves to a closed hosted thread", http.StatusBadRequest)
		return
	}
	normalizedActor, err := s.normalizeHeartbeatActor(body.Actor)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.CadenceMinutes < 1 || body.OffsetMinutes < 0 {
		httpapi.WriteHTTPError(w, "cadenceMinutes must be positive and offsetMinutes must not be negative", http.StatusBadRequest)
		return
	}
	if _, existingErr := s.heartbeat.Store.AttachmentForTarget(target.ID); existingErr == nil {
		httpapi.WriteHTTPError(w, featureheartbeat.ErrAttachmentExists.Error(), http.StatusConflict)
		return
	}
	var prePing *featureheartbeat.PrePingReference
	if body.PrePing != nil {
		prePing, body.Prompt, err = s.copyHeartbeatTemplate(*body.PrePing, normalizedActor, body.Authority, body.Prompt, time.Now())
		if err != nil {
			httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	rec, err := s.heartbeat.Store.CreateAttachment(featureheartbeat.AttachmentInput{
		TargetHostedThreadID: target.ID, CadenceMinutes: body.CadenceMinutes, OffsetMinutes: body.OffsetMinutes,
		Prompt: body.Prompt, PrePing: prePing, Authority: body.Authority, Actor: normalizedActor,
	}, time.Now())
	if err != nil {
		if errors.Is(err, featureheartbeat.ErrAttachmentExists) {
			httpapi.WriteHTTPError(w, err.Error(), http.StatusConflict)
			return
		}
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(rec)
}

func (s *Controller) normalizeHeartbeatActor(actor featureheartbeat.Actor) (featureheartbeat.Actor, error) {
	actor.Kind = strings.TrimSpace(actor.Kind)
	actor.Name = strings.TrimSpace(actor.Name)
	if actor.Kind == "" {
		return featureheartbeat.Actor{}, errors.New("actor.kind is required")
	}
	if actor.Kind == "hosted-thread" {
		record, ok := s.hosted.ResolveHostedRecord(actor.HostedThreadID)
		if !ok {
			return featureheartbeat.Actor{}, errors.New("actor hosted thread is unknown")
		}
		actor.HostedThreadID = record.ID
	}
	return actor, nil
}

func decodeHeartbeatRequest(w http.ResponseWriter, r *http.Request, limit int64, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func heartbeatMutationActor(w http.ResponseWriter, r *http.Request) (featureheartbeat.Actor, string, bool) {
	var body struct {
		Reason string                 `json:"reason"`
		Actor  featureheartbeat.Actor `json:"actor"`
	}
	if err := decodeHeartbeatRequest(w, r, 16<<10, &body); err != nil {
		httpapi.WriteHTTPError(w, "body requires actor and reason", http.StatusBadRequest)
		return featureheartbeat.Actor{}, "", false
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if strings.TrimSpace(body.Actor.Kind) == "" || body.Reason == "" {
		httpapi.WriteHTTPError(w, "body requires actor and reason", http.StatusBadRequest)
		return featureheartbeat.Actor{}, "", false
	}
	return body.Actor, body.Reason, true
}

func (s *Controller) heartbeatAttachmentID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.heartbeat == nil {
		httpapi.WriteHTTPError(w, "heartbeat store unavailable", http.StatusServiceUnavailable)
		return "", false
	}
	id := r.PathValue("attachmentId")
	if id == "" {
		httpapi.WriteNotFound(w)
		return "", false
	}
	return id, true
}

// HandleHeartbeatAttachmentRead serves one heartbeat attachment.
func (s *Controller) HandleHeartbeatAttachmentRead(w http.ResponseWriter, r *http.Request) {
	id, ok := s.heartbeatAttachmentID(w, r)
	if !ok {
		return
	}
	rec, err := s.heartbeat.Store.Attachment(id)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusNotFound)
		return
	}
	httpapi.WriteJSON(w, rec)
}

// HandleHeartbeatAttachmentUpdate validates changes to one heartbeat attachment.
func (s *Controller) HandleHeartbeatAttachmentUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.heartbeatAttachmentID(w, r)
	if !ok {
		return
	}
	var body struct {
		CadenceMinutes *int                   `json:"cadenceMinutes"`
		OffsetMinutes  *int                   `json:"offsetMinutes"`
		Prompt         *string                `json:"prompt"`
		PrePing        json.RawMessage        `json:"prePing"`
		Reason         string                 `json:"reason"`
		Actor          featureheartbeat.Actor `json:"actor"`
	}
	if err := decodeHeartbeatRequest(w, r, 128<<10, &body); err != nil {
		httpapi.WriteHTTPError(w, "invalid heartbeat attachment patch", http.StatusBadRequest)
		return
	}
	actor, err := s.normalizeHeartbeatActor(body.Actor)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	prePingTemplate, prePingSet, err := decodeHeartbeatPrePingPatch(body.PrePing)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	var prePing *featureheartbeat.PrePingReference
	if prePingTemplate != nil {
		current, currentErr := s.heartbeat.Store.Attachment(id)
		if currentErr != nil {
			httpapi.WriteHTTPError(w, currentErr.Error(), http.StatusNotFound)
			return
		}
		prompt := current.Prompt
		if body.Prompt != nil {
			prompt = *body.Prompt
		}
		prePing, _, err = s.copyHeartbeatTemplate(*prePingTemplate, actor, current.Authority, prompt, time.Now())
		if err != nil {
			httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	rec, err := s.heartbeat.Store.UpdateAttachment(id, featureheartbeat.AttachmentPatch{
		CadenceMinutes: body.CadenceMinutes, OffsetMinutes: body.OffsetMinutes, Prompt: body.Prompt,
		PrePingSet: prePingSet, PrePing: prePing,
	}, body.Reason, actor, time.Now())
	if err != nil {
		if errors.Is(err, featureheartbeat.ErrNotFound) {
			httpapi.WriteHTTPError(w, err.Error(), http.StatusNotFound)
			return
		}
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	httpapi.WriteJSON(w, rec)
}

// HandleHeartbeatAttachmentRemove removes the selected heartbeat attachment with its actor evidence.
func (s *Controller) HandleHeartbeatAttachmentRemove(w http.ResponseWriter, r *http.Request) {
	id, ok := s.heartbeatAttachmentID(w, r)
	if !ok {
		return
	}
	actor, reason, ok := heartbeatMutationActor(w, r)
	if !ok {
		return
	}
	actor, err := s.normalizeHeartbeatActor(actor)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	rec, err := s.heartbeat.Store.RemoveAttachment(id, reason, actor, time.Now())
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusNotFound)
		return
	}
	httpapi.WriteJSON(w, rec)
}

// HandleHeartbeatAttachmentPause pauses the selected heartbeat attachment.
func (s *Controller) HandleHeartbeatAttachmentPause(w http.ResponseWriter, r *http.Request) {
	s.handleHeartbeatAttachmentToggle(w, r, true)
}

// HandleHeartbeatAttachmentResume resumes the selected heartbeat attachment.
func (s *Controller) HandleHeartbeatAttachmentResume(w http.ResponseWriter, r *http.Request) {
	s.handleHeartbeatAttachmentToggle(w, r, false)
}

func (s *Controller) handleHeartbeatAttachmentToggle(w http.ResponseWriter, r *http.Request, pause bool) {
	id, ok := s.heartbeatAttachmentID(w, r)
	if !ok {
		return
	}
	actor, reason, ok := heartbeatMutationActor(w, r)
	if !ok {
		return
	}
	actor, err := s.normalizeHeartbeatActor(actor)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	var rec featureheartbeat.Attachment
	if pause {
		rec, err = s.heartbeat.Store.PauseAttachment(id, reason, actor, time.Now())
	} else {
		rec, err = s.heartbeat.Store.ResumeAttachment(id, reason, actor, time.Now())
	}
	if err != nil {
		if errors.Is(err, featureheartbeat.ErrNotFound) {
			httpapi.WriteHTTPError(w, err.Error(), http.StatusNotFound)
			return
		}
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	httpapi.WriteJSON(w, rec)
}

func decodeHeartbeatPrePingPatch(raw json.RawMessage) (*heartbeatPrePingTemplateRequest, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value heartbeatPrePingTemplateRequest
	if err := decoder.Decode(&value); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, false, errors.New("invalid heartbeat prePing reference")
	}
	return &value, true, nil
}

// HandleHeartbeatLog serves the bounded heartbeat decision log.
func (s *Controller) HandleHeartbeatLog(w http.ResponseWriter, r *http.Request) {
	if s.heartbeat == nil {
		httpapi.WriteHTTPError(w, "heartbeat store unavailable", http.StatusServiceUnavailable)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	attachmentID := strings.TrimSpace(r.URL.Query().Get("attachmentId"))
	items, err := s.heartbeat.Store.ListWakeLog(attachmentID, limit)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	httpapi.WriteJSON(w, map[string]any{"items": items})
}
