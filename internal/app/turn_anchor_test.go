package app

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"
	"time"

	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// anchorRecorder stands in for the attachment store's setTurnAnchor, so a test
// can see exactly what a session would have written down.
type anchorRecorder struct {
	mu      sync.Mutex
	writes  []anchorWrite
	records map[string]attachmentRecord
}

type anchorWrite struct {
	provider  string
	threadID  string
	startedAt int64
	seq       int64
}

func newAnchorRecorder() *anchorRecorder {
	return &anchorRecorder{records: map[string]attachmentRecord{}}
}

func (r *anchorRecorder) set(provider, threadID string, startedAtMs, seq int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, anchorWrite{provider, threadID, startedAtMs, seq})
	rec := r.records[threadID]
	rec.TurnStartedAtMs, rec.TurnAnchorSeq = startedAtMs, seq
	r.records[threadID] = rec
}

func (r *anchorRecorder) all() []anchorWrite {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]anchorWrite(nil), r.writes...)
}

func (r *anchorRecorder) last() (anchorWrite, bool) {
	writes := r.all()
	if len(writes) == 0 {
		return anchorWrite{}, false
	}
	return writes[len(writes)-1], true
}

func anchorTestSession(provider string) *session {
	return &session{id: "sess-anchor",
		provider:       provider,
		createdAt:      time.Now(),
		subs:           map[chan wireEvent]struct{}{},
		state:          map[string]json.RawMessage{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
}

func codexTurnStartedLine(turnID string, startedAtSeconds int64) []byte {
	line, _ := json.Marshal(map[string]any{
		"method": "turn/started",
		"params": map[string]any{
			"threadId": "t1",
			"turn":     map[string]any{"id": turnID, "status": "inProgress", "startedAt": startedAtSeconds},
		},
	})
	return line
}

func TestCodexTurnStartedAnchorConvertsSecondsToMilliseconds(t *testing.T) {
	active, turnID, startedAtMs, ok := turnActiveTransition("codex", protocol.Recv, codexTurnStartedLine("turn-1", 1785510514))
	if !ok || !active || turnID != "turn-1" {
		t.Fatalf("transition = (%v, %q, %v)", active, turnID, ok)
	}
	if startedAtMs != 1785510514000 {
		t.Fatalf("startedAtMs = %d, want 1785510514000 (seconds × 1000)", startedAtMs)
	}
}

// The residual defense (D1): a future schema that switched the field to
// milliseconds must not be multiplied a second time into the year 58000.
func TestCodexTurnStartedAnchorLeavesMillisecondsAlone(t *testing.T) {
	_, _, startedAtMs, _ := turnActiveTransition("codex", protocol.Recv, codexTurnStartedLine("turn-1", 1785510514000))
	if startedAtMs != 1785510514000 {
		t.Fatalf("startedAtMs = %d, want the value unchanged", startedAtMs)
	}
}

// A turn with no stated start yields no anchor rather than a zero that would
// render as 1970 (D2/D8).
func TestCodexTurnStartedAnchorAbsentWhenUnstated(t *testing.T) {
	line, _ := json.Marshal(map[string]any{
		"method": "turn/started",
		"params": map[string]any{"threadId": "t1", "turn": map[string]any{"id": "turn-1"}},
	})
	active, _, startedAtMs, ok := turnActiveTransition("codex", protocol.Recv, line)
	if !ok || !active {
		t.Fatal("turn/started must still open the turn without a startedAt")
	}
	if startedAtMs != 0 {
		t.Fatalf("startedAtMs = %d, want 0 (no evidence)", startedAtMs)
	}
}

func TestTurnAnchorSetOnStartAndNotMovedByAKeepAlive(t *testing.T) {
	sess := anchorTestSession("codex")
	sess.recordWire(protocol.Recv, codexTurnStartedLine("turn-1", 1785510514))
	if got := sess.turnAnchor(); got != 1785510514000 {
		t.Fatalf("anchor after start = %d, want 1785510514000", got)
	}
	sess.recordWire(protocol.Recv, codexTurnStartedLine("turn-1", 1785510999))
	if got := sess.turnAnchor(); got != 1785510514000 {
		t.Fatalf("anchor after a same-turn event = %d, want it unmoved at 1785510514000", got)
	}
}

// A NEW turn must not inherit the old anchor (the other half of D7/D10).
func TestTurnAnchorResetsForTheNextTurn(t *testing.T) {
	sess := anchorTestSession("codex")
	sess.recordWire(protocol.Recv, codexTurnStartedLine("turn-1", 1785510514))
	completed, _ := json.Marshal(map[string]any{
		"method": "turn/completed",
		"params": map[string]any{"threadId": "t1", "turn": map[string]any{"id": "turn-1", "status": "completed"}},
	})
	sess.recordWire(protocol.Recv, completed)
	sess.recordWire(protocol.Recv, codexTurnStartedLine("turn-2", 1785510999))
	if got := sess.turnAnchor(); got != 1785510999000 {
		t.Fatalf("anchor for the second turn = %d, want 1785510999000", got)
	}
}

// D14: a session that dies mid-turn must not leave a timer running. There are
// exactly three sites that can end a turn's visibility, and all three clear.
func TestTurnAnchorClearsAtEveryTerminalSite(t *testing.T) {
	cases := []struct {
		name string
		end  func(*session)
	}{
		{"turn completed", func(s *session) { s.setTurnActive(false, "", 0, true) }},
		{"session exited", func(s *session) { s.setStatus("exited") }},
		{"spawn failed", func(s *session) { s.failSpawn("boom") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := anchorTestSession("codex")
			sess.recordWire(protocol.Recv, codexTurnStartedLine("turn-1", 1785510514))
			if sess.turnAnchor() == 0 {
				t.Fatal("precondition: the turn should be anchored")
			}
			tc.end(sess)
			if got := sess.turnAnchor(); got != 0 {
				t.Fatalf("anchor after %s = %d, want 0", tc.name, got)
			}
			if sum := sess.summary(); sum.TurnStartedAtMs != 0 {
				t.Fatalf("summary still carries anchor %d", sum.TurnStartedAtMs)
			}
		})
	}
}

// Claude states nothing about when a turn began, so nano (its host) uses
// arrival time — but only for a line that is genuinely arriving. A shim replay
// re-delivers the turn's opening line long after the fact; dating the turn to
// that moment is precisely the invention D2/D8 forbid.
func TestClaudeReplayRecoversTurnStateWithoutInventingAnAnchor(t *testing.T) {
	userLine := []byte(`{"type":"user","message":{"role":"user"}}`)

	live := anchorTestSession("claude")
	live.recordWireLive(protocol.Send, userLine, true)
	if live.summary().TurnActive || live.turnAnchor() != 0 {
		t.Fatal("local send invented execution evidence")
	}
	live.recordWireLive(protocol.Recv, []byte(`{"type":"assistant","message":{"role":"assistant"}}`), true)
	if !live.summary().TurnActive || live.turnAnchor() == 0 {
		t.Fatalf("live provider output should open an anchored turn, got %+v", live.summary())
	}

	replayed := anchorTestSession("claude")
	replayed.beginTurnRestore()
	replayed.recordWireLive(protocol.Send, userLine, false)
	if replayed.summary().TurnActive {
		t.Fatal("replay published work before settlement")
	}
	replayed.settleRestoredTurnState()
	if !replayed.summary().TurnActive {
		t.Fatal("a replayed send should still recover the turn state")
	}
	if got := replayed.turnAnchor(); got != 0 {
		t.Fatalf("replayed anchor = %d, want 0 — arrival time is not evidence here", got)
	}
}

// restoreTurnAnchor is a correction, not a second source of truth: it only
// applies to a turn that is actually running, and it may only move the anchor
// backward (D9/D11), so recovery can lengthen a reported elapsed but never
// silently shorten one.
func TestRestoreTurnAnchorGuards(t *testing.T) {
	idle := anchorTestSession("claude")
	if idle.restoreTurnAnchor(1785510514000) {
		t.Fatal("an idle session must not accept an anchor")
	}

	sess := anchorTestSession("claude")
	sess.recordWireLive(protocol.Send, []byte(`{"type":"user"}`), false)
	if !sess.restoreTurnAnchor(1785510514000) {
		t.Fatal("an unanchored active turn should accept the persisted anchor")
	}
	if sess.restoreTurnAnchor(1785510999000) {
		t.Fatal("an anchor must never move forward")
	}
	if got := sess.turnAnchor(); got != 1785510514000 {
		t.Fatalf("anchor = %d, want the earlier 1785510514000", got)
	}
	if !sess.restoreTurnAnchor(1785510000000) {
		t.Fatal("an earlier anchor is a legitimate backward correction")
	}
}

// The claude anchor's whole point is surviving a nano restart, which means
// surviving a JSON round-trip through attachments.json. A record written by an
// older nano has neither field and must degrade to "no anchor" — never to a
// zero that reads as 1970.
func TestAttachmentRecordRoundTripsTurnAnchor(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/attachments.json"
	store, err := newAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store.remember(attachmentRecord{Provider: "claude", NativeThreadID: "native-1", Cwd: dir})
	store.setTurnAnchor("claude", "native-1", 1785510514000, 42)

	reloaded, err := newAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := reloaded.get("claude", "native-1")
	if !ok {
		t.Fatal("record did not survive the reload")
	}
	if rec.TurnStartedAtMs != 1785510514000 || rec.TurnAnchorSeq != 42 {
		t.Fatalf("anchor round-trip = (%d, %d), want (1785510514000, 42)", rec.TurnStartedAtMs, rec.TurnAnchorSeq)
	}

	store.setTurnAnchor("claude", "native-1", 0, 0)
	cleared, err := newAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := cleared.get("claude", "native-1"); rec.TurnStartedAtMs != 0 || rec.TurnAnchorSeq != 0 {
		t.Fatalf("anchor after clear = (%d, %d), want zeroes", rec.TurnStartedAtMs, rec.TurnAnchorSeq)
	}
}

// An anchor is only meaningful for a session that outlives nano, and only a
// durable transport gets a record. Writing one for a thread with no record
// would offer a recovery that cannot happen.
func TestSetTurnAnchorNeverCreatesARecord(t *testing.T) {
	store, err := newAttachmentStore(t.TempDir() + "/attachments.json")
	if err != nil {
		t.Fatal(err)
	}
	store.setTurnAnchor("claude", "ghost", 1785510514000, 7)
	if _, ok := store.get("claude", "ghost"); ok {
		t.Fatal("setTurnAnchor invented an attachment record")
	}
}

// The codex half of surviving a restart mid-turn: thread/resume's turns array
// is the only place the in-flight turn is named before state is compacted.
// Shape probed live 2026-07-31.
func TestCodexInFlightTurnFromResumeResponse(t *testing.T) {
	resume := []byte(`{"thread":{"id":"th-1","turns":[
		{"id":"turn-0","status":"completed","startedAt":1785510000,"completedAt":1785510100},
		{"id":"turn-1","status":"inProgress","startedAt":1785510514,"completedAt":null,"durationMs":null}
	]}}`)
	turn, ok := decodedCodexResume(resume).inFlightTurn()
	if !ok {
		t.Fatal("the in-flight turn was not recognised")
	}
	if turn.ID != "turn-1" || turn.startedAtMs() != 1785510514000 {
		t.Fatalf("turn = %+v (anchor %d), want turn-1 anchored at 1785510514000", turn, turn.startedAtMs())
	}
}

// A thread whose newest turn finished while nano was down comes back idle. The
// alternative — treating any trailing turn as live — would leave a permanently
// working row with a timer that never stops.
func TestCodexInFlightTurnIgnoresFinishedAndMissingTurns(t *testing.T) {
	cases := map[string][]byte{
		"newest turn has no id":   []byte(`{"thread":{"turns":[{"id":"old","status":"completed"},{"status":"inProgress"}]}}`),
		"newest turn completed":   []byte(`{"thread":{"turns":[{"id":"t","status":"completed","startedAt":1785510000}]}}`),
		"newest turn interrupted": []byte(`{"thread":{"turns":[{"id":"t","status":"interrupted","startedAt":1785510000}]}}`),
		"no turns at all":         []byte(`{"thread":{"turns":[]}}`),
		"turns already stripped":  []byte(`{"thread":{"id":"th-1"}}`),
		"unparseable":             []byte(`not json`),
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			if turn, ok := decodedCodexResume(resp).inFlightTurn(); ok {
				t.Fatalf("reported %+v as in flight", turn)
			}
		})
	}
}

