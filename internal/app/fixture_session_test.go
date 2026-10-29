package app

import (
	"testing"

	"github.com/dkroshow/nano/internal/sessions"
)

// newFixtureSession configures an unstarted attachment through its real constructor.
func newFixtureSession(cfg sessions.Config, nativeID string) *sessions.Session {
	sess := newSession(cfg)
	if nativeID != "" {
		sess.ReserveNativeID(nativeID)
	}
	return sess
}

// resumeCodexFixture connects a configured attachment through the scripted transport.
func resumeCodexFixture(t *testing.T, cfg sessions.Config, nativeID string, fake *fakeAppServer) *sessions.Session {
	t.Helper()
	sess := newSession(cfg)
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	sess.ReserveNativeID(nativeID)
	if err := sess.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: nativeID}, fake.daemon(), func() {}); err != nil {
		t.Fatal(err)
	}
	waitReconnectCondition(t, func() bool { return sess.Summary().Status == "ready" })
	t.Cleanup(sess.Close)
	return sess
}
