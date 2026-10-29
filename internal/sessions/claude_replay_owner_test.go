package sessions

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	coresession "github.com/dkroshow/nano/internal/core/session"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	fixture "github.com/dkroshow/nano/internal/testsupport/claudeshim"
)

// replayOwnerHarness retains the real prompt/completed stores while recording
// projection and execution writes at the session's consumer boundaries.
type replayOwnerHarness struct {
	base, current *Session
	turns         *coreturns.Store
	dir           string
	shim          *fixture.SpoolShim
	projection    func() *LiveProjection
	executions    Executions
}

func newReplayOwnerHarness(t *testing.T) *replayOwnerHarness {
	t.Helper()
	base, _ := claudeTestSession(t)
	turns, err := coreturns.OpenStore(context.Background(), base.queuedTurns.Database())
	if err != nil {
		t.Fatal(err)
	}
	h := &replayOwnerHarness{base: base, turns: turns, dir: t.TempDir(), shim: fixture.NewSpoolShim(t, nil)}
	h.projection = func() *LiveProjection { return newDeliveryTestProjection("claude") }
	return h
}
func (h *replayOwnerHarness) resume(t *testing.T) error {
	t.Helper()
	if h.current != nil {
		if err := claudeDrainSettled(h.current); err != nil {
			return err
		}
	}
	b := h.base
	sess := New(Config{ID: "replay-owner", Provider: "claude", HostedThreadID: b.HostedThreadID(), CWD: b.CWD(), Projection: h.projection(), Metadata: b.metadata, Turns: b.turns, QueuedTurns: b.queuedTurns, SendAttempts: b.sendAttempts, CompletedTurns: func(context.Context) (*coreturns.Store, error) { return h.turns, nil }, AgentExecutions: h.executions, RememberAttachment: b.rememberAttachment, ForgetAttachment: b.forgetAttachment})
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	h.current = sess
	t.Cleanup(func() {
		if sess.core != nil {
			sess.core.Close()
		}
	})
	c, err := resumeClaudeSession(sess, ProviderRequest{Provider: "claude", Mode: "resume", ThreadID: "native-1", Cwd: b.CWD(), ClaudeBin: "/fake/claude", ClaudeShimWS: h.shim.WSURL(), ClaudeShimSpools: h.dir, ClaudeAnchor: hostedstore.Attachment{Provider: "claude", NativeThreadID: "native-1", HostedThreadID: b.HostedThreadID(), ShimSessionID: "shim-1"}})
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return err
}
func (h *replayOwnerHarness) seed(t *testing.T, action string, backlog int) (string, int64) {
	t.Helper()
	b := &fixture.SpoolBuilder{Key: h.base.HostedThreadID(), EventID: SubmissionEventID}
	b.Add("recv", "", claudeInitLine)
	turn := b.Prompt(action)
	start := b.Message(turn, "final", "answer")
	b.Result(turn)
	b.Background(backlog)
	h.shim.Frames = b.Frames
	fixture.WriteSpool(t, h.dir, b.Key, b.Frames...)
	seedRecoveryPrompt(t, h.base.turns, b.Key, "native-1", action, turn, "prompt")
	h.projection = func() *LiveProjection {
		return newResultProjection("claude", map[string]string{turn: "msg_final:0"})
	}
	return turn, start
}
func (h *replayOwnerHarness) count(t *testing.T, turn string) int {
	t.Helper()
	var n int
	if err := h.base.queuedTurns.Database().QueryRow(`SELECT count(*) FROM completed_turns WHERE target_hosted_thread_id=? AND turn_id=? AND outcome<>''`, h.base.HostedThreadID(), turn).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A stalled session mailbox cannot let an abandoned attach publish or finish
// a prompt later. Retry computes the same floor and commits exactly once.
func TestClaudeReconnectStalledReplayStopsAndRetrySucceeds(t *testing.T) {
	h := newReplayOwnerHarness(t)
	turn, start := h.seed(t, "stall-action", 200)
	previous := claudeShimReplayTimeout
	claudeShimReplayTimeout = 300 * time.Millisecond
	t.Cleanup(func() { claudeShimReplayTimeout = previous })
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	h.shim.BeforeAttach = func() {
		blocked := make(chan struct{})
		h.current.coreSession().ObserveAsync(coresession.AdapterEvent{LegacyApply: func() error { close(blocked); <-release; return nil }})
		<-blocked
	}
	err := h.resume(t)
	if !errors.Is(err, errClaudeReplayUnsettled) {
		t.Fatalf("stalled resume=%v; want unsettled replay", err)
	}
	stalled := h.current
	drained, ok := claudeDrains.Load(claudeDrainKey(stalled))
	if !ok {
		t.Fatal("stalled replay has no pending drain acknowledgement")
	}
	before := stalled.wire.seq
	unblock()
	// A mailbox barrier can finish before the drain goroutine clears the
	// retry guard. Its acknowledgement orders both publication and retry.
	select {
	case <-drained.(chan struct{}):
	case <-time.After(3 * time.Second):
		t.Fatal("abandoned replay did not acknowledge its drain")
	}
	if after := stalled.wire.seq; after != before {
		t.Fatalf("abandoned replay published %d more events after failure", after-before)
	}
	if n := h.count(t, turn); n != 0 {
		t.Fatalf("abandoned replay recorded %d completions", n)
	}
	if starts := len(h.shim.CallsOf(shimMethodStart)); starts != 0 {
		t.Fatalf("stalled replay started %d replacements", starts)
	}
	h.shim.BeforeAttach = nil
	claudeShimReplayTimeout = previous
	if err := h.resume(t); err != nil {
		t.Fatalf("retry=%v", err)
	}
	if h.shim.LastFromSeq() != start-1 {
		t.Fatalf("retry fromSeq=%d,want %d", h.shim.LastFromSeq(), start-1)
	}
	if n := h.count(t, turn); n != 1 {
		t.Fatalf("completion rows=%d,want exactly one", n)
	}
}

// The projection consumer blocks after the frame is admitted. The old frame
// must emit no terminal write after abandonment, and no retry can overtake it.
func TestClaudeReplayDrainFencesSessionRetryAndCompletion(t *testing.T) {
	h := newReplayOwnerHarness(t)
	turn, _ := h.seed(t, "admitted-action", 40)
	recorder := &resultRecorder{}
	h.executions = recorder
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	normal := h.projection
	h.projection = func() *LiveProjection {
		p := normal()
		p.adapter = &gatedLiveAdapter{next: p.adapter, entered: entered, release: release, match: func(line []byte) bool { return strings.Contains(string(line), `"type":"result"`) }}
		return p
	}
	previous := claudeShimReplayTimeout
	claudeShimReplayTimeout = 300 * time.Millisecond
	t.Cleanup(func() { claudeShimReplayTimeout = previous })
	err := h.resume(t)
	select {
	case <-entered:
	default:
		t.Fatal("result frame was never admitted")
	}
	if err == nil || !strings.Contains(err.Error(), "still draining") {
		t.Fatalf("stalled resume=%v; want unacknowledged drain", err)
	}
	stalled := h.current
	before := stalled.wire.seq
	attaches := len(h.shim.CallsOf(shimMethodAttach))
	if err := h.resume(t); err == nil || !strings.Contains(err.Error(), "still draining") {
		t.Fatalf("overlapping retry=%v", err)
	}
	if got := len(h.shim.CallsOf(shimMethodAttach)); got != attaches || len(h.shim.CallsOf(shimMethodStart)) != 0 {
		t.Fatalf("overlapping retry attached (%d->%d) or started replacements", attaches, got)
	}
	unblock()
	waitForClaude(t, "abandoned drain acknowledgement", func() bool { return claudeDrainSettled(stalled) == nil })
	if after := stalled.wire.seq; after != before {
		t.Fatalf("abandoned generation published %d events", after-before)
	}
	if n := h.count(t, turn); n != 0 {
		t.Fatalf("abandoned generation recorded %d completions", n)
	}
	if calls := recorder.snapshot(); len(calls) != 0 {
		t.Fatalf("abandoned generation advanced operation: %+v", calls)
	}
	h.projection = normal
	claudeShimReplayTimeout = previous
	if err := h.resume(t); err != nil {
		t.Fatalf("retry after drain=%v", err)
	}
	if n := h.count(t, turn); n != 1 {
		t.Fatalf("completion rows=%d,want exactly one", n)
	}
	var calls []resultTerminal
	for _, call := range recorder.snapshot() {
		if call.turn == turn {
			calls = append(calls, call)
		}
	}
	if len(calls) != 1 || calls[0].turn != turn || calls[0].item != "msg_final:0" || calls[0].status != agentTurnCompleted {
		t.Fatalf("terminal operation=%+v", calls)
	}
}

// The app sibling verifies actual saved final IDs/usage behind a backlog.
// This test retains its shorter progress window at the scheduling owner.
func TestClaudeReplayBackgroundBacklogKeepsProgressWindowAlive(t *testing.T) {
	sess, _ := claudeTestSession(t)
	if err := sess.ensureCoreSession(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.coreSession().Close)
	previous := claudeShimReplayTimeout
	claudeShimReplayTimeout = 100 * time.Millisecond
	t.Cleanup(func() { claudeShimReplayTimeout = previous })
	tr := &claudeShimTransport{}
	for i := 0; i < 10; i++ {
		sess.coreSession().ObserveAsync(coresession.AdapterEvent{LegacyApply: func() error { time.Sleep(30 * time.Millisecond); tr.applied.Add(1); return nil }})
	}
	start := time.Now()
	if err := tr.awaitApplied(sess.coreSession()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 3*claudeShimReplayTimeout {
		t.Fatalf("backlog did not outlast progress window: %s", took)
	}
}
