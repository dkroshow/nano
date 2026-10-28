package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// Claude's shim turn id and Grok's prompt id must travel through the
// same durable journal binding as Codex's turn id. The old non-Codex path only
// marked accepted, so a restart lost the only exact ownership alias.
func TestNonCodexDeliveryPersistsExactProviderTurnBinding(t *testing.T) {
	for _, tc := range []struct {
		provider string
		turnID   string
	}{
		{provider: "claude", turnID: "claude:7"},
		{provider: "grok", turnID: "prompt-7"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "attempts.json"), "first")
			if err != nil {
				t.Fatal(err)
			}
			sess := readySendTestSession(t, attempts, func(context.Context, string) (json.RawMessage, error) {
				return json.RawMessage(`{"turn":{"id":"` + tc.turnID + `"}}`), nil
			})
			sess.provider = tc.provider
			sess.projection = newLiveProjection(tc.provider)
			if rec := postTurn(t, sess, context.Background(), "action-1", "provider-specific binding"); rec.Code != 200 {
				t.Fatalf("send = %d %s", rec.Code, rec.Body.String())
			}
			waitTurnSendDone(t, sess, "action-1")
			journal, ok := attempts.Get("action-1")
			if !ok || journal.ProviderTurnID != tc.turnID || journal.Status != sendAttemptAccepted {
				t.Fatalf("journal = %#v found=%v", journal, ok)
			}
		})
	}
}
