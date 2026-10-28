package app

import (
	"testing"

	"github.com/dkroshow/nano/internal/config"
)

// Catches: a Codex child started without a sandbox field getting a read-only
// sandbox, against the permissive default launch policy.
// Boundary: Nano's flag defaults, which the launchd job leaves unset, through
// the start resolver that fills an omitted sandbox and approval.
func TestCodexStartWithoutSandboxGetsPermissiveDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{defaults: sessionDefaults{cwd: t.TempDir(), sandbox: cfg.Sandbox, approval: cfg.Approval}}
	req, err := s.resolveSpawn(spawnRequest{providerRequest: providerRequest{Provider: "codex", Mode: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	if req.Sandbox != "danger-full-access" || req.Approval != "never" {
		t.Fatalf("omitted Codex posture resolved to sandbox=%q approval=%q", req.Sandbox, req.Approval)
	}
}
