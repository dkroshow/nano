package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
	liveview "github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

type liveFixtureLine struct {
	Dir string          `json:"dir"`
	Msg json.RawMessage `json:"msg"`
}

// projectionStateForTest is the projection's native thread, turns, runtime and
// pending questions, as one snapshot without rows.
func projectionStateForTest(view sessionProjection) liveProjectionSnapshot {
	p := view.(*liveProjection)
	status := p.Status()
	p.mu.Lock()
	native := p.nativeThread
	p.mu.Unlock()
	return liveProjectionSnapshot{NativeThreadID: native, Turns: p.TurnList(), Runtime: status.Runtime, Interactions: status.Interactions}
}

// savedTurnTestCatalog answers the saved-turn read with one fixed turn and
// records each read.
type savedTurnTestCatalog struct {
	fakeThreadCatalog
	saved history.SavedTurnStatus
	reads []string
}

func (c *savedTurnTestCatalog) ResolveThreadReference(_ context.Context, reference string) (string, error) {
	return "nth_" + reference, nil
}

func (c *savedTurnTestCatalog) ReadSavedTurnStatus(_ context.Context, _, turnID string) (history.SavedTurnStatus, bool, error) {
	c.reads = append(c.reads, turnID)
	return c.saved, true, nil
}

// A lost terminal event settles only from an exact saved turn id. Claude saved
// turns share no id with the live turn, and a conflicted catalog identity
// proves nothing, so neither is read. Boundary: savedTurnEvidence.
func TestSavedTurnEvidenceReadsOnlyExactCodexAndGrokTurns(t *testing.T) {
	completed := time.Now().UTC()
	for _, tc := range []struct {
		provider       string
		conflict, want bool
	}{{"codex", false, true}, {"grok", false, true}, {"claude", false, false}, {"codex", true, false}} {
		catalog := &savedTurnTestCatalog{saved: history.SavedTurnStatus{
			Turn: history.TranscriptTurn{NativeTurnID: "turn-1", Status: history.TurnCompleted, CompletedAt: &completed},
		}}
		catalog.thread.Source.Conflict = tc.conflict
		sess := newFixtureSession(sessionConfig{Provider: tc.provider}, "native")
		got, ok := (&server{catalog: catalog}).savedTurnEvidence(context.Background(), sess, "turn-1")
		if ok != tc.want || (ok && (got.Turn.ID != "turn-1" || got.Turn.Status != agentTurnCompleted || got.Turn.CompletedAtMs != completed.UnixMilli())) ||
			(!tc.want && len(catalog.reads) != 0) {
			t.Fatalf("%s conflict=%v: evidence = %+v, %v; reads %v", tc.provider, tc.conflict, got, ok, catalog.reads)
		}
	}
}

// reconcileWithSavedTurn runs turn repair against one saved turn, which the
// read returns only for its own exact id.
func reconcileWithSavedTurn(sess *session, saved durableTurnEvidence) {
	sess.ConfigureSavedReaders(func(_ context.Context, turnID string) (durableTurnEvidence, bool) {
		return saved, turnID == saved.Turn.ID
	}, nil, nil, nil, nil)
	sess.ReconcileDurableTurn(context.Background())
}

