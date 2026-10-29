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
	if err := sess.runtime.BeginProviderSpawn(ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: nativeID}, fake.daemon(), func() {}); err != nil {
		t.Fatal(err)
	}
	waitReconnectCondition(t, func() bool { return sess.view.Summary().Status == "ready" })
	t.Cleanup(sess.teardown.Close)
	return sess
}

// sessionWithOwners retains fixture state while connecting the concrete owners.
func sessionWithOwners(sess *Session) *Session {
	sess.wire.identity = &sess.identity
	sess.wire.delivery = &sess.delivery
	sess.wire.state = &sess.state
	sess.wire.runtime = &sess.runtime

	sess.wire.view = &sess.view
	sess.delivery.identity = &sess.identity
	sess.delivery.wire = &sess.wire
	sess.delivery.state = &sess.state
	sess.delivery.runtime = &sess.runtime
	sess.delivery.teardown = &sess.teardown
	sess.delivery.view = &sess.view
	sess.state.identity = &sess.identity
	sess.state.wire = &sess.wire
	sess.state.delivery = &sess.delivery
	sess.state.runtime = &sess.runtime

	sess.state.view = &sess.view
	sess.runtime.identity = &sess.identity
	sess.runtime.wire = &sess.wire
	sess.runtime.delivery = &sess.delivery
	sess.runtime.state = &sess.state

	sess.runtime.view = &sess.view
	sess.teardown.identity = &sess.identity
	sess.teardown.wire = &sess.wire
	sess.teardown.delivery = &sess.delivery
	sess.teardown.state = &sess.state
	sess.teardown.runtime = &sess.runtime

	sess.view.identity = &sess.identity
	sess.view.wire = &sess.wire

	sess.view.state = &sess.state
	sess.view.runtime = &sess.runtime

	return sess
}
