package sessions

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	fixture "github.com/dkroshow/nano/internal/testsupport"
	"github.com/dkroshow/nano/internal/wire"
)

// Boundary: ordered session ingress must remain independent of an outstanding provider call.
func TestCodexTextDeltaIngressDoesNotWaitForOutstandingProviderCall(t *testing.T) {
	t.Setenv("NANO_LIVE_TEXT_DELTAS", "on")
	providerEntered := make(chan struct{})
	providerRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseProvider := func() { releaseOnce.Do(func() { close(providerRelease) }) }
	defer releaseProvider()

	sess, _ := readyTransactionalSendTestSession(t, func(context.Context, string) (json.RawMessage, error) {
		close(providerEntered)
		<-providerRelease
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})
	if err := sess.ensureCoreSession(); err != nil {
		t.Fatal(err)
	}
	core := sess.coreSession()
	t.Cleanup(core.Close)

	response := make(chan int, 1)
	go func() {
		response <- postTurn(t, sess.Delivery(), context.Background(), "callback-interlock", "exercise callback interlock").Code
	}()
	select {
	case <-providerEntered:
	case <-time.After(time.Second):
		t.Fatal("provider call did not start")
	}

	callbackReturned := make(chan struct{})
	go func() {
		sess.wire.recordWireAt(wire.Recv, fixture.CodexDeltaFrames("streaming")[0], true, false, time.Now())
		close(callbackReturned)
	}()
	select {
	case <-callbackReturned:
	case <-time.After(250 * time.Millisecond):
		releaseProvider()
		<-response
		t.Fatal("root text-delta callback waited for the outstanding provider call")
	}

	releaseProvider()
	select {
	case code := <-response:
		if code != http.StatusOK {
			t.Fatalf("provider response status = %d, want %d", code, http.StatusOK)
		}
	case <-time.After(time.Second):
		t.Fatal("provider response did not progress after callback returned")
	}
}