func TestDurableTerminalAuthority(t *testing.T) {
	completed := durableTurnEvidence{Turn: liveProjectionTurn{ID: "turn-1", Status: "completed", CompletedAtMs: 200}}
	cases := []struct {
		name   string
		exact  bool
		newer  bool
		wireAt int64
		want   bool
	}{
		{"newer durable turn proves finality", true, true, 300, true},
		{"exact completed evidence with no newer wire", true, false, 200, true},
		{"newer live wire keeps reopenable completion active", true, false, 201, false},
		{"missing completion timestamp fails safe", true, false, 0, false},
		{"exact id mismatch fails safe", false, true, 0, false},
	}
	for _, tc := range cases {
		exact := completed
		exact.Newer = tc.newer
		if tc.name == "missing completion timestamp fails safe" {
			exact.Turn.CompletedAtMs = 0
		}
		if got := liveview.DurableTerminalAuthoritative(exact, tc.exact, tc.wireAt); got != tc.want {
			t.Fatalf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func durableTurnTestSession(t *testing.T, turnID string, startedAtMs int64) (*session, *liveProjection) {
	p := newLiveProjection("codex")
	f := newProviderRuntimeFixture(t, sessionConfig{ID: "session-1", Provider: "codex", HostedThreadID: "hosted", Projection: p}, "native-thread", "", func(f *providerRuntimeFixture) {
		f.resumeResult = map[string]any{"thread": map[string]any{"id": "native-thread", "turns": []any{map[string]any{"id": turnID, "status": "inProgress", "startedAt": startedAtMs / 1000}}}}
	})
	return f.session, p
}

// A saved completion ends the open turn only if the turn did no live work
// since it or while it was read, on the attachment and native thread that
// read it. Otherwise an old completion would mark a working thread idle and
// release the next queued prompt early (the drain sends only to an idle
// thread). Replayed old output is not new work. Boundary: reconcileDurableTurn
// with the saved read standing in for the catalog.
func TestSavedCompletionEndsOnlyATurnThatDidNoLiveWorkSinceIt(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	saved := durableTurnEvidence{Turn: liveProjectionTurn{ID: "turn-1", Status: "completed", CompletedAtMs: started.Add(time.Second).UnixMilli(), DurationMS: 1000}}
	views := map[*session]*liveProjection{}
	work := func(sess *session, at time.Time) {
		views[sess].applyWire(protocol.Recv, []byte(`{"method":"item/completed","params":{"threadId":"native-thread","turnId":"turn-1","item":{"id":"cmd-`+fmt.Sprint(at.UnixNano())+`","type":"commandExecution","command":"ls","status":"completed"}}}`), at)
	}
	for _, tc := range []struct {
		name    string
		before  func(*session)
		during  func(*session)
		working bool
	}{
		{"live work after the saved completion", func(sess *session) { work(sess, started.Add(2*time.Second)) }, nil, true},
		{"same-turn live work during the read", nil, func(sess *session) { work(sess, started.Add(time.Second/2)) }, true},
		// Hiding Codex text deltas hides text, not the work they are.
		{"same-turn hidden text delta during the read", func(sess *session) { views[sess].dropLiveTextDeltas = true }, func(sess *session) {
			views[sess].applyWire(protocol.Recv, []byte(`{"method":"item/agentMessage/delta","params":{"threadId":"native-thread","turnId":"turn-1","itemId":"answer-1","delta":"still working"}}`), started.Add(time.Second/2))
		}, true},
		{"replayed old output during the read", nil, func(sess *session) {
			views[sess].ApplyWireLive(protocol.Recv, []byte(`{"method":"item/completed","params":{"threadId":"native-thread","turnId":"turn-1","item":{"id":"answer-1","type":"agentMessage","phase":"final_answer","text":"done"}}}`), time.Now(), false, true)
		}, false},
		{"nothing since the saved completion", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, p := durableTurnTestSession(t, "turn-1", started.UnixMilli())
			views[sess] = p
			if tc.before != nil {
				tc.before(sess)
			}
			sess.ConfigureSavedReaders(func(_ context.Context, turnID string) (durableTurnEvidence, bool) {
				if tc.during != nil {
					tc.during(sess)
				}
				return saved, turnID == saved.Turn.ID
			}, nil, nil, nil, nil)
			sess.ReconcileDurableTurn(context.Background())
			working := sess.Summary().TurnActive
			if working != tc.working {
				t.Fatalf("working after the saved completion = %v, want %v", working, tc.working)
			}
		})
	}
}

// The pane marks a turn's final answer, and shows its duration, from the
// published turn's finalItemID and durationMs. That id must name the turn's
// final answer, not commentary after it. Boundary: the projection's turn
// after a completed Codex turn.
func TestCompletedTurnNamesItsFinalAnswerNotItsCommentary(t *testing.T) {
	p := newLiveProjection("codex", "hosted-final")
	at := time.Now()
	for _, line := range []string{
		`{"method":"item/completed","params":{"threadId":"native-final","turnId":"turn-1","item":{"id":"commentary-1","type":"agentMessage","phase":"commentary","text":"working"}}}`,
		`{"method":"item/completed","params":{"threadId":"native-final","turnId":"turn-1","item":{"id":"final-1","type":"agentMessage","phase":"final_answer","text":"answer"}}}`,
		`{"method":"item/completed","params":{"threadId":"native-final","turnId":"turn-1","item":{"id":"commentary-2","type":"agentMessage","phase":"commentary","text":"one more note"}}}`,
	} {
		p.applyWire(protocol.Recv, []byte(line), at)
	}
	p.UpdateCompletedTurn("turn-1", "completed", 1500, at.UnixMilli())
	if turns := p.TurnList(); len(turns) != 1 || turns[0].DurationMS != 1500 || turns[0].FinalItemID != "final-1" {
		t.Fatalf("completed turns = %#v, want final-1", turns)
	}
}

func TestCodexProjectionSelectsFinalAnswerWithinTerminalTurn(t *testing.T) {
	p := newLiveProjection("codex")
	at := time.Now()
	for _, line := range []string{
		`{"method":"item/completed","params":{"turnId":"turn-1","item":{"id":"commentary-1","type":"agentMessage","phase":"commentary","text":"working"}}}`,
		`{"method":"item/completed","params":{"turnId":"turn-1","item":{"id":"final-1","type":"agentMessage","phase":"final_answer","text":"answer"}}}`,
		`{"method":"item/completed","params":{"turnId":"turn-1","item":{"id":"late-commentary","type":"agentMessage","phase":"commentary","text":"late"}}}`,
	} {
		p.applyWire(protocol.Recv, []byte(line), at)
	}
	p.UpdateCompletedTurn("turn-1", "completed", 10, at.UnixMilli())
	p.applyWire(protocol.Recv, []byte(`{"method":"item/completed","params":{"turnId":"turn-2","item":{"id":"failed-partial","type":"agentMessage","text":"partial"}}}`), at)
	p.UpdateCompletedTurn("turn-2", "failed", 5, at.UnixMilli(), json.RawMessage(`{"message":"boom"}`))

	turns := p.TurnList()
	if len(turns) != 2 {
		t.Fatalf("turns = %#v", turns)
	}
	if turns[0].FinalItemID != "final-1" {
		t.Fatalf("completed final item = %q", turns[0].FinalItemID)
	}
	if turns[1].FinalItemID != "" || string(turns[1].Error) != `{"message":"boom"}` {
		t.Fatalf("failed turn leaked response or lost error: %#v", turns[1])
	}
}

func TestCodexResumeSeedsFiveCompleteTurnsAndActiveTurn(t *testing.T) {
	p := newLiveProjection("codex")
	at := time.Unix(1785511000, 0)
	// A notification can beat the thread/resume reply. Its newer text must win
	// when the historical baseline is inserted ahead of it.
	p.applyWire(protocol.Recv, []byte(`{"method":"item/started","params":{"turnId":"turn-8","item":{"id":"agent-8","type":"agentMessage","text":"live partial answer"}}}`), at)

	turns := make([]map[string]any, 0, 8)
	for ordinal := 1; ordinal <= 7; ordinal++ {
		turns = append(turns, map[string]any{
			"id": fmt.Sprintf("turn-%d", ordinal), "status": "completed",
			"startedAt": 1785510000 + ordinal, "completedAt": 1785510001 + ordinal,
			"items": []map[string]any{
				{"id": fmt.Sprintf("user-%d", ordinal), "type": "userMessage", "content": []map[string]any{{"type": "text", "text": fmt.Sprintf("prompt %d", ordinal)}}},
				{"id": fmt.Sprintf("agent-%d", ordinal), "type": "agentMessage", "text": fmt.Sprintf("answer %d", ordinal)},
			},
		})
	}
	turns = append(turns, map[string]any{
		"id": "turn-8", "status": "inProgress", "startedAt": 1785511000,
		"items": []map[string]any{
			{"id": "user-8", "type": "userMessage", "content": []map[string]any{{"type": "text", "text": "prompt 8"}}},
			{"id": "agent-8", "type": "agentMessage", "text": "stale partial"},
		},
	})
	response, err := json.Marshal(map[string]any{"thread": map[string]any{"id": "native-1", "turns": turns}})
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := json.Unmarshal(response, &reply); err != nil {
		t.Fatal(err)
	}
	newProviderRuntimeFixture(t, sessionConfig{ID: "seed-five", Provider: "codex", Projection: p}, "native-1", "", func(f *providerRuntimeFixture) { f.resumeResult = reply })
	turnsAfter := p.TurnList()

	if got, want := len(turnsAfter), 6; got != want {
		t.Fatalf("turns = %d, want %d: %#v", got, want, turnsAfter)
	}
	if turnsAfter[0].ID != "turn-3" || turnsAfter[5].ID != "turn-8" || turnsAfter[5].Status != "in_progress" {
		t.Fatalf("retained turns = %#v, want turn-3 through an open turn-8", turnsAfter)
	}
	if turnsAfter[0].FinalItemID != "agent-3" {
		t.Fatalf("resumed final answer = %q, want agent-3", turnsAfter[0].FinalItemID)
	}
}

func TestLiveProjectionPrunesOnlyWholeCompletedTurns(t *testing.T) {
	p := newLiveProjection("codex")
	at := time.Now()
	for ordinal := 1; ordinal <= 8; ordinal++ {
		turnID := fmt.Sprintf("turn-%d", ordinal)
		line := fmt.Sprintf(`{"method":"item/completed","params":{"turnId":%q,"item":{"id":%q,"type":"agentMessage","text":"done"}}}`, turnID, "msg-"+turnID)
		p.applyWire(protocol.Recv, []byte(line), at)
		p.UpdateCompletedTurn(turnID, "completed", 1, at.UnixMilli())
	}
	turns := p.TurnList()
	if got, want := len(turns), 5; got != want {
		t.Fatalf("turns = %d, want %d", got, want)
	}
	if turns[0].ID != "turn-4" {
		t.Fatalf("oldest retained turn = %q, want turn-4", turns[0].ID)
	}
}

func TestClaudeProjectionUsesShimTurnIDs(t *testing.T) {
	p := newLiveProjection("claude")
	at := time.Now()
	for ordinal := 1; ordinal <= 2; ordinal++ {
		p.SetClaudeProviderTurnID(fmt.Sprintf("shim-turn-%d", ordinal))
		prompt := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"prompt %d"}}`, ordinal)
		p.applyWire(protocol.Send, []byte(prompt), at)
		p.applyWire(protocol.Recv, []byte(`{"type":"result","subtype":"success"}`), at.Add(time.Second))
	}
	turns := p.TurnList()
	if len(turns) != 2 || turns[0].ID != "shim-turn-1" || turns[1].ID != "shim-turn-2" {
		t.Fatalf("turns = %#v", turns)
	}
}

func applyClaudeFinalMessageForTest(p *liveProjection, at time.Time, live, replay bool) {
	p.ApplyWireLive(protocol.Recv, []byte(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"final-1"}}}`), at, live, replay)
	p.ApplyWireLive(protocol.Recv, []byte(`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text"}}}`), at, live, replay)
	p.ApplyWireLive(protocol.Recv, []byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"text":"done"}}}`), at, live, replay)
	p.ApplyWireLive(protocol.Recv, []byte(`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`), at, live, replay)
}

func TestClaudeLiveProjectionKeepsCompletedTurnIdentity(t *testing.T) {
	p := newLiveProjection("claude")
	at := time.Date(2026, time.August, 27, 5, 44, 0, 0, time.UTC)
	p.SetClaudeProviderTurnID("shim-turn-1")

	p.ApplyWireLive(protocol.Send, []byte(`{"type":"user","message":{"role":"user","content":"inspect"}}`), at.Add(-time.Second), true, false)
	applyClaudeFinalMessageForTest(p, at.Add(-time.Millisecond), true, false)
	result := []byte(`{"type":"result","subtype":"success"}`)
	p.SetClaudeResultTurnID("shim-turn-1") // the turn the session loop finds the result names
	p.ApplyWireLive(protocol.Recv, result, at, true, false)
	p.ApplyWireLive(protocol.Recv, result, at.Add(time.Second), true, false)

	turns := projectionStateForTest(p).Turns
	if len(turns) != 1 || turns[0].ID != "shim-turn-1" || turns[0].FinalItemID == "" {
		t.Fatalf("completed Claude turn = %#v", turns)
	}
}

func TestClaudeProjectionUsesResultErrorAndAbortEvidence(t *testing.T) {
	p := newLiveProjection("claude")
	at := time.Now()
	p.SetClaudeProviderTurnID("shim-turn-1")
	p.applyWire(protocol.Send, []byte(`{"type":"user","message":{"role":"user","content":"first"}}`), at)
	p.SetClaudeResultTurnID("shim-turn-1")
	p.applyWire(protocol.Recv, []byte(`{"type":"result","is_error":true,"subtype":"error_during_execution","result":"failed","terminal_reason":"error"}`), at.Add(time.Second))
	p.SetClaudeProviderTurnID("shim-turn-2")
	p.applyWire(protocol.Send, []byte(`{"type":"user","message":{"role":"user","content":"second"}}`), at.Add(2*time.Second))
	p.SetClaudeResultTurnID("shim-turn-2")
	p.applyWire(protocol.Recv, []byte(`{"type":"result","is_error":true,"terminal_reason":"aborted_streaming"}`), at.Add(3*time.Second))

	turns := projectionStateForTest(p).Turns
	if len(turns) != 2 || turns[0].Status != agentTurnFailed || len(turns[0].Error) == 0 {
		t.Fatalf("failed Claude turn = %#v", turns)
	}
	if turns[1].Status != agentTurnInterrupted || turns[1].FinalItemID != "" {
		t.Fatalf("interrupted Claude turn = %#v", turns[1])
	}
}

func TestCompletedProviderUserMessageDoesNotSettleThePrompt(t *testing.T) {
	dir := t.TempDir()
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, store)
	seedAcceptedTransactionalTurn(t, turnStore, "hosted-thread", model.ProviderCodex, "native-thread", "action-wire-confirm", "turn-1", "ship it now")
	projection := newLiveProjection("codex", "hosted-thread")
	projection.SetNativeThreadID("native-thread")
	f := newProviderRuntimeFixture(t, sessionConfig{ID: "session-wire-confirm", Provider: "codex", HostedThreadID: "hosted-thread", Projection: projection, Turns: turnStore, QueuedTurns: queue, SendAttempts: store}, "native-thread", "")

	started := []byte(`{"method":"item/started","params":{"threadId":"native-thread","turnId":"turn-1","item":{"id":"provider-user-1","type":"userMessage","content":[{"type":"text","text":"ship it now"}]}}}`)
	completed := []byte(`{"method":"item/completed","params":{"threadId":"native-thread","turnId":"turn-1","item":{"id":"provider-user-1","type":"userMessage","content":[{"type":"text","text":"ship it now"}]}}}`)
	agent := []byte(`{"method":"item/started","params":{"threadId":"native-thread","turnId":"turn-1","item":{"id":"agent-1","type":"agentMessage","text":"Working"}}}`)

	f.emitAndWait(t, started)
	if record, _ := store.Get("action-wire-confirm"); record.Confirmed {
		t.Fatalf("item/started confirmed the send before the provider completed its user item: %#v", record)
	}
	f.emitAndWait(t, completed)
	if record, ok := store.Get("action-wire-confirm"); !ok || record.Confirmed || record.Status != sendAttemptAccepted {
		t.Fatalf("completed provider user item settled accepted prompt: %#v, found=%v", record, ok)
	}
	f.emitAndWait(t, agent)
	if record, ok := store.Get("action-wire-confirm"); !ok || record.Confirmed {
		t.Fatalf("agent activity settled the accepted prompt: %#v, found=%v", record, ok)
	}
}

func TestClaudeFable51CompletedTurnKeepsStandaloneNextIdentity(t *testing.T) {
	p := newLiveProjection("claude")
	p.SetClaudeProviderTurnID("claude:shim-fable:1")
	p.applyWire(protocol.Send, []byte(`{"type":"user","uuid":"user-1","message":{"role":"user","content":[{"type":"text","text":"Inspect"}]}}`), time.Now())
	for _, line := range []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_body"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Body"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_next"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Next: owner — continue"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"result","subtype":"success","is_error":false}`,
	} {
		if strings.Contains(line, `"type":"result"`) {
			p.SetClaudeResultTurnID("claude:shim-fable:1")
		}
		p.applyWire(protocol.Recv, []byte(line), time.Now())
	}
	turns := p.TurnList()
	if len(turns) != 1 || turns[0].ID != "claude:shim-fable:1" || turns[0].FinalItemID != "msg_next:0" {
		t.Fatalf("completed split Fable turn = %#v", turns)
	}
}

