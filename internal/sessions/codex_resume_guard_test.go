package sessions

import (
	"testing"
)

// TestRegisterThreadKeepsFirstBinding pins the clobber guard: a conflicting
// registration keeps the first session's routing, and releaseThread only
// removes the entry for its owner.
func TestRegisterThreadKeepsFirstBinding(t *testing.T) {
	d := NewCodexDaemon("ws://unused", supervisedDaemonLabel, func() bool { return true })
	first := sessionWithOwners(&Session{
		state: State{
			lifecycleState: testLifecycleSnapshot("ready", false)},
		identity: attachmentIdentity{id: "aaaa"},
	})
	second := sessionWithOwners(&Session{
		state: State{
			lifecycleState: testLifecycleSnapshot("ready", false)},
		identity: attachmentIdentity{id: "bbbb"},
	})
	d.registerThread("t1", first.Runtime())
	d.registerThread("t1", second.Runtime())
	if d.sessions["t1"] != first.Runtime() {
		t.Fatalf("registerThread clobbered the first binding with %v", d.sessions["t1"])
	}
	d.releaseThread("t1", second.Runtime())
	if d.sessions["t1"] != first.Runtime() {
		t.Fatal("releaseThread removed a routing entry it did not own")
	}
	d.releaseThread("t1", first.Runtime())
	if _, ok := d.sessions["t1"]; ok {
		t.Fatal("releaseThread left the owner's entry in place")
	}
}
