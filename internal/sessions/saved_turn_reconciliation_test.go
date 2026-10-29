package sessions

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/wire"
)

func durableLiveWorkTestSession(t *testing.T, turnID string, startedAtMs int64, drop ...bool) (*Session, *LiveProjection) {
	p := NewLiveProjection("codex", "", len(drop) > 0 && drop[0])
	f := newProviderRuntimeFixture(t, Config{ID: "session-1", Provider: "codex", HostedThreadID: "hosted", Projection: p}, "native-thread", "", func(f *providerRuntimeFixture) {
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
	checkSavedAttachmentReplacement(t)
	started := time.Now().Add(-time.Minute)
	saved := liveview.DurableTurnEvidence{Turn: liveview.Turn{ID: "turn-1", Status: "completed", CompletedAtMs: started.Add(time.Second).UnixMilli(), DurationMS: 1000}}
	views := map[*Session]*LiveProjection{}
	work := func(sess *Session, at time.Time) {
		views[sess].applyWire(wire.Recv, []byte(`{"method":"item/completed","params":{"threadId":"native-thread","turnId":"turn-1","item":{"id":"cmd-`+fmt.Sprint(at.UnixNano())+`","type":"commandExecution","command":"ls","status":"completed"}}}`), at)
	}
	for _, tc := range []struct {
		name    string
		before  func(*Session)
		during  func(*Session)
		working bool
	}{
		{"live work after the saved completion", func(sess *Session) { work(sess, started.Add(2*time.Second)) }, nil, true},
		{"same-turn live work during the read", nil, func(sess *Session) { work(sess, started.Add(time.Second/2)) }, true},
		// Hiding Codex text deltas hides text, not the work they are.
		{"same-turn hidden text delta during the read", nil, func(sess *Session) {
			views[sess].applyWire(wire.Recv, []byte(`{"method":"item/agentMessage/delta","params":{"threadId":"native-thread","turnId":"turn-1","itemId":"answer-1","delta":"still working"}}`), started.Add(time.Second/2))
		}, true},
		{"replayed old output during the read", nil, func(sess *Session) {
			views[sess].applyWireLive(wire.Recv, []byte(`{"method":"item/completed","params":{"threadId":"native-thread","turnId":"turn-1","item":{"id":"answer-1","type":"agentMessage","phase":"final_answer","text":"done"}}}`), time.Now(), false, true)
		}, false},
		{"nothing since the saved completion", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, p := durableLiveWorkTestSession(t, "turn-1", started.UnixMilli(), tc.name == "same-turn hidden text delta during the read")
			views[sess] = p
			if tc.before != nil {
				tc.before(sess)
			}
			sess.ConfigureSavedReaders(func(_ context.Context, turnID string) (liveview.DurableTurnEvidence, bool) {
				if tc.during != nil {
					tc.during(sess)
				}
				return saved, turnID == saved.Turn.ID
			}, nil, nil, nil, nil)
			sess.view.ReconcileDurableTurn(context.Background())
			working := sess.view.Summary().TurnActive
			if working != tc.working {
				t.Fatalf("working after the saved completion = %v, want %v", working, tc.working)
			}
		})
	}
}
