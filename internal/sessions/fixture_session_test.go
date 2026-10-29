package sessions

import (
	"testing"
)

// newFixtureSession configures an unstarted attachment through its real constructor.
func newFixtureSession(cfg Config, nativeID string) *Session {
	sess := New(cfg)
	if nativeID != "" {
		sess.ReserveNativeID(nativeID)
	}
	return sess
}

// resumeCodexFixture connects a configured attachment through the scripted transport.
func resumeCodexFixture(t *testing.T, cfg Config, nativeID string, fake *fakeAppServer) *Session {
	t.Helper()
	sess := New(cfg)
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	sess.ReserveNativeID(nativeID)
	if err := sess.BeginProviderSpawn(ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: nativeID}, fake.daemon(), func() {}); err != nil {
		t.Fatal(err)
	}
	waitReconnectCondition(t, func() bool { return sess.Summary().Status == "ready" })
	t.Cleanup(sess.Close)
	return sess
}

// sessionWithOwners retains fixture state while connecting the concrete owners.
func sessionWithOwners(sess *Session) *Session {
	sess.wire.session = sess
	return sess
}
