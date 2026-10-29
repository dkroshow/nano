package sessions

import (
	"testing"
)

// Every provider session gets the loop that owns its file-only pane.
func TestEveryProviderSessionBuildsLiveTextLoop(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {

		projection := newLiveProjection(provider)
		sess := newFixtureSession(Config{Provider: provider, Projection: projection}, "")
		sess.AttachLiveText(NewLiveTextLoop(LiveTextSources{}, sess))
		if sess.LiveTextService() == nil || projection.liveText != sess.LiveTextService() {
			t.Fatalf("%s session has no file-only loop", provider)
		}
	}
}
