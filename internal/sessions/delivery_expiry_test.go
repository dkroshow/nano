package sessions

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// A stalled provider held every send past Nano's delivery deadline and no pane
// said so. Each expiry counts in its provider's runtime status, the stream a
// pane reads, and ages out of it with no further send.
func TestDeliveryDeadlineExpiryShowsInRuntimeStatusUntilItAgesOut(t *testing.T) {
	store, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	sess := readySendTestSession(t, store, func(context.Context, string) (json.RawMessage, error) {
		return nil, context.DeadlineExceeded // the provider did not answer in time
	})
	sess.deliveryExpiries = NewDeliveryExpiries(time.Second, func(string) { sess.PublishProjectionRuntime() })
	shown := func() int { return sess.projection.Status().Runtime.DeliveryExpiries }

	if rec := postTurn(t, sess, context.Background(), "slow", "slow prompt"); rec.Code != http.StatusAccepted {
		t.Fatalf("send = %d %s; want 202", rec.Code, rec.Body.String())
	}
	waitForClaude(t, "delivery expiry state", func() bool { return shown() == 1 })
	waitForClaude(t, "delivery expiry state", func() bool { return shown() == 0 })
}
