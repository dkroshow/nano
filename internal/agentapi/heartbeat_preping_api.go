package agentapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	featureheartbeat "github.com/dkroshow/nano/internal/features/heartbeat"
	"github.com/dkroshow/nano/internal/httpapi"
)

// HandleHeartbeatScriptTemplates serves the repository-owned pre-ping templates.
func (s *Controller) HandleHeartbeatScriptTemplates(w http.ResponseWriter, r *http.Request) {
	items, err := featureheartbeat.LoadScriptTemplates()
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	httpapi.WriteJSON(w, map[string]any{"items": items})
}

type heartbeatPrePingTemplateRequest struct {
	TemplateID string          `json:"templateId"`
	Settings   json.RawMessage `json:"settings,omitempty"`
}

func (s *Controller) copyHeartbeatTemplate(body heartbeatPrePingTemplateRequest, actor featureheartbeat.Actor, authority, prompt string, now time.Time) (*featureheartbeat.PrePingReference, string, error) {
	template, err := featureheartbeat.ScriptTemplateByID(strings.TrimSpace(body.TemplateID))
	if err != nil {
		return nil, prompt, err
	}
	settings := body.Settings
	if len(settings) == 0 {
		settings = template.DefaultSettings
	}
	settings, err = featureheartbeat.NormalizeJSONObject(settings, 16<<10)
	if err != nil {
		return nil, prompt, err
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = template.DefaultPrompt
	}
	input := featureheartbeat.ScriptRevisionInput{
		Name: template.Name + " attachment copy", Source: template.Source, DefaultPrompt: prompt,
		DefaultSettings: settings, TemplateID: template.ID, TemplateRevision: template.Revision,
		Authority: strings.TrimSpace(authority), Actor: actor,
	}
	_, revision, err := s.heartbeat.Store.CreateScript(input, now)
	if err != nil {
		return nil, prompt, err
	}
	return &featureheartbeat.PrePingReference{ScriptID: revision.ScriptID, Revision: revision.Revision, Settings: settings}, prompt, nil
}

// HandleHeartbeatScripts serves the immutable stored script copies.
func (s *Controller) HandleHeartbeatScripts(w http.ResponseWriter, r *http.Request) {
	if s.heartbeat == nil {
		httpapi.WriteHTTPError(w, "heartbeat store unavailable", http.StatusServiceUnavailable)
		return
	}
	items, err := s.heartbeat.Store.ListScripts()
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	httpapi.WriteJSON(w, map[string]any{"items": items})
}

// HandleHeartbeatScriptRead serves one stored script revision.
func (s *Controller) HandleHeartbeatScriptRead(w http.ResponseWriter, r *http.Request) {
	if s.heartbeat == nil {
		httpapi.WriteHTTPError(w, "heartbeat store unavailable", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("scriptId")
	if id == "" {
		httpapi.WriteNotFound(w)
		return
	}
	script, err := s.heartbeat.Store.Script(id)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusNotFound)
		return
	}
	revision := script.CurrentRevision
	if rawRevision := strings.TrimSpace(r.URL.Query().Get("revision")); rawRevision != "" {
		revision, err = strconv.ParseInt(rawRevision, 10, 64)
		if err != nil || revision < 1 {
			httpapi.WriteHTTPError(w, "revision must be a positive integer", http.StatusBadRequest)
			return
		}
	}
	value, err := s.heartbeat.Store.ScriptRevision(id, revision)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusNotFound)
		return
	}
	httpapi.WriteJSON(w, map[string]any{"script": script, "revision": value})
}

// HandleHeartbeatScriptUnsupported rejects unsupported script mutation routes.
func (s *Controller) HandleHeartbeatScriptUnsupported(w http.ResponseWriter, _ *http.Request) {
	httpapi.WriteHTTPError(w, "GET only; heartbeat scripts are immutable attachment copies", http.StatusMethodNotAllowed)
}