// A turn parked on a dialog is not working (D15). Both providers leave
// turnActive true while a request is parked, so `waiting` is the field that
// keeps the rail from ticking an elapsed at a thread that is standing still.
func TestWaitingReflectsParkedInteractions(t *testing.T) {
	sess := anchorTestSession("codex")
	sess.recordWire(protocol.Recv, codexTurnStartedLine("turn-1", 1785510514))
	if sum := sess.summary(); sum.Waiting {
		t.Fatal("a working session is not waiting")
	}
	sess.addPendingInteraction(pendingInteraction{ID: "1", Method: methodCommandApproval, Kind: kindCommandApproval})
	sum := sess.summary()
	if !sum.Waiting {
		t.Fatal("a parked interaction must read as waiting")
	}
	if !sum.TurnActive || sum.TurnStartedAtMs == 0 {
		t.Fatalf("waiting is display state layered on the live turn, got %+v", sum)
	}
	if _, ok := sess.claimPendingInteraction("1"); !ok {
		t.Fatal("the interaction should still be claimable")
	}
	if sess.summary().Waiting {
		t.Fatal("claiming the last interaction ends the wait")
	}
}

// Parking, claiming and draining are the three ways `waiting` changes, and each
// one has to reach the catalog stream: the rail subscribes to the runtime
// nudge, not to the session-scoped feed events an open pane uses. The counter
// is reset after setup so each case measures only its own transition.
func TestInteractionTransitionsNudgeTheRuntimeStream(t *testing.T) {
	park := func(s *session) {
		s.addPendingInteraction(pendingInteraction{ID: "1", Method: methodCommandApproval, Kind: kindCommandApproval})
	}
	cases := []struct {
		name  string
		setup func(*session)
		act   func(*session)
	}{
		{"park", func(*session) {}, park},
		{"claim", park, func(s *session) { s.claimPendingInteraction("1") }},
		{"drain", park, func(s *session) { s.drainPendingInteractions() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := anchorTestSession("codex")
			var mu sync.Mutex
			var nudges int
			sess.runtimeNotify = func() {
				mu.Lock()
				nudges++
				mu.Unlock()
			}
			tc.setup(sess)
			mu.Lock()
			nudges = 0
			mu.Unlock()
			tc.act(sess)
			mu.Lock()
			defer mu.Unlock()
			if nudges == 0 {
				t.Fatalf("%s did not nudge the runtime stream", tc.name)
			}
		})
	}
}

