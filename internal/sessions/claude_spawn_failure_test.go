package sessions

import (
	"errors"
	"strings"
	"testing"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	shimfixture "github.com/dkroshow/nano/internal/testsupport/claudeshim"
)

const claudeMissingTranscriptResult = shimfixture.MissingTranscriptResult

// rejectingNativeClaim records the hosted owner's missing-identity refusal.
type rejectingNativeClaim struct{ Metadata }

func (rejectingNativeClaim) ClaimNativeIDResolving(string, string, string, bool, *hostedstore.NativeResolution) error {
	return errors.New("hosted identity absent")
}

// The pre-init guard keys on the handshake, not on the thread id. Identity at
// spawn hands a `--resume` session its predecessor's id before any wire traffic,
// so a guard that read that id would make this translation unreachable on
// exactly the rung most likely to need it.
func TestClaudeSpawnFailureTranslatedDespiteTheSeededThreadID(t *testing.T) {
	sess, _ := claudeTestSession(t)
	sess.stateOwner.threadID = "native-1" // identity at spawn, no handshake yet
	c := &claudeSession{sess: sess, exited: make(chan struct{})}

	c.captureState(&claudeChildTransport{owner: c}, []byte(claudeMissingTranscriptResult))

	sum := sess.Summary()
	if sum.Status != "exited" || !strings.Contains(sum.Error, "no longer on disk") {
		t.Fatalf("summary = %#v, want the pre-init failure translated", sum)
	}
}

// A failing result after the handshake is one turn's error, not the session's:
// a live session must not be marked as a failed spawn by it.
func TestClaudeResultErrorAfterInitLeavesTheSessionAlone(t *testing.T) {
	sess, _ := claudeTestSession(t)
	c := &claudeSession{sess: sess, exited: make(chan struct{})}
	tr := &claudeChildTransport{owner: c}

	c.markTransportReady()
	c.captureState(tr, []byte(claudeInitLine))
	c.captureState(tr, []byte(claudeMissingTranscriptResult))

	if sum := sess.Summary(); sum.Status != "ready" || sum.Error != "" {
		t.Fatalf("summary = %#v, want the handshaken session untouched", sum)
	}

	// The same holds when the handshake's provisional pair cannot be
	// recorded: the init still happened, so a later error result is not a
	// failed spawn, and the session keeps its provisional identity.
	unrecorded, _ := claudeTestSession(t)
	unrecorded.hostedThreadID = "nht_unrecorded"
	unrecorded.metadata = rejectingNativeClaim{unrecorded.metadata} // models the hosted owner refusing the absent identity
	unrecorded.stateOwner.threadID = "cs_handle"
	child := &claudeSession{sess: unrecorded, exited: make(chan struct{}), supersedes: "cs_handle"}
	shim := &claudeShimTransport{owner: child, shimSessionID: "cs_handle"}
	child.markTransportReady()
	child.captureState(shim, []byte(claudeInitLine))
	child.captureState(shim, []byte(claudeMissingTranscriptResult))
	if sum := unrecorded.Summary(); sum.Status == "exited" || strings.Contains(sum.Error, "no longer on disk") {
		t.Fatalf("summary = %#v; a handshake whose pair was not recorded became a failed spawn", sum)
	}
	if got := unrecorded.stateOwner.ThreadID(); got != "cs_handle" {
		t.Fatalf("identity = %q; want it to stay provisional until the pair is recorded", got)
	}
}

// TestClaudeInitAfterCloseDoesNotResurrectTheSession: a fresh start's child
// announces itself asynchronously, so the handshake can land after the operator
// has already closed the session. Adopting it then would flip a torn-down
// session back to ready and leave a durable attachment record offering to
// re-attach to a session that was deliberately let go of.
func TestClaudeInitAfterCloseDoesNotResurrectTheSession(t *testing.T) {
	sess, attachments := claudeTestSession(t)
	c := newClaudeSession(sess, "")
	sess.Close()

	c.captureState(&claudeChildTransport{owner: c}, []byte(claudeInitLine))

	sum := sess.Summary()
	if sum.Status != StatusExited {
		t.Fatalf("status = %q after a post-close init, want it to stay exited", sum.Status)
	}
	if sum.ThreadID != "" {
		t.Fatalf("threadId = %q, want a closed session to take no identity", sum.ThreadID)
	}
	if recs := attachments.All(); len(recs) != 0 {
		t.Fatalf("a closed session was remembered as re-attachable: %+v", recs)
	}
}
