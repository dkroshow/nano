package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dkroshow/nano/internal/config"
)

func TestClaudePreviewUsesResolvedBinary(t *testing.T) {
	cwd := t.TempDir()
	s := &server{defaults: config.SessionDefaults{
		CWD:          cwd,
		Model:        "sonnet",
		AllowedTools: "Read,Grep",
		ClaudeBin:    "/test/bin/claude",
	}}
	body, _ := json.Marshal(map[string]string{
		"provider": "claude",
		"model":    "fable",
		"effort":   "xhigh",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/preview", bytes.NewReader(body))
	s.attachmentCoordinator().HandleSpawnPreview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Argv []string `json:"argv"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(res.Argv) == 0 || res.Argv[0] != "/test/bin/claude" {
		t.Fatalf("argv = %#v, want resolved binary first", res.Argv)
	}
	if !containsArgPair(res.Argv, "--model", "fable") || !containsArgPair(res.Argv, "--effort", "xhigh") {
		t.Fatalf("argv missing model/effort flags: %#v", res.Argv)
	}
}

func TestClaudePreviewPassesExactPinnedModels(t *testing.T) {
	s := &server{defaults: config.SessionDefaults{
		CWD:       t.TempDir(),
		ClaudeBin: "/test/bin/claude",
	}}
	for _, model := range []string{
		"claude-fable-5-1",
		"claude-mythos-5-1",
		"claude-fable-5",
	} {
		t.Run(model, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"provider": "claude", "model": model})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/sessions/preview", bytes.NewReader(body))
			s.attachmentCoordinator().HandleSpawnPreview(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var res struct {
				Argv []string `json:"argv"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if !containsArgPair(res.Argv, "--model", model) {
				t.Fatalf("argv = %#v, want exact model %q", res.Argv, model)
			}
		})
	}
}

func TestClaudeBypassPermissionsDoesNotApplyAllowedToolsDefault(t *testing.T) {
	cwd := t.TempDir()
	s := &server{defaults: config.SessionDefaults{
		CWD:          cwd,
		AllowedTools: "Read,Grep",
		ClaudeBin:    "/test/bin/claude",
	}}
	body, _ := json.Marshal(map[string]string{
		"provider":       "claude",
		"permissionMode": "bypassPermissions",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/preview", bytes.NewReader(body))
	s.attachmentCoordinator().HandleSpawnPreview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Argv []string `json:"argv"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if containsArgPair(res.Argv, "--allowed-tools", "Read") || containsArgPair(res.Argv, "--allowed-tools", "Grep") {
		t.Fatalf("bypass permissions should not inherit allowed-tools defaults: %#v", res.Argv)
	}
	if !containsArgPair(res.Argv, "--permission-mode", "bypassPermissions") {
		t.Fatalf("argv missing bypass permission mode: %#v", res.Argv)
	}
}
