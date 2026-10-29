package sessions

import (
	"testing"
)

// TestRegisterThreadKeepsFirstBinding pins the clobber guard: a conflicting
// registration keeps the first session's routing, and releaseThread only
// removes the entry for its owner.
func TestRegisterThreadKeepsFirstBinding(t *testing.T) {
	d := NewCodexDaemon("ws://unused", supervisedDaemonLabel, func() bool { return true })
	first := sessionWithOwners(&Session{id: "aaaa",
		stateOwner: State{
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	second := sessionWithOwners(&Session{id: "bbbb",
		stateOwner: State{
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	d.registerThread("t1", first)
	d.registerThread("t1", second)
	if d.sessions["t1"] != first {
		t.Fatalf("registerThread clobbered the first binding with %v", d.sessions["t1"])
	}
	d.releaseThread("t1", second)
	if d.sessions["t1"] != first {
		t.Fatal("releaseThread removed a routing entry it did not own")
	}
	d.releaseThread("t1", first)
	if _, ok := d.sessions["t1"]; ok {
		t.Fatal("releaseThread left the owner's entry in place")
	}
}
