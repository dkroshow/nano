package sessions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// gatedLiveAdapter pauses the real parser at the ordered projection boundary.
// It implements the existing provider parser contract, never a session view.
type gatedLiveAdapter struct {
	next             liveAdapter
	entered, release chan struct{}
	match            func([]byte) bool
	once             sync.Once
}

func (a *gatedLiveAdapter) apply(dir protocol.Direction, line []byte, at time.Time) model.Batch {
	if a.match == nil || a.match(line) {
		a.once.Do(func() { close(a.entered); <-a.release })
	}
	return a.next.apply(dir, line, at)
}
func parsedAdapter(p *LiveProjection) liveAdapter {
	if gated, ok := p.adapter.(*gatedLiveAdapter); ok {
		return gated.next
	}
	return p.adapter
}
func holdFirstProviderFrame(t *testing.T, f *providerRuntimeFixture, p *LiveProjection, first []byte) func() {
	t.Helper()
	gate := &gatedLiveAdapter{entered: make(chan struct{}), release: make(chan struct{})}
	p.adapterMu.Lock()
	gate.next = p.adapter
	p.adapter = gate
	p.adapterMu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { close(gate.release) }) }
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
	awaitProviderFrameWithin(t, f, line, 3*time.Second)
}

func awaitProviderFrameWithin(t *testing.T, f *providerRuntimeFixture, line []byte, timeout time.Duration) {
	t.Helper()
	waitForCatalogWithin(t, timeout, func() bool {
		for _, event := range liveWirePageEvents(f.session) {
			if string(event.Msg) == string(line) {
				return true
			}
		}
		return false
	})
}

func receivedProviderFrames(sess *session) []wireEvent {
	var frames []wireEvent
	for _, event := range liveWirePageEvents(sess) {
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

func liveWirePageEvents(sess *session) []wireEvent {
	recorder := httptest.NewRecorder()
	sess.HandleEventsPage(recorder, httptest.NewRequest(http.MethodGet, "/events?beforeSeq=9223372036854775807&limit=2000", nil))
	var page struct {
		Events []wireEvent `json:"events"`
	}
	if recorder.Code != http.StatusOK {
		panic(recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		panic(err)
	}
	return page.Events
}
