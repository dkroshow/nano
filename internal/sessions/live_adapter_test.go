package sessions

import (
	"testing"
	"time"

	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func TestLiveAdaptersReturnTheParserBatch(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			var adapter liveAdapter
			switch provider {
			case "codex":
				adapter = newCodexLiveAdapter()
			case "claude":
				adapter = newClaudeLiveAdapter()
			case "grok":
				adapter = newGrokLiveAdapter()
			}
			result := adapter.Apply(protocol.Recv, []byte(`{"not":"a semantic provider frame"}`), time.Unix(200, 0).UTC())
			if result.Versions.Schema == "" {
				t.Fatal("adapter discarded the normalized parser batch")
			}
		})
	}
}
