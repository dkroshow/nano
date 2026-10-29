package sessions

import (
	"testing"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// The application sibling tests persistence across an actual fallback. This
// check follows the transport builder that intentionally does not own lineage.
func TestClaudeAttachmentRecordDoesNotInventLineage(t *testing.T) {
	successor := hostedstore.Attachment{Provider: "claude", NativeThreadID: "native-2", Cwd: "/fixture", Model: "model", Bin: "/fake/claude", Args: []string{"-p"}, ContinuedFrom: "native-1"}
	tr := &claudeShimTransport{owner: &claudeSession{sess: &Session{cwd: successor.Cwd, lifecycleState: testLifecycleSnapshot("ready", false)}}, posture: claudeSpawnPosture{Bin: successor.Bin, Args: successor.Args}}
	rewrite, ok := tr.durableAttachment("native-2", successor.Model)
	if !ok {
		t.Fatal("the shim transport refused to build a durable record")
	}
	if rewrite.ContinuedFrom != "" {
		t.Fatalf("durableAttachment invented lineage: %+v", rewrite)
	}
}
