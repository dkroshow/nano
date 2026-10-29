package hosted

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	sessionruntime "github.com/dkroshow/nano/internal/sessions"
)

// HandleSpawnPreview returns the exact protocol payload a spawn request
// would produce — built by the same resolveSpawn/codexSessionRequest/claudeArgs
// code that spawnSession uses, so the New Window dialog's JSON pane shows
// the wire truth, not a frontend reconstruction.
// HandleSpawnPreview serves attachment provider configuration.
func (s *AttachmentCoordinator) HandleSpawnPreview(w http.ResponseWriter, r *http.Request) {
	var req SpawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Provider == "" {
		httpapi.WriteHTTPError(w, "body must be a spawn request", http.StatusBadRequest)
		return
	}
	resolved, err := s.resolveSpawn(req)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch resolved.Provider {
	case "codex":
		method, params := sessionruntime.CodexSessionRequest(resolved.ProviderRequest)
		out := map[string]any{
			"provider":      "codex",
			"method":        method,
			"params":        params,
			"turnOverrides": map[string]any{},
		}
		if resolved.Mode != "resume" {
			// The model rides thread/start at spawn time, so it is not a turn
			// override yet — the pane's per-turn control introduces one later.
			out["turnOverrides"] = sessionruntime.CodexTurnOverrides("", resolved.Effort, "", s.Defaults.ReasoningSummary)
		}
		httpapi.WriteJSON(w, out)
	case "claude":
		httpapi.WriteJSON(w, map[string]any{
			"provider": "claude",
			"argv":     sessionruntime.ClaudeCommand(resolved.ProviderRequest),
		})
	case "grok":
		method, params := sessionruntime.GrokSessionRequest(resolved.ProviderRequest)
		httpapi.WriteJSON(w, map[string]any{
			"provider": "grok",
			"argv":     sessionruntime.GrokCommand(resolved.ProviderRequest),
			"initialize": map[string]any{
				"protocolVersion": 1, "clientCapabilities": map[string]any{},
			},
			"method": method,
			"params": params,
		})
	}
}

// HandleCodexModels serves the (cached) codex model/list response so the New
// Window dialog can offer models, efforts, and service tiers from the wire.
// HandleCodexModels serves attachment provider configuration.
func (s *AttachmentCoordinator) HandleCodexModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.Codex.FetchModels(r.URL.Query().Get("refresh") == "true")
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(models)
}

// HandleGrokModels serves attachment provider configuration.
func (s *AttachmentCoordinator) HandleGrokModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	models, err := sessionruntime.FetchGrokModels(ctx, sessionruntime.ProviderRequest{Mode: "start", Cwd: s.Defaults.CWD, GrokBin: s.Defaults.GrokBin, GrokLeaderSocket: s.Defaults.GrokLeaderSocket})
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(models)
}
