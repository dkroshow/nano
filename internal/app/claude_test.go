package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveClaudeBinPrefersConfiguredAndEnv(t *testing.T) {
	t.Setenv(claudeBinEnv, "/env/claude")
	if got := resolveClaudeBin("/flag/claude"); got != "/flag/claude" {
		t.Fatalf("configured bin = %q, want /flag/claude", got)
	}
	if got := resolveClaudeBin(""); got != "/env/claude" {
		t.Fatalf("env bin = %q, want /env/claude", got)
	}
}

func TestResolveClaudeBinFindsLocalInstallOutsidePATH(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(bin), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv(claudeBinEnv, "")

	if got := resolveClaudeBin(""); got != bin {
		t.Fatalf("resolved bin = %q, want %q", got, bin)
	}
}

func TestClaudePreviewUsesResolvedBinary(t *testing.T) {
	cwd := t.TempDir()
	s := &server{defaults: sessionDefaults{
		cwd:          cwd,
		model:        "sonnet",
		allowedTools: "Read,Grep",
		claudeBin:    "/test/bin/claude",
	}}
	body, _ := json.Marshal(map[string]string{
		"provider": "claude",
		"model":    "fable",
		"effort":   "xhigh",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/preview", bytes.NewReader(body))
	s.handleSpawnPreview(rec, req)
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
	s := &server{defaults: sessionDefaults{
		cwd:       t.TempDir(),
		claudeBin: "/test/bin/claude",
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
			s.handleSpawnPreview(rec, req)
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
	s := &server{defaults: sessionDefaults{
		cwd:          cwd,
		allowedTools: "Read,Grep",
		claudeBin:    "/test/bin/claude",
	}}
	body, _ := json.Marshal(map[string]string{
		"provider":       "claude",
		"permissionMode": "bypassPermissions",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/preview", bytes.NewReader(body))
	s.handleSpawnPreview(rec, req)
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

func TestClaudeStatusClassifierSentinelDisablesAllTools(t *testing.T) {
	args := claudeArgs(spawnRequest{PermissionMode: "bypassPermissions", AllowedTools: claudeNoToolsSentinel})
	if !containsArgPair(args, "--tools", "") {
		t.Fatalf("status classifier argv does not disable tools: %#v", args)
	}
	if containsArgPair(args, "--allowed-tools", claudeNoToolsSentinel) {
		t.Fatalf("internal sentinel leaked as an ordinary permission rule: %#v", args)
	}
}

func TestClaudeAllowedToolsReadGrepBashCurlParityRegression(t *testing.T) {
	args := claudeArgs(spawnRequest{AllowedTools: "Read,Grep"})
	if !containsArgPair(args, "--allowed-tools", "Read") || !containsArgPair(args, "--allowed-tools", "Grep") {
		t.Fatalf("Read,Grep probe lost its requested tools: %#v", args)
	}
	if containsArgPair(args, "--allowed-tools", "Bash") || containsArgPair(args, "--allowed-tools", "curl") {
		t.Fatalf("Read,Grep Bash/curl parity probe widened its advertised tools: %#v", args)
	}
}

func containsArgPair(argv []string, key, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == key && argv[i+1] == value {
			return true
		}
	}
	return false
}