// Failure caught: a stream that falls behind silently loses an update, which
// may be the terminal turn update no later one exposes. Boundary: the
// projection's subscription, flooded past its queue.
func TestLiveProjectionClosesALaggingSubscriber(t *testing.T) {
	p := newLiveProjection("codex")
	ch, cancel := p.Subscribe()
	defer cancel()
	p.mu.Lock()
	for i := 0; i < cap(ch)+10; i++ {
		p.appendLocked(liveProjectionUpdate{Op: projectionTurnUpsert,
			Turn: &liveProjectionTurn{ID: "turn", Status: "in_progress"}})
	}
	p.mu.Unlock()
	closed := false
drain:
	for i := 0; i <= cap(ch); i++ {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
				break drain
			}
		case <-time.After(50 * time.Millisecond):
			break drain
		}
	}
	if !closed {
		t.Fatal("lagging projection subscriber remained open after its queue overflowed")
	}
	p.mu.Lock()
	subscriberCount := len(p.subs)
	p.mu.Unlock()
	if subscriberCount != 0 {
		t.Fatalf("lagging projection subscriber remained registered: %d", subscriberCount)
	}
}

func TestLiveProjectionCatalogChangeResolvesNativeSessionIdentity(t *testing.T) {
	started := time.Now().Add(-time.Minute).UnixMilli()
	sess, _ := durableTurnTestSession(t, "turn-1", started)
	sess.ConfigureSavedReaders(func(_ context.Context, turnID string) (durableTurnEvidence, bool) {
		return durableTurnEvidence{Turn: liveProjectionTurn{ID: turnID, Status: "completed", CompletedAtMs: started + 1}}, turnID == "turn-1"
	}, nil, nil, nil, nil)
	s := testServerWithSessions(&server{catalog: &fakeMessagesCatalog{}, Registry: newSessionRegistry()}, map[string]*session{sess.ID(): sess})

	// Transcript notifications use the catalog's nth_ identity, while hosted
	// sessions retain the provider-native thread identity.
	s.confirmDurableThread(context.Background(), transcriptChange{
		ThreadID: "nth_1", Provider: "codex", NativeThreadID: "native-thread",
	})

	if summary := sess.Summary(); summary.TurnActive {
		t.Fatalf("catalog notification did not reach the session's open turn: %#v", summary)
	}
}

