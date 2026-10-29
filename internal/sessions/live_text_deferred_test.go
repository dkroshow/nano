package sessions

import (
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/diagnostics"
)

// A known deferred re-read must label an unsaved reply, without suppressing the diagnostic.
func TestLiveTextUnsavedReplyLabelsQueuedAndBuildingRereads(t *testing.T) {
	for _, state := range []string{"ordinary", "queued", "building"} {
		t.Run(state, func(t *testing.T) {
			sink := diagnostics.NewStore(10, "test")
			sess := newFixtureSession(Config{Provider: "codex", HostedThreadID: "synthetic-hosted"}, "native-thread")
			loop := &LiveTextLoop{sources: LiveTextSources{Diagnostics: sink}, sess: sess}
			loop.reportUnsaved("missing-reply", time.Now().Add(-time.Minute), state != "ordinary")
			events := sink.Recent(10, "", "", "file.reply.unsaved")
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
