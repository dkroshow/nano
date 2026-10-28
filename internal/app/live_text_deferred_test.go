package app

import (
	"testing"
	"time"
)

// A known deferred re-read must label an unsaved reply, without suppressing the diagnostic.
func TestLiveTextUnsavedReplyLabelsQueuedAndBuildingRereads(t *testing.T) {
	f := newParserBudgetFixture(t, 1)
	f.service.Close()
	catalog := synchronousRereadService(f)
	for _, state := range []string{"ordinary", "queued", "building"} {
		t.Run(state, func(t *testing.T) {
			srv := &server{catalog: catalog, browserDiagnostics: newBrowserDiagnosticStore(10)}
			sess := newFixtureSession(sessionConfig{Provider: "codex", HostedThreadID: "synthetic-hosted"}, f.stored(t).Projection.NativeThreadID)
			loop := &liveTextLoop{srv: srv, sess: sess, awaiting: map[string]awaitedReply{"missing-reply": {finishedAt: time.Now().Add(-time.Minute)}}}
			if state == "queued" {
				catalog.enqueueTranscriptReread(f.threadID)
			}
			if state == "building" {
				catalog.finishTranscriptReread(f.threadID, false)
				catalog.rereadBuilding = f.threadID
			}
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