// A small parser-recovery backlog must leave hosted control responsive and
// reconcile every attached session once, plus a change received in flight.
// The index package separately tests the 5,000-change dispatcher backlog.
func TestTranscriptChangeParserRecoveryLoadKeepsControlPromptAndIdempotent(t *testing.T) {
	const (
		attachedSessions   = 100
		changedTranscripts = attachedSessions
	)
	dir := t.TempDir()
	executions, err := newAgentExecutionStore(filepath.Join(dir, "executions.sqlite"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, attempts)
	hosted, _, err := executions.beginSession("load-target", "request-hash", "codex", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(hosted.ID, "session-99", "native-99", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}

	server := &server{
		Registry: newSessionRegistry(), agentExecutions: executions,
		sendAttempts: attempts, queuedTurns: queue, turns: turnStore,
	}
	blocked := make(chan struct{})
	started := make(chan struct{}, transcriptChangeWorkerCount)
	var durableCalls atomic.Int32
	var providerCalls atomic.Int32
	for index := 0; index < attachedSessions; index++ {
		id := fmt.Sprintf("session-%d", index)
		nativeID := fmt.Sprintf("native-%d", index)
		cfg := sessionConfig{ID: id, Provider: "codex", Projection: newLiveProjection("codex")}
		if id == "session-99" {
			cfg.HostedThreadID = hosted.ID
			cfg.PresentationMode = presentationBackground
			cfg.AgentExecutions = executions
			cfg.Turns = turnStore
			cfg.SendAttempts = attempts
			cfg.QueuedTurns = queue
		}
		f := newProviderRuntimeFixture(t, cfg, nativeID, "", func(f *providerRuntimeFixture) {
			if id != "session-99" {
				f.resumeResult = map[string]any{"thread": map[string]any{"id": nativeID, "turns": []any{map[string]any{"id": "open-turn", "status": "inProgress"}}}}
			}
			if id == "session-99" {
				f.send = func(context.Context, string) (json.RawMessage, error) {
					providerCalls.Add(1)
					return json.RawMessage(`{"turn":{"id":"provider-load-turn"}}`), nil
				}
			}
		})
		reader := func(context.Context, string) (durableTurnEvidence, bool) {
			durableCalls.Add(1)
			return durableTurnEvidence{}, false
		}
		if index < transcriptChangeWorkerCount {
			reader = func(ctx context.Context, _ string) (durableTurnEvidence, bool) {
				started <- struct{}{}
				select {
				case <-blocked:
				case <-ctx.Done():
				}
				durableCalls.Add(1)
				return durableTurnEvidence{}, false
			}
		}
		f.session.ConfigureSavedReaders(reader, nil, nil, nil, nil)
		server.RegisterSession(f.session)
	}
	target := server.LookupSession("session-99")

	nativeIDs := make([]string, changedTranscripts)
	for i := range nativeIDs {
		nativeIDs[i] = fmt.Sprintf("native-%d", i)
	}
	service, paths := queuedCatalogFixture(t, "codex", nativeIDs)
	var callbacks atomic.Int32

	service.SetTranscriptChanged(func(ctx context.Context, change transcriptChange) {
		callbacks.Add(1)
		server.confirmDurableThread(ctx, change)
	})
	for worker := 0; worker < transcriptChangeWorkerCount; worker++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("transcript workers did not start")
		}
	}
	// One update lands while the first session is in flight and must run again.
	appendCallbackFixture(t, service, paths[0])
	waitForCatalog(t, func() bool { status := service.IngestStatus(); return status.Queued == 0 && status.InFlight == 0 })
	if status, err := service.IndexStatus(context.Background()); err != nil || status.TranscriptChangeBacklog != changedTranscripts || status.TranscriptChangeInFlight != transcriptChangeWorkerCount {
		t.Fatalf("storm status = %#v, %v", status, err)
	}

	assertPrompt := func(name string, call func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		t.Helper()
		startedAt := time.Now()
		rec := call()
		if elapsed := time.Since(startedAt); elapsed > 250*time.Millisecond {
			t.Fatalf("%s took %s during transcript storm", name, elapsed)
		}
		return rec
	}
	if rec := assertPrompt("hosted thread GET", func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		applicationHandler(server).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/"+hosted.ID, nil))
		return rec
	}); rec.Code != http.StatusOK {
		t.Fatalf("hosted thread GET = %d: %s", rec.Code, rec.Body.String())
	}
	sender := confirmedTestSender(t, server, "codex")
	body, err := json.Marshal(map[string]any{
		"clientActionId": "load-require-idle", "prompt": "run once", "requireIdle": true, "senderThreadRef": sender,
	})
	if err != nil {
		t.Fatal(err)
	}
	submit := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		applicationHandler(server).ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/v1/hosted-threads/"+hosted.ID+"/turns", strings.NewReader(string(body))))
		return rec
	}
	first := assertPrompt("first requireIdle", submit)
	second := assertPrompt("idempotent requireIdle", submit)
	if first.Code != http.StatusAccepted || second.Code != http.StatusOK {
		t.Fatalf("requireIdle statuses = %d/%d: %s / %s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	var firstView, secondView agentTurnView
	if err := json.Unmarshal(first.Body.Bytes(), &firstView); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondView); err != nil {
		t.Fatal(err)
	}
	if firstView.ID == "" || secondView.ID != firstView.ID {
		t.Fatalf("idempotent operation ids = %q / %q", firstView.ID, secondView.ID)
	}
	initialOperation, ok := executions.turnForAction(hosted.ID, "load-require-idle")
	if !ok {
		t.Fatal("canonical operation is missing")
	}
	waitForTurnSendDone(t, target, initialOperation.TransportActionID)
	if operation, ok := executions.turnForAction(hosted.ID, "load-require-idle"); !ok || operation.ID != firstView.ID || operation.ProviderTurnID != "provider-load-turn" {
		t.Fatalf("canonical operation = %#v, found=%v", operation, ok)
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("provider turns = %d, want exactly one", providerCalls.Load())
	}

	close(blocked)
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := service.IndexStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		backlog, inFlight := status.TranscriptChangeBacklog, status.TranscriptChangeInFlight
		if backlog == 0 && inFlight == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("transcript recovery did not drain: backlog=%d in-flight=%d", backlog, inFlight)
		}
		time.Sleep(time.Millisecond)
	}
	if callbacks.Load() != changedTranscripts+1 {
		t.Fatalf("callbacks = %d, want %d (one in-flight replay)", callbacks.Load(), changedTranscripts+1)
	}
	// Every session reads its open turn once, the prompt target included (its
	// prompt's turn is open when its change runs); the first session runs twice.
	if durableCalls.Load() != attachedSessions+1 {
		t.Fatalf("attached session reconciliations = %d, want %d", durableCalls.Load(), attachedSessions+1)
	}
}