// The overlay both the rail and the Explorer render from must carry the anchor
// and the waiting flag, or the compact row has no path to either.
func TestRuntimeOverlayCarriesAnchorAndWaiting(t *testing.T) {
	sess := anchorTestSession("codex")
	sess.threadID = "t1"
	sess.recordWire(protocol.Recv, codexTurnStartedLine("turn-1", 1785510514))
	sess.addPendingInteraction(pendingInteraction{ID: "1", Method: methodCommandApproval, Kind: kindCommandApproval})

	s := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{"sess-anchor": sess}}}
	info, ok := s.RuntimeFor("codex", "t1")
	if !ok {
		t.Fatal("the hosted thread has no runtime overlay")
	}
	if !info.Attached || !info.TurnActive || info.TurnStartedAtMs != 1785510514000 || !info.Waiting {
		t.Fatalf("overlay = %+v, want attached+working+anchored+waiting", info)
	}

	row := memoryThreadRow(sess.summary())
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

const claudeReplayUserLine = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`
const claudeReplayResultLine = `{"type":"result","subtype":"success","session_id":"native-1"}`

// replayingShim scripts a re-attach: the given lines are replayed in order,
// then replayDone, then the connection answers everything else.
func replayingShim(t *testing.T, lines []struct {
	seq  int64
	dir  string
	line string
}) *fakeShim {
	t.Helper()
	return newFakeShim(t, func(c *shimConn) {
		attach, ok := c.next()
		if !ok {
			return
		}
		last := int64(0)
		if len(lines) > 0 {
			last = lines[len(lines)-1].seq
		}
		c.reply(attach, map[string]any{
			"shimSessionId": "cs_shim1", "nativeSessionId": "native-1",
			"state": "running", "lastSeq": last,
		})
		for _, l := range lines {
			c.line(l.seq, l.dir, l.line)
		}
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "cs_shim1", "lastSeq": last})
		c.answerRest()
	})
}

func attachWithStoredAnchor(t *testing.T, shim *fakeShim, stored attachmentRecord) (*session, *claudeSession, *anchorRecorder) {
	t.Helper()
	sess, _ := claudeTestSession(t)
	anchors := newAnchorRecorder()
	sess.updateTurnAnchor = anchors.set
	claude, err := attachClaudeSession(sess, shim.wsURL(), "native-1", claudeReplayPlan{}, claudeSpawnPosture{}, stored)
	if err != nil {
		t.Fatalf("attachClaudeSession: %v", err)
	}
	t.Cleanup(func() { claude.Close() })
	return sess, claude, anchors
}

// The live case: the spool ends with the turn's opening user line and no
// result, so the turn is genuinely still running and the persisted anchor is
// the real start — minutes before this nano process existed.
func TestClaudeReattachRestoresPersistedAnchorForALiveTurn(t *testing.T) {
	shim := replayingShim(t, []struct {
		seq  int64
		dir  string
		line string
	}{
		{1, "send", claudeReplayUserLine},
		{2, "recv", claudeInitLine},
	})
	sess, _, _ := attachWithStoredAnchor(t, shim, attachmentRecord{
		Provider: "claude", NativeThreadID: "native-1",
		TurnStartedAtMs: 1785510514000, TurnAnchorSeq: 0,
	})

	sum := sess.summary()
	if !sum.TurnActive {
		t.Fatal("the replayed turn should still read as in flight")
	}
	if sum.TurnStartedAtMs != 1785510514000 {
		t.Fatalf("anchor = %d, want the persisted 1785510514000 (never re-dated to the replay)", sum.TurnStartedAtMs)
	}
}

// The unattended-finish case: the turn completed while nano was down, so the
// record describes a turn that is over. Restoring it would leave a finished
// thread ticking forever.
func TestClaudeReattachDiscardsAnchorWhenTheTurnFinishedUnattended(t *testing.T) {
	shim := replayingShim(t, []struct {
		seq  int64
		dir  string
		line string
	}{
		{1, "send", claudeReplayUserLine},
		{2, "recv", claudeInitLine},
		{3, "recv", claudeReplayResultLine},
	})
	sess, _, anchors := attachWithStoredAnchor(t, shim, attachmentRecord{
		Provider: "claude", NativeThreadID: "native-1",
		TurnStartedAtMs: 1785510514000, TurnAnchorSeq: 0,
	})

	sum := sess.summary()
	if sum.TurnActive || sum.TurnStartedAtMs != 0 {
		t.Fatalf("summary = %+v, want an idle, unanchored session", sum)
	}
	if write, ok := anchors.last(); !ok || write.startedAt != 0 {
		t.Fatalf("stale anchor was not cleared from the record (last write %+v, wrote %v)", write, ok)
	}
}

// D9/D11: the restore needs hard identity, not a plausible-looking number. The
// record's seq was sampled before its own turn was sent, so a replayed user
// line at an EARLIER sequence belongs to a different turn and must be refused.
func TestClaudeReattachRefusesAnAnchorFromAnEarlierTurn(t *testing.T) {
	shim := replayingShim(t, []struct {
		seq  int64
		dir  string
		line string
	}{
		// Contiguous from the floor; the prompt at 1 is older than anchor 9.
		{1, "send", claudeReplayUserLine},
		{2, "recv", claudeInitLine},
	})
	sess, _, _ := attachWithStoredAnchor(t, shim, attachmentRecord{
		Provider: "claude", NativeThreadID: "native-1",
		TurnStartedAtMs: 1785510514000, TurnAnchorSeq: 9,
	})

	sum := sess.summary()
	if !sum.TurnActive {
		t.Fatal("the turn state still comes from the replay")
	}
	if sum.TurnStartedAtMs != 0 {
		t.Fatalf("anchor = %d, want none: the record names a turn the spool does not show", sum.TurnStartedAtMs)
	}
}

// A record written by a nano that predates the anchor fields degrades to no
// timer — a working dot with nothing beside it — never to the moment of replay.
func TestClaudeReattachWithoutAPersistedAnchorShowsNoTimer(t *testing.T) {
	shim := replayingShim(t, []struct {
		seq  int64
		dir  string
		line string
	}{
		{1, "send", claudeReplayUserLine},
		{2, "recv", claudeInitLine},
	})
	sess, _, _ := attachWithStoredAnchor(t, shim, attachmentRecord{
		Provider: "claude", NativeThreadID: "native-1",
	})

	sum := sess.summary()
	if !sum.TurnActive {
		t.Fatal("the replayed turn should still read as in flight")
	}
	if sum.TurnStartedAtMs != 0 {
		t.Fatalf("anchor = %d, want none", sum.TurnStartedAtMs)
	}
}

// The write side: sending a turn anchors it now AND writes that anchor down,
// with the sequence number sampled before the send. Completing the turn clears
// both, so nothing stale is left for the next restart to find.
func TestClaudeSendTurnPersistsAndClearsTheAnchor(t *testing.T) {
	shim := newFakeShim(t, func(c *shimConn) {
		start, ok := c.next()
		if !ok {
			return
		}
		c.reply(start, map[string]any{"shimSessionId": "cs_shim1"})
		c.line(1, "recv", claudeInitLine)
		c.line(2, "recv", `{"type":"system","subtype":"status"}`)
		c.answerRest()
	})

	sess, _ := claudeTestSession(t)
	anchors := newAnchorRecorder()
	sess.updateTurnAnchor = anchors.set
	claude, err := startClaudeSession(sess, claudeShimSpawnRequest(sess, shim.wsURL()).providerRequest)
	if err != nil {
		t.Fatalf("startClaudeSession: %v", err)
	}
	defer claude.Close()
	waitForClaude(t, "the init line and the shim seq watermark", func() bool {
		return sess.currentThreadID() == "native-1" && claude.tr.lastSeq() == 2
	})

	if err := claude.SendTurn("hello"); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	// The send samples the correlation sequence; live execution dates the turn.
	claude.deliverLine(claude.tr, protocol.Recv, []byte(`{"type":"assistant","message":{"role":"assistant","content":[]}}`), 3, time.Time{})
	write, ok := anchors.last()
	if !ok || write.startedAt == 0 {
		t.Fatalf("the opening turn was not persisted (last %+v, wrote %v)", write, ok)
	}
	if write.threadID != "native-1" || write.seq != 2 {
		t.Fatalf("persisted %+v, want thread native-1 at the pre-send seq 2", write)
	}

	claude.deliverLine(claude.tr, protocol.Recv, []byte(claudeReplayResultLine), 4, time.Time{})
	if got := sess.turnAnchor(); got != 0 {
		t.Fatalf("anchor after the result line = %d, want 0", got)
	}
	if write, _ := anchors.last(); write.startedAt != 0 || write.seq != 0 {
		t.Fatalf("record after the result line = %+v, want the anchor cleared", write)
	}
}

// The ordering that actually broke this in live verification: claude CLI
// 2.1.220 announces no session id until it has been SENT a turn, so the first
// turn of every hosted claude session opens before its native id exists. The
// shim session id stands in until then (adoptDurableIdentity): the anchor
// reaches the provisional record on live execution evidence — so a restart during the very
// first turn can restore the timer — and is rewritten under the real id when
// the init line retires the provisional identity (rekeyTurnAnchor).
func TestClaudeFirstTurnAnchorIsPersistedOnceTheInitLineLands(t *testing.T) {
	inputSeen := make(chan struct{})
	// The child holds its init line until the test has asserted the no-id
	// window — without the gate, the assertion races the init processing and
	// fails spuriously under full-suite load.
	releaseInit := make(chan struct{})
	shim := newFakeShim(t, func(c *shimConn) {
		start, ok := c.next()
		if !ok {
			return
		}
		c.reply(start, map[string]any{"shimSessionId": "cs_shim1"})
		// Nothing from the child until it has been sent a turn — no init, no id.
		// The control channel's initialize (itself a session/input line) can
		// arrive first; answer it and keep reading, or the fake holds the gate
		// before it ever sees the turn.
		input, ok := c.next()
		for ok && (input.Method != shimMethodInput || bytes.Contains(input.Params, []byte(claudeControlRequestType))) {
			c.reply(input, nil)
			input, ok = c.next()
		}
		if !ok {
			return
		}
		c.reply(input, nil)
		c.line(1, "recv", `{"type":"assistant","message":{"role":"assistant","content":[]}}`)
		close(inputSeen)
		<-releaseInit
		c.line(2, "recv", claudeInitLine)
		c.answerRest()
	})

	sess, _ := claudeTestSession(t)
	anchors := newAnchorRecorder()
	sess.updateTurnAnchor = anchors.set
	claude, err := startClaudeSession(sess, claudeShimSpawnRequest(sess, shim.wsURL()).providerRequest)
	if err != nil {
		t.Fatalf("startClaudeSession: %v", err)
	}
	defer claude.Close()

	if err := claude.SendTurn("hello"); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	<-inputSeen
	waitForClaude(t, "provider output anchored under provisional identity", func() bool {
		write, ok := anchors.last()
		return ok && write.threadID == "cs_shim1" && write.startedAt > 0
	})
	close(releaseInit)

	waitForClaudeThreadID(t, sess, "native-1")
	waitForClaude(t, "the first turn's anchor to reach the record", func() bool {
		write, ok := anchors.last()
		return ok && write.threadID == "native-1" && write.startedAt == sess.turnAnchor()
	})
}

// The New Window dialog's session name is written exactly once, at the first
// bind site that actually has a native id — an empty id must not consume it
// (claude announces no id until its first turn), and a second bind (claude
// resume: spawn-time id, then init) must not write twice.
func TestApplyPendingNameWritesOnceAtTheFirstRealBind(t *testing.T) {
	var writes []string
	sess := &session{provider: "claude",
		pendingName:    "overnight run",
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.nameThread = func(provider, nativeID, name string) {
		writes = append(writes, provider+"/"+nativeID+"="+name)
	}

	sess.applyPendingName("") // no id yet: nothing written, name kept
	if len(writes) != 0 {
		t.Fatalf("wrote a name with no native id: %v", writes)
	}
	sess.applyPendingName("sess-native-1")
	sess.applyPendingName("sess-native-1") // second bind site: no double write
	if len(writes) != 1 || writes[0] != "claude/sess-native-1=overnight run" {
		t.Fatalf("expected exactly one name write, got %v", writes)
	}
}
