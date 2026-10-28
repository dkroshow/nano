package app

import (
	"testing"
	"time"
)

// A known deferred re-read must label an unsaved reply, without suppressing the diagnostic.
func TestLiveTextUnsavedReplyLabelsQueuedAndBuildingRereads(t *testing.T) {
	catalog := &deferredDiagnosticCatalog{fakeThreadCatalog: &fakeThreadCatalog{}}
	for _, state := range []string{"ordinary", "queued", "building"} {
		t.Run(state, func(t *testing.T) {
			srv := &server{catalog: catalog, browserDiagnostics: newBrowserDiagnosticStore(10)}
			sess := newFixtureSession(sessionConfig{Provider: "codex", HostedThreadID: "synthetic-hosted"}, "native-thread")
			loop := &liveTextLoop{srv: srv, sess: sess, awaiting: map[string]awaitedReply{"missing-reply": {finishedAt: time.Now().Add(-time.Minute)}}}
			catalog.deferred = state != "ordinary"
			loop.checkUnsaved("missing-reply")
			events := srv.browserDiagnostics.recent(10, "", "", "file.reply.unsaved")
			if len(events) != 1 {
				t.Fatal("unsaved-reply diagnostic was suppressed", len(events))
			}
			want := ""
			if state != "ordinary" {
				want = "deferred-re-read"
			}
			if events[0].Status != want || events[0].Code != "missing-reply" {
				t.Fatalf("diagnostic label=%q code=%q", events[0].Status, events[0].Code)
			}
		})
	}
}

type deferredDiagnosticCatalog struct {
	*fakeThreadCatalog
	deferred bool
}

func (c *deferredDiagnosticCatalog) DeferredTranscriptReread(string) bool { return c.deferred }
