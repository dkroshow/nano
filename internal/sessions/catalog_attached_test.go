package sessions

import (
	"testing"

	"github.com/dkroshow/nano/internal/testsupport"
)

// Failure caught: a late identity event on a closed attachment
// re-established a watch, or a close racing the bind left one behind.
// Boundary: the session's bind on native identity.
func TestSessionCatalogBindNeverOutlivesClose(t *testing.T) {
	sources := &testsupport.Sources{}
	sess := sessionWithOwners(&Session{id: "s1", provider: "claude", cwd: "/work", catalogSources: sources})
	sess.setProjectionThreadID("native-1")
	if len(sources.Binds) != 1 || sources.Binds[0] != "claude/native-1@/work|" {
		t.Fatalf("binds = %q, want the identity once", sources.Binds)
	}
	sources.OnBind = func() { sess.closed = true } // a close lands while the hold is made
	sess.setProjectionThreadID("native-2")
	if sources.Releases != 1 {
		t.Fatalf("releases after a racing close = %d, want 1", sources.Releases)
	}
	sources.OnBind = nil
	sess.setProjectionThreadID("native-3")
	if len(sources.Binds) != 2 {
		t.Fatalf("a closed attachment bound again: %q", sources.Binds)
	}
}
