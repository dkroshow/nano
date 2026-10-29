package app

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// A fresh claude start hands the session back before its transport is up, the
// same contract resume has had since Slice R3. The bug it fixes: the New Window
// dialog sat on "Creating…" for as long as the shim (or the app-server, on the
// codex side) took to answer, which reads as a hung app rather than a slow one.

func postClaudeStart(t *testing.T, s *server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	body := `{"provider":"claude","mode":"start","cwd":"` + t.TempDir() + `"}`
	fixtureOwners(s).hostedController().HandleLegacyCreate(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body)))
	return rec
}

// startingShimID is shaped like a real shim session id: only a cs_ id is a
// provisional Claude identity that a later init may resolve.
const startingShimID = "cs_starting1"

// startingShim answers session/start only after `hold` closes, so a test can
// keep the handshake in flight for as long as it needs. Like a real `claude -p`
// child, which sends no init before Nano has adopted its provisional identity,
// it sends the init line only after `speak` closes (provisionallyAdopted).
// Everything after that (input, close) is answered normally and recorded.
func startingShim(t *testing.T, hold, speak <-chan struct{}) *fakeShim {
	t.Helper()
	return newFakeShim(t, func(c *shimConn) {
		start, ok := c.Next()
		if !ok {
			return
		}
		<-hold
		c.Reply(start, map[string]any{"shimSessionId": startingShimID})
		<-speak
		c.Line(1, "recv", claudeInitLine)
		c.AnswerRest()
	})
}

// provisionallyAdopted waits until the session's shim id is its recorded
// provisional identity, then lets the shim send the init that resolves it.
// Sent sooner, the init can race the adoption and find no handle to resolve.
func provisionallyAdopted(t *testing.T, sess *session, speak chan struct{}) {
	t.Helper()
	waitForClaude(t, "the shim id to be the recorded provisional identity", func() bool {
		view := sess.Summary().Metadata
		return view != nil && view.NativeThreadID == startingShimID
	})
	close(speak)
}

// TestClaudeStartReturnsStartingWhileTransportComesUp: the POST answers while
// session/start is still unanswered, with the spawn intent on the summary so
// the pane can say "creating" rather than the resume path's "attaching".
func TestClaudeStartReturnsStartingWhileTransportComesUp(t *testing.T) {
	hold, speak := make(chan struct{}), make(chan struct{})
	shim := startingShim(t, hold, speak)
	s, _ := claudeResumeServer(t, shim.WSURL())
	migrateTestControl(t, s.controlMetadata) // the init resolves the provisional handle, as on the real schema

	begin := time.Now()
	sum := spawnedSummary(t, postClaudeStart(t, s))
	elapsed := time.Since(begin)
	if elapsed > time.Second {
		t.Fatalf("start POST took %v while the shim held session/start — it waited on the handshake", elapsed)
	}
	if sum.Status != "starting" || sum.ThreadID != "" || sum.Error != "" {
		t.Fatalf("summary = %#v, want a starting session with no thread id yet", sum)
	}
	if sum.SpawnMode != "start" {
		t.Fatalf("spawnMode = %q, want start", sum.SpawnMode)
	}
	// Listed from the moment the POST returns: the card is on screen while the
	// child is still coming up.
	sess := s.LookupSession(sum.ID)
	if sess == nil {
		t.Fatal("a starting session is not listed")
	}

	close(hold)
	provisionallyAdopted(t, sess, speak)
	waitForClaudeThreadID(t, sess, "native-1")
	waitForClaudeStatus(t, sess, "ready")
	if got := sess.Summary().Status; got != "ready" {
		t.Fatalf("status = %q after the transport came up, want ready", got)
	}
}

// TestClaudeStartTransportFailureLandsOnTheSession: with the spawn now async,
// a transport that cannot come up has nowhere to fail but the session card —
// exited, with the reason attached (the R3 failure contract).
func TestClaudeStartTransportFailureLandsOnTheSession(t *testing.T) {
	// The shim refuses, and the direct-child fallback runs a binary that is not
	// there: both rungs of the start path fail.
	shim := newFakeShim(t, func(c *shimConn) {
		start, ok := c.Next()
		if !ok {
			return
		}
		c.ReplyError(start, shimCodeSessionExited, "child died on exec")
		c.AnswerRest() // stay connected: a dropped connection is its own exit path
	})
	s, _ := claudeResumeServer(t, shim.WSURL())
	s.defaults.ClaudeBin = filepath.Join(t.TempDir(), "no-such-claude")

	sum := spawnedSummary(t, postClaudeStart(t, s))
	if sum.Status != "starting" {
		t.Fatalf("summary = %#v, want the failure to land on the session rather than the POST", sum)
	}
	sess := s.LookupSession(sum.ID)
	waitForClaude(t, "the spawn failure to reach the session", func() bool {
		return strings.Contains(sess.Summary().Error, "starting claude")
	})
	if got := sess.Summary().Status; got != sessionStatusExited {
		t.Fatalf("status = %q, want exited alongside the error (the R3 contract)", got)
	}
}

// stubClaudeTransport answers only what captureInit asks of a transport: the
// lines are live, and the child is durable enough to be worth remembering.
type stubClaudeTransport struct{}

func (stubClaudeTransport) sendLine([]byte) error { return nil }
func (stubClaudeTransport) isReplaying() bool     { return false }
func (stubClaudeTransport) lastSeq() int64        { return 0 }
func (stubClaudeTransport) close() error          { return nil }
func (stubClaudeTransport) durableAttachment(nativeThreadID, model string) (hostedstore.Attachment, bool) {
	return hostedstore.Attachment{Provider: "claude", NativeThreadID: nativeThreadID, Model: model}, true
}

// A close before the transport exists must fail safely. Once start settles,
// retry closes the exact child and removes the session only after confirmation.
func TestCloseDuringClaudeStartClosesTheLateTransport(t *testing.T) {
	hold, speak := make(chan struct{}), make(chan struct{})
	shim := startingShim(t, hold, speak)
	s, _ := claudeResumeServer(t, shim.WSURL())
	migrateTestControl(t, s.controlMetadata) // the init resolves the provisional handle, as on the real schema

	sum := spawnedSummary(t, postClaudeStart(t, s))
	sess := s.LookupSession(sum.ID)
	if sess == nil {
		t.Fatal("a starting session is not listed")
	}

	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sum.ID, nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("unconfirmed close status = %d, body = %s", rec.Code, rec.Body.String())
	}
	retained := s.LookupSession(sum.ID) == sess
	if !retained || len(shim.CallsOf(shimMethodClose)) != 0 {
		t.Fatal("unconfirmed close removed the session or attempted an unidentified stop")
	}
	close(hold)
	provisionallyAdopted(t, sess, speak)
	waitForClaudeThreadID(t, sess, "native-1")
	waitForClaudeStatus(t, sess, "ready")
	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sum.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, body = %s", rec.Code, rec.Body.String())
	}

	waitForClaude(t, "the orphaned shim session to be closed", func() bool {
		return len(shim.CallsOf(shimMethodClose)) == 1
	})
	if got := sess.Summary().Status; got != sessionStatusExited {
		t.Fatalf("status = %q after a close raced the handshake, want it to stay exited", got)
	}
	if got := sess.Summary().Error; got != "" {
		t.Fatalf("closing a starting session reported a spawn failure: %q", got)
	}
	listed := s.LookupSession(sum.ID) != nil
	if listed {
		t.Fatal("the closed session came back into the session list")
	}
}
