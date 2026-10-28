package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveClaudeBinPrefersConfiguredAndEnv(t *testing.T) {
	t.Setenv(claudeBinEnv, "/env/claude")
	if got := ResolveClaudeBin("/flag/claude"); got != "/flag/claude" {
		t.Fatalf("configured bin = %q, want /flag/claude", got)
	}
	if got := ResolveClaudeBin(""); got != "/env/claude" {
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

	if got := ResolveClaudeBin(""); got != bin {
		t.Fatalf("resolved bin = %q, want %q", got, bin)
	}
}

func TestClaudeStatusClassifierSentinelDisablesAllTools(t *testing.T) {
	args := ClaudeArgs(ProviderRequest{PermissionMode: "bypassPermissions", AllowedTools: claudeNoToolsSentinel})
	if !containsArgPair(args, "--tools", "") {
		t.Fatalf("status classifier argv does not disable tools: %#v", args)
	}
	if containsArgPair(args, "--allowed-tools", claudeNoToolsSentinel) {
		t.Fatalf("internal sentinel leaked as an ordinary permission rule: %#v", args)
	}
}

func TestClaudeAllowedToolsReadGrepBashCurlParityRegression(t *testing.T) {
	args := ClaudeArgs(ProviderRequest{AllowedTools: "Read,Grep"})
	if !containsArgPair(args, "--allowed-tools", "Read") || !containsArgPair(args, "--allowed-tools", "Grep") {
		t.Fatalf("Read,Grep probe lost its requested tools: %#v", args)
	}
	if containsArgPair(args, "--allowed-tools", "Bash") || containsArgPair(args, "--allowed-tools", "curl") {
		t.Fatalf("Read,Grep Bash/curl parity probe widened its advertised tools: %#v", args)
	}
}
