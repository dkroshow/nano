package app

import (
	"strings"
	"testing"
	"time"

	coreturns "github.com/dkroshow/nano/internal/core/turns"
	hosted "github.com/dkroshow/nano/internal/hosted"
)

func TestCompletedTurnSourceDoesNotRequireHeartbeat(t *testing.T) {
	store := mustCompletedStore(t)
	s := &server{completedTurns: hosted.NewCompletedTurnSource(store)}
	if s.heartbeat != nil {
		t.Fatal("test requires no heartbeat")
	}
	if _, created, err := recordTestCompletedTurn(store, coreturns.Finish{HostedThreadID: "nht_target", Provider: "codex", TurnID: "turn_1", FinalMessageID: "final_1", Outcome: "completed", At: time.Now(), Source: "adapter"}); err != nil || !created {
		t.Fatalf("publish = %v %v", created, err)
	}
}

// The app-server's thread/start rejects sessionStartSource outside
// startup|clear as an unknown enum variant. Catching a bad value here, before
// the app-server sees it, keeps a caller's field mix-up (this value belongs
// on threadSource, a free-text tag) from minting a session that can never
// start — observed 2026-08-06: five background workers stuck exited, each
// carrying rpc error -32600 unknown variant `nano`.
func TestResolveSpawnRejectsInvalidSessionStartSource(t *testing.T) {
	cwd := t.TempDir()
	s := &server{defaults: sessionDefaults{cwd: cwd}}
	_, err := s.resolveSpawn(spawnRequest{ProviderRequest: providerRequest{Provider: "codex", Mode: "start", Cwd: cwd, SessionStartSource: "nano"}})
	if err == nil || !strings.Contains(err.Error(), "sessionStartSource") {
		t.Fatalf("resolveSpawn error = %v, want a sessionStartSource complaint", err)
	}
}

func TestResolveSpawnAcceptsKnownSessionStartSourceValues(t *testing.T) {
	cwd := t.TempDir()
	s := &server{defaults: sessionDefaults{cwd: cwd}}
	for _, value := range []string{"", "startup", "clear"} {
		if _, err := s.resolveSpawn(spawnRequest{ProviderRequest: providerRequest{Provider: "codex", Mode: "start", Cwd: cwd, SessionStartSource: value}}); err != nil {
			t.Fatalf("resolveSpawn(%q) error = %v", value, err)
		}
	}
}

func TestResolveSpawnDefaultsCodexServiceTierToStandard(t *testing.T) {
	cwd := t.TempDir()
	s := &server{defaults: sessionDefaults{cwd: cwd}}

	resolved, err := s.resolveSpawn(spawnRequest{ProviderRequest: providerRequest{Provider: "codex", Mode: "start", Cwd: cwd}})
	if err != nil {
		t.Fatalf("resolve codex start: %v", err)
	}
	if resolved.ServiceTier != "default" {
		t.Fatalf("default service tier = %q, want explicit Standard", resolved.ServiceTier)
	}

	resolved, err = s.resolveSpawn(spawnRequest{ProviderRequest: providerRequest{Provider: "codex", Mode: "start", Cwd: cwd, ServiceTier: "priority"}})
	if err != nil {
		t.Fatalf("resolve priority codex start: %v", err)
	}
	if resolved.ServiceTier != "priority" {
		t.Fatalf("explicit service tier = %q, want priority", resolved.ServiceTier)
	}
}