type savedHistoryCatalog struct {
	*fakeTranscriptCatalog
}

func (c *savedHistoryCatalog) ResolveThreadReference(_ context.Context, reference string) (string, error) {
	return "nth-owner", nil
}

func TestConversationHistoryReadsSavedRowsWithoutACut(t *testing.T) {
	nativeThreadID := "native-thread"
	p := newLiveProjection("codex", "hosted-thread")
	p.SetNativeThreadID(nativeThreadID)
	fake := &savedHistoryCatalog{fakeTranscriptCatalog: &fakeTranscriptCatalog{response: transcriptResponse{
		ThreadID: "nth-owner",
		Items:    []transcriptItem{{ID: "durable-0", OwnerItemID: "retained-0", ProjectedSeq: 1, TurnID: "turn-0"}},
		Turns:    []transcriptTurn{{ID: "turn-0", FinalItemID: "durable-0"}},
		Page:     transcriptPageInfo{StartSeq: 1, EndSeq: 1},
	}}}
	sess := newFixtureSession(sessionConfig{Provider: "codex", Projection: p}, nativeThreadID)
	recorder := httptest.NewRecorder()
	(&server{catalog: fake}).handleConversationHistory(recorder,
		httptest.NewRequest(http.MethodGet, "/api/v1/sessions/session/conversation/history", nil), sess)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response transcriptResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if fake.query.BeforeNativeTurn != "" || fake.query.BeforeTurn != 0 || fake.query.TurnLimit != 0 {
		t.Fatalf("saved history query = %#v", fake.query)
	}
	if len(response.Items) != 1 || response.Items[0].ID != "durable-0" {
		t.Fatalf("history items = %#v", response.Items)
	}
	if len(response.Turns) != 1 || response.Turns[0].ID != "turn-0" || response.Turns[0].FinalItemID != "durable-0" {
		t.Fatalf("history turns = %#v", response.Turns)
	}
	if response.Page.StartSeq != 1 || response.Page.EndSeq != 1 {
		t.Fatalf("history page = %#v", response.Page)
	}
}

// --- Pending-message confirmation seam (2026-08-04) --------------------------

func durableHousekeepingWindow(count int) []liveProjectionItem {
	rows := make([]liveProjectionItem, 0, count)
	for i := 0; i < count; i++ {
		rows = append(rows, liveProjectionItem{
			ID:            fmt.Sprintf("durable:noise-%d", i),
			DurableItemID: fmt.Sprintf("row-noise-%d", i),
			ProjectedSeq:  int64(i + 1),
			Type:          "provider_event",
			Text:          "Provider housekeeping record",
		})
	}
	return rows
}
