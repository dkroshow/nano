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
	t.Cleanup(sess.lifecycle.Close)
	return sess
}

// sessionWithOwners retains fixture state while connecting the concrete owners.
func sessionWithOwners(sess *Session) *Session {
	sess.wire.identity = &sess.identity
	sess.wire.delivery = &sess.delivery
	sess.wire.stateOwner = &sess.stateOwner
	sess.wire.runtime = &sess.runtime
	sess.wire.lifecycle = &sess.lifecycle
	sess.wire.view = &sess.view
	sess.delivery.identity = &sess.identity
	sess.delivery.wire = &sess.wire
	sess.delivery.stateOwner = &sess.stateOwner
	sess.delivery.runtime = &sess.runtime
	sess.delivery.lifecycle = &sess.lifecycle
	sess.delivery.view = &sess.view
	sess.stateOwner.identity = &sess.identity
	sess.stateOwner.wire = &sess.wire
	sess.stateOwner.delivery = &sess.delivery
	sess.stateOwner.runtime = &sess.runtime
	sess.stateOwner.lifecycle = &sess.lifecycle
	sess.stateOwner.view = &sess.view
	sess.runtime.identity = &sess.identity
	sess.runtime.wire = &sess.wire
	sess.runtime.delivery = &sess.delivery
	sess.runtime.stateOwner = &sess.stateOwner
	sess.runtime.lifecycle = &sess.lifecycle
	sess.runtime.view = &sess.view
	sess.lifecycle.identity = &sess.identity
	sess.lifecycle.wire = &sess.wire
	sess.lifecycle.delivery = &sess.delivery
	sess.lifecycle.stateOwner = &sess.stateOwner
	sess.lifecycle.runtime = &sess.runtime
	sess.lifecycle.view = &sess.view
	sess.view.identity = &sess.identity
	sess.view.wire = &sess.wire
	sess.view.delivery = &sess.delivery
	sess.view.stateOwner = &sess.stateOwner
	sess.view.runtime = &sess.runtime
	sess.view.lifecycle = &sess.lifecycle
	return sess
}
