package app

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/protocol"
)

// gatedLiveProjection holds the real consumer before applying one wire frame.
// The provider reader stays free to enqueue the remaining burst behind it.
type gatedLiveProjection struct {
	*liveProjection
	gateMu sync.Mutex
	next   *projectionGate
}

type projectionGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *gatedLiveProjection) ApplyWireLive(dir protocol.Direction, line []byte, at time.Time, live, replay bool, seq ...int64) {
	p.gateMu.Lock()
	gate := p.next
	p.next = nil
	p.gateMu.Unlock()
	if gate != nil {
		close(gate.entered)
		<-gate.release
	}
	p.liveProjection.ApplyWireLive(dir, line, at, live, replay, seq...)
}

func holdFirstProviderFrame(t *testing.T, f *providerRuntimeFixture, p *gatedLiveProjection, first []byte) func() {
	t.Helper()
	gate := &projectionGate{entered: make(chan struct{}), release: make(chan struct{})}
	p.gateMu.Lock()
	p.next = gate
	p.gateMu.Unlock()
	release := func() { gate.once.Do(func() { close(gate.release) }) }
	t.Cleanup(release)
	f.emit(t, first)
	select {
	case <-gate.entered:
	case <-time.After(3 * time.Second):
		release()
		t.Fatal("provider frame did not reach the held projection")
	}
	return release
}

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
	waitForClaude(t, "queued provider frame", func() bool {
		for _, event := range claudeWireEvents(f.session) {
			if string(event.Msg) == string(line) {
				return true
			}
		}
		return false
	})
}

func receivedProviderFrames(sess *session) []wireEvent {
	var frames []wireEvent
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
