package app

import (
	"github.com/dkroshow/nano/internal/hosted"
	"strings"
	"testing"
)

// End to end through spawnSession: a New Window (UI-path) claude start reaches
// the shim carrying --append-system-prompt with the session's own minted
// hosted id — proving both the identity flag and the hostedThreadID
// write-back that feeds it (slice-1 checklist items 1 and 3).
func TestClaudeStartSpawnCarriesIdentity(t *testing.T) {
	// A fresh start, so the shim behaves as a real one does: it sends the init
	// only after Nano has adopted its cs_ id (startingShim).
	hold, speak := make(chan struct{}), make(chan struct{})
	close(hold)
	shim := startingShim(t, hold, speak)
	s, _ := claudeResumeServer(t, shim.WSURL())
	migrateTestControl(t, s.controlMetadata)
	s.defaults.APIOrigin = "http://127.0.0.1:7999"

	sess, err := s.attachmentCoordinator().Spawn(hosted.SpawnRequest{ProviderRequest: providerRequest{Provider: "claude", Cwd: t.TempDir()}})
	if err != nil {
		t.Fatalf("start spawn: %v", err)
	}
	provisionallyAdopted(t, sess, speak)
	waitForResumedThreadID(t, s, sess.ID(), "native-1")

	if sess.HostedThreadID() == "" {
		t.Fatal("start spawn minted no hosted thread id")
	}
	_, args, _ := startArgs(t, shim)
	block := singletonValue(t, args, claudeIdentityFlag)
	if !strings.Contains(block, "hostedThreadId: "+sess.HostedThreadID()) {
		t.Fatalf("injected block = %q, want the session's own hosted id %s", block, sess.HostedThreadID())
	}
	if !strings.Contains(block, "http://127.0.0.1:7999/api/v1/hosted-threads/"+sess.HostedThreadID()) {
		t.Fatalf("injected block = %q, want the self-lookup URL", block)
	}
}

// A cs_ shim stand-in must never pass as a real provider id: the view marks
// it provisional, and a genuine UUID clears the marker. Live instance that
// motivated this: the grok-acp-projector worker record of 2026-08-05.
func TestAgentSessionViewMarksProvisionalNativeID(t *testing.T) {
	_, store := newTestHostedControl(t)
	hosted, _, err := fixtureBeginSession(store, "caller", "hash", "claude", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{Registry: newSessionRegistry(), agentExecutions: store}

	if err := store.BindSession(hosted.ID, "nano-1", "cs_eec9373f24115d539dd1c06786b96b36", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	if view := s.agentSessionView(hosted.ID); !view.NativeThreadIDProvisional {
		t.Fatalf("view = %+v, want the cs_ stand-in marked provisional", view)
	}

	resolveTestProvisional(t, store.Control(), hosted.ID, "claude", "cs_eec9373f24115d539dd1c06786b96b36", "2dd36191-9386-542b-a1ef-bed95e638620")
	if err := store.BindSession(hosted.ID, "nano-1", "2dd36191-9386-542b-a1ef-bed95e638620", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	if view := s.agentSessionView(hosted.ID); view.NativeThreadIDProvisional {
		t.Fatalf("view = %+v, want a real UUID unmarked", view)
	}
}
