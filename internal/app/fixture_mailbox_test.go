package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/wire"
)

// awaitProviderIngress reads an RPC response behind the burst on the same
// connection. It proves the reader enqueued those frames while its consumer waits.
func awaitProviderIngress(t *testing.T, f *providerRuntimeFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var result json.RawMessage
	if err := f.daemon.Call(ctx, "fixture/ingress-barrier", map[string]any{}, &result); err != nil {
		t.Fatal(err)
	}
}

func awaitProviderFrame(t *testing.T, f *providerRuntimeFixture, line []byte) {
	t.Helper()
	awaitProviderFrameWithin(t, f, line, 3*time.Second)
}

func awaitProviderFrameWithin(t *testing.T, f *providerRuntimeFixture, line []byte, timeout time.Duration) {
	t.Helper()
	waitForCatalogWithin(t, timeout, func() bool {
		for _, event := range claudeWireEvents(f.session) {
			if string(event.Msg) == string(line) {
				return true
			}
		}
		return false
	})
}

func receivedProviderFrames(sess *sessions.Session) []wire.Event {
	var frames []wire.Event
	for _, event := range claudeWireEvents(sess) {
		var body struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(event.Msg, &body)
		if event.Dir == "recv" && body.Method != "" {
			frames = append(frames, event)
		}
	}
	return frames
}
