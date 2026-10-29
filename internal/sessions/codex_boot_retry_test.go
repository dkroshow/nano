package sessions

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestBootReattachRetriesCodexResumeRefusedByAClosedListener(t *testing.T) {
	closedListener := "failed to enqueue running thread resume for thread %s: thread listener command channel is closed"
	// Seen at a 2026-10-09 boot: the app server still closed the thread.
	closing := "thread %s is closing; retry thread/resume after the thread is closed"
	for _, tc := range []struct {
		name     string
		refusal  string
		refusals int
	}{
		{"clears on retry", closedListener, 1},
		{"outlasts the retries", closedListener, 1 + 6},
		{"closing thread clears on retry", closing, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newFakeAppServer(t)
			daemon.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "after-refusal"}})
			connection := daemon.daemon()
			connection.reconnectBackoff = time.Millisecond
			sess := reconnectTestSession(t, connection, "native-boot-retry")
			sess.stateOwner.lifecycleState = testLifecycleSnapshot("starting", false)
			refusals := make([]string, tc.refusals)
			for i := range refusals {
				refusals[i] = fmt.Sprintf(tc.refusal, sess.stateOwner.threadID)
			}
			daemon.SetResumeErrorSequence(refusals...)

			if err := startCodexSession(sess.Runtime(), connection, ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: sess.stateOwner.threadID, Cwd: t.TempDir(), BootReattach: NewBootControl(0)}); err != nil {
				t.Fatal(err)
			}
			if tc.refusals > 1 {
				waitForClaude(t, "the bounded reconnect attempts to park", func() bool { return sess.view.Summary().Status == "reconnect_parked" })
				for repeat := 0; repeat < 2; repeat++ {
					if response := postTurn(t, sess.Delivery(), context.Background(), "after-parking", "prompt to a parked thread"); response.Code != http.StatusAccepted && response.Code != http.StatusOK {
						t.Fatalf("parked prompt: %d %s", response.Code, response.Body.String())
					}
				}
				waitTurnSendDone(t, sess, "after-parking")
			}
			waitForClaude(t, "the resumed session to become ready", func() bool { return sess.view.Summary().Status == "ready" })
			if got := daemon.ResumeCount(); got != tc.refusals+1 {
				t.Fatalf("thread/resume calls = %d, want %d refused and one that succeeds", got, tc.refusals)
			}
			daemon.Mu.Lock()
			starts := daemon.TurnStarts
			daemon.Mu.Unlock()
			if want := map[bool]int{true: 1, false: 0}[tc.refusals > 1]; starts != want {
				t.Fatalf("turn/start calls = %d, want %d: the parked prompt is delivered once", starts, want)
			}
		})
	}
}
