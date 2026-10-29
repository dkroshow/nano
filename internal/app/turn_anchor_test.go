package app

import (
	"testing"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/sidebar"
)

// The claude anchor's whole point is surviving a nano restart, which means
// surviving a JSON round-trip through attachments.json. A record written by an
// older nano has neither field and must degrade to "no anchor" — never to a
// zero that reads as 1970.
func TestAttachmentRecordRoundTripsTurnAnchor(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/attachments.json"
	store, err := hostedstore.NewAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Remember(hostedstore.Attachment{Provider: "claude", NativeThreadID: "native-1", Cwd: dir})
	store.SetTurnAnchor("claude", "native-1", 1785510514000, 42)

	reloaded, err := hostedstore.NewAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := reloaded.Get("claude", "native-1")
	if !ok {
		t.Fatal("record did not survive the reload")
	}
	if rec.TurnStartedAtMs != 1785510514000 || rec.TurnAnchorSeq != 42 {
		t.Fatalf("anchor round-trip = (%d, %d), want (1785510514000, 42)", rec.TurnStartedAtMs, rec.TurnAnchorSeq)
	}

	store.SetTurnAnchor("claude", "native-1", 0, 0)
	cleared, err := hostedstore.NewAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := cleared.Get("claude", "native-1"); rec.TurnStartedAtMs != 0 || rec.TurnAnchorSeq != 0 {
		t.Fatalf("anchor after clear = (%d, %d), want zeroes", rec.TurnStartedAtMs, rec.TurnAnchorSeq)
	}
}

// An anchor is only meaningful for a session that outlives nano, and only a
// durable transport gets a record. Writing one for a thread with no record
// would offer a recovery that cannot happen.
func TestSetTurnAnchorNeverCreatesARecord(t *testing.T) {
	store, err := hostedstore.NewAttachmentStore(t.TempDir() + "/attachments.json")
	if err != nil {
		t.Fatal(err)
	}
	store.SetTurnAnchor("claude", "ghost", 1785510514000, 7)
	if _, ok := store.Get("claude", "ghost"); ok {
		t.Fatal("setTurnAnchor invented an attachment record")
	}
}

// The overlay both the rail and the Explorer render from must carry the anchor
// and the waiting flag, or the compact row has no path to either.
func TestRuntimeOverlayCarriesAnchorAndWaiting(t *testing.T) {
	fake := newFakeAppServer(t)
	queue := mustQueuedTurnStore(t)
	journal, err := newSendAttemptStore(queue.Database(), queue.Path(), "anchor", testPromptStore(t, queue.Database()))
	if err != nil {
		t.Fatal(err)
	}
	store := mustBindTransactionalTurnStore(t, queue, journal)
	sess := resumeCodexFixture(t, sessions.Config{ID: "sess-anchor", Provider: "codex", HostedThreadID: "anchor-hosted", Projection: newLiveProjection("codex"), Turns: store, QueuedTurns: queue, SendAttempts: journal}, "t1", fake)
	fake.EmitThreadNotification("t1", map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "t1", "turn": map[string]any{"id": "turn-1", "status": "inProgress", "startedAt": int64(1785510514)}}})
	fake.EmitThreadNotification("t1", map[string]any{"id": 1, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "t1", "turnId": "turn-1", "command": "pwd"}})
	waitReconnectCondition(t, func() bool { return sess.State().AvailabilityState().Waiting })

	s := testServerWithSessions(&server{Registry: newSessionRegistry()}, map[string]*sessions.Session{"sess-anchor": sess})
	info, ok := runtimeForTest(t, s, "codex", "t1")
	if !ok {
		t.Fatal("the hosted thread has no runtime overlay")
	}
	if !info.Attached || !info.TurnActive || info.TurnStartedAtMs != 1785510514000 || !info.Waiting {
		t.Fatalf("overlay = %+v, want attached+working+anchored+waiting", info)
	}

	row := sidebar.MemoryThreadRow(sess.View().Summary())
	if row.Runtime == nil || row.Runtime.TurnStartedAtMs != 1785510514000 || !row.Runtime.Waiting {
		t.Fatalf("memory row runtime = %+v, want the same anchor and waiting flag", row.Runtime)
	}
}

// --- claude re-attach: the anchor survives a nano restart ---------------------
//
// The shim's spool is the only evidence a restarted nano has, and it carries no
// timestamps across the RPC — the daemon's contract is frozen. So nano writes
// the anchor into its own attachment record when the turn opens and revalidates
// it against the replay. Line shapes and sequence behaviour probed live against
// the running shim on 2026-07-31.
