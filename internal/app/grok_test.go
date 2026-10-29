package app

import (
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/sessions"
	grokfixture "github.com/dkroshow/nano/internal/testsupport/grokserver"
)

func TestGrokProviderSelectionSettlesCanonicalEffectiveEffort(t *testing.T) {
	sess, store := causationTestSessionForProvider(t, "nht_grok_effort", "grok")
	sess.State().RecordCreationPosture(sessions.ProviderRequest{Effort: "xhigh"})
	result := json.RawMessage(`{"sessionId":"native","_meta":{"x.ai/sessionConfig":{"options":[{"category":"mode","id":"xhigh","selected":true},{"category":"mode","id":"high","selected":false}]}}}`)
	provider := grokfixture.NewServer(t, func(c *grokfixture.Conn) {
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			switch call.Method {
			case "initialize":
				c.Reply(call, map[string]any{"protocolVersion": 1})
			case "session/new":
				c.Reply(call, result)
			default:
				c.Reply(call, map[string]any{})
			}
		}
	})
	if err := sess.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "grok", Mode: "start", Effort: "xhigh", GrokBin: provider.Bin(), Cwd: t.TempDir()}, nil, func() {}); err != nil {
		t.Fatal(err)
	}
	waitForClaudeStatus(t, sess, "ready")
	field, desired, effective := fieldStrings(t, store, sess.HostedThreadID(), postureFieldEffort)
	if desired != "xhigh" || effective != "xhigh" || field.DesiredDisposition != metadataDispositionConfirmed || field.EffectiveDisposition != metadataDispositionConfirmed {
		t.Fatalf("canonical effort = %+v desired=%q effective=%q", field, desired, effective)
	}
}
