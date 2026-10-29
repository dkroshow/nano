package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	testfixture "github.com/dkroshow/nano/internal/testsupport"
)

func TestGrokLoadHandshakeSettlesLifecycleAndQueue(t *testing.T) {
	for _, tc := range []struct {
		name, mode, identity string
		active               bool
	}{
		{"new", "start", "captured", false},
		{"load-idle", "resume", "captured", false},
		{"load-working", "resume", "captured", true},
		{"load-no-id-idle", "resume", "absent", false},
		{"load-no-id-working", "resume", "absent", true},
		{"load-detail-id", "resume", "detail", false},
		{"load-wrong-id", "resume", "wrong", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "grok-restart")
			if err != nil {
				t.Fatal(err)
			}
			sess := readySendTestSession(t, attempts, func(context.Context, string) (json.RawMessage, error) {
				sends.Add(1)
				return json.RawMessage(`{"turn":{"id":"queued-provider-turn"}}`), nil
			})
			sess.provider = "grok"
			sess.stateOwner.state = map[string]json.RawMessage{}
			sess.stateOwner.lifecycleState = testLifecycleSnapshot("starting", false)
			sess.stateOwner.turnID, sess.stateOwner.turnStartedAtMs = "old-anchor", 123
			t.Cleanup(sess.Close)
			if rec := postTurn(t, sess.Delivery(), context.Background(), "after-restart", "queued while starting"); rec.Code != http.StatusAccepted {
				t.Fatalf("queue status %d: %s", rec.Code, rec.Body.String())
			}

			fixture := "session-load-1.0.5.json"
			if tc.mode == "start" {
				fixture = "session-new-1.0.5.json"
			}
			raw, err := testfixture.GrokLifecycleFixture(fixture)
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			meta := result["_meta"].(map[string]any)
			switch tc.identity {
			case "absent", "detail":
				delete(result, "sessionId")
				delete(meta, "sessionId")
				if tc.identity == "absent" {
					delete(meta, "x.ai/sessionDetail")
				}
			case "wrong":
				meta["sessionId"] = "wrong-native"
			}
			if tc.active {
				meta["x.ai/runningPromptId"] = "surviving-turn"
			}
			response, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "result": result})
			if err != nil {
				t.Fatal(err)
			}
			// A disposable ACP peer uses the observed session/load shape. Run the
			// real handshake so missing ordered-ingress coordinates also fail here.
			bin := filepath.Join(t.TempDir(), "grok-fixture")
			script := fmt.Sprintf(`#!/bin/sh
IFS= read -r request || exit 1
printf '%%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
IFS= read -r request || exit 1
printf '%%s\n' '%s'
while IFS= read -r request; do :; done
`, response)
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			requestedID := sess.stateOwner.ThreadID()
			if tc.mode == "start" {
				requestedID = ""
			}
			err = runGrokHandshake(sess, ProviderRequest{Mode: tc.mode, ThreadID: requestedID, Cwd: t.TempDir(), GrokBin: bin})
			if tc.identity == "wrong" {
				if err == nil || !strings.Contains(err.Error(), "sessionId") || sends.Load() != 0 || sess.Summary().Status == "ready" {
					t.Fatalf("mismatched native identity accepted: err=%v, sends=%d", err, sends.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := sess.Summary().Status; got != "ready" {
				t.Fatalf("settled session/load left connection %q", got)
			}
			if tc.active {
				if sum := sess.Summary(); !sum.TurnActive || sum.TurnStartedAtMs != 0 || sess.stateOwner.turnID != "surviving-turn" {
					t.Fatalf("provider work was lost or inherited an old anchor: %+v, turn %q", sum, sess.stateOwner.turnID)
				}
				sess.delivery.drainQueuedTurn()
				if row, ok := attempts.Get("after-restart"); !ok || row.Status != sendAttemptQueued || sends.Load() != 0 {
					t.Fatalf("running provider drained queued work: row=%+v, sends=%d", row, sends.Load())
				}
				return
			}
			waitTurnSendDone(t, sess, "after-restart")
			if row, ok := attempts.Get("after-restart"); !ok || row.Status != sendAttemptAccepted || sends.Load() != 1 {
				t.Fatalf("idle provider did not drain exactly once: row=%+v, sends=%d", row, sends.Load())
			}
		})
	}
}
