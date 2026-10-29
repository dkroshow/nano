package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/config"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	grokfixture "github.com/dkroshow/nano/internal/testsupport/grokserver"
)

func TestResolveGrokSpawnAcceptsSupportedSessionPermissionPosture(t *testing.T) {
	cwd := t.TempDir()
	s := &server{defaults: config.SessionDefaults{CWD: cwd}}
	for _, mode := range []string{"", "auto", "bypassPermissions"} {
		resolved, err := s.attachmentCoordinator().ResolveSpawn(hostedmodel.SpawnRequest{ProviderRequest: providerRequest{Provider: "grok", Mode: "start", Cwd: cwd, PermissionMode: mode}})
		if err != nil || resolved.PermissionMode != mode {
			t.Fatalf("resolveSpawn(%q) = mode %q, error %v", mode, resolved.PermissionMode, err)
		}
	}
	_, err := s.attachmentCoordinator().ResolveSpawn(hostedmodel.SpawnRequest{ProviderRequest: providerRequest{Provider: "grok", Mode: "start", Cwd: cwd, PermissionMode: "acceptEdits"}})
	if err == nil || !strings.Contains(err.Error(), "Grok permissionMode") {
		t.Fatalf("unsupported resolveSpawn error = %v", err)
	}
}

func TestGrokProviderSelectionSettlesCanonicalEffectiveEffort(t *testing.T) {
	sess, store := causationTestSessionForProvider(t, "nht_grok_effort", "grok")
	sess.RecordCreationPosture(providerRequest{Effort: "xhigh"})
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
	if err := sess.BeginProviderSpawn(providerRequest{Provider: "grok", Mode: "start", Effort: "xhigh", GrokBin: provider.Bin(), Cwd: t.TempDir()}, nil, func() {}); err != nil {
		t.Fatal(err)
	}
	waitForClaudeStatus(t, sess, "ready")
	field, desired, effective := fieldStrings(t, store, sess.HostedThreadID(), postureFieldEffort)
	if desired != "xhigh" || effective != "xhigh" || field.DesiredDisposition != metadataDispositionConfirmed || field.EffectiveDisposition != metadataDispositionConfirmed {
		t.Fatalf("canonical effort = %+v desired=%q effective=%q", field, desired, effective)
	}
}
