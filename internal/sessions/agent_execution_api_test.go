package sessions

import (
	"errors"
	"fmt"
	"testing"
	"time"

	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func TestDeliveryErrorsDistinguishRefusalFromUncertainty(t *testing.T) {
	unknown := deliveryExecutionError(errors.New("socket closed"))
	if unknown.Code != "delivery_unknown" {
		t.Fatalf("transport error code = %q", unknown.Code)
	}
	refused := deliveryExecutionError(&protocol.RPCError{Code: -32602, Message: "bad request"})
	if refused.Code != "delivery_refused" {
		t.Fatalf("provider refusal code = %q", refused.Code)
	}
}

func TestSessionSummaryCountsProviderAcceptanceAsActive(t *testing.T) {
	sess := &Session{lifecycleState: testLifecycleSnapshot("ready", false)}
	if sess.Summary().TurnActive {
		t.Fatal("local reservation asserted work")
	}
	sess.acceptProviderTurn("accepted-turn")
	if summary := sess.Summary(); !summary.TurnActive {
		t.Fatalf("accepted provider turn summary = %#v", summary)
	}
}

func TestResolveHostedThreadRecordReleasesServerLockBeforeControlStore(t *testing.T) {
	hosted := agentSessionRecord{ID: "nht_lock_boundary"}
	sess := &Session{id: "nano-lock-boundary",
		hostedThreadID: hosted.ID,
		provider:       "codex",
		threadID:       "native-live-only",
		lifecycleState: testLifecycleSnapshot("ready", false)}
	s := NewRegistry()
	s.RegisterSession(sess)

	// Block the downstream lookup and prove the server lock is already free.
	lookupStarted := make(chan struct{})
	releaseLookup := make(chan struct{})
	resolved := make(chan agentSessionRecord, 1)
	go func() {
		record, _ := s.ResolveLiveHostedThreadRecord("native-live-only", func(id string) (agentSessionRecord, bool) {
			close(lookupStarted)
			<-releaseLookup
			return hosted, id == hosted.ID
		})
		resolved <- record
	}()
	select {
	case <-lookupStarted:
	case <-time.After(time.Second):
		t.Fatal("resolver never reached the control-store lookup")
	}
	if !s.mu.TryLock() {
		close(releaseLookup)
		t.Fatal("server lock stayed held while control-store read was blocked")
	}

	s.mu.Unlock()
	close(releaseLookup)
	select {
	case record := <-resolved:
		if record.ID != hosted.ID {
			t.Fatalf("resolved record = %#v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("resolver did not finish after control store unblocked")
	}
}

// Failure caught: after a failed boot resume left an exited Session beside the
// one that replaced it, the lookup returned the first match in map order, so
// most prompts to that hosted thread bound to the exited Session and failed
// with "session is exited". Boundary: the Session a hosted thread's
// operations use.
func TestHostedThreadOperationsUseItsLiveSessionNotAnExitedOne(t *testing.T) {
	sessions := map[string]*Session{
		"dead": {id: "dead", hostedThreadID: "target", lifecycleState: testLifecycleSnapshot(StatusExited, false)},
		"live": {id: "live", hostedThreadID: "target", lifecycleState: testLifecycleSnapshot("ready", false)},
	}
	for i := range 20 { // more entries, so map order varies between lookups
		id := fmt.Sprintf("other-%d", i)
		sessions[id] = &Session{id: id, hostedThreadID: id, lifecycleState: testLifecycleSnapshot("ready", false)}
	}
	s := NewRegistry()
	for _, sess := range sessions {
		s.RegisterSession(sess)
	}
	for range 200 {
		if got := s.SessionForHostedThread("target"); got == nil || got.id != "live" {
			id := "none"
			if got != nil {
				id = got.id
			}
			t.Fatalf("hosted thread session = %s, want the live Session every time", id)
		}
	}
}
