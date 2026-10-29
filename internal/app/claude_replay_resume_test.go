package app

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/testsupport/claudeshim"
)

// Exercise the callers too: an attach-level error alone does not prove that
// Resume refuses rather than starting a second writer through its fallback.
func TestClaudeResumeReplayFailureNeverStartsReplacement(t *testing.T) {
	for _, mode := range []string{"manual", "boot"} {
		t.Run(mode, func(t *testing.T) {
			var s *server
			var failOnce sync.Once
			shim := newFakeShim(t, func(c *claudeshim.Conn) {
				for {
					call, ok := c.Next()
					if !ok {
						return
					}
					switch call.Method {
					case shimMethodAttach:
						failed := false
						failOnce.Do(func() { failed = true })
						c.Reply(call, map[string]any{"shimSessionId": "original-child", "nativeSessionId": claudeResumeThreadID, "state": "running", "lastSeq": 1})
						c.Line(1, "recv", claudeInitLine)
						if failed {
							_ = c.Conn.Close()
							return
						}
						c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "original-child", "lastSeq": 1})
					case shimMethodStart:
						c.Reply(call, map[string]any{"shimSessionId": "replacement-child"})
					default:
						c.Reply(call, nil)
					}
				}
			})
			var store *hostedstore.AttachmentStore
			s, store = claudeResumeServer(t, shim.WSURL())
			store.Remember(rememberedClaudeSession(t, "/fake/claude"))
			if mode == "manual" {
				response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
				if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), "claude replay did not settle") {
					t.Fatalf("Resume must refuse replay failure: HTTP %d, %s", response.Code, response.Body.String())
				}
			} else {
				seedHostedAttachmentFixture(t, s, "claude", claudeResumeThreadID)
				fixtureOwners(s).attachmentCoordinator().PrepareBoot()
				fixtureOwners(s).attachmentCoordinator().ReattachOnBoot()
				fixtureReattach(s, store.Records()[0].HostedThreadID)

				waitForClaude(t, "failed boot attachment discarded", func() bool {
					return len(s.SnapshotSessions()) == 0 || len(shim.CallsOf(shimMethodStart)) > 0
				})
			}
			if starts := len(shim.CallsOf(shimMethodStart)); starts != 0 {
				t.Fatalf("replay failure started %d replacements", starts)
			}
			if _, ok := fixtureAttachmentFor(s, "claude", claudeResumeThreadID); !ok {
				t.Fatal("replay failure removed the retryable identity")
			}
			// The failed attachment must not poison the durable identity: the
			// next attempt can attach to that same surviving child.
			response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
			if response.Code != http.StatusOK || len(shim.CallsOf(shimMethodStart)) != 0 {
				t.Fatalf("retry must attach without replacement: HTTP %d, %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestClaudeResumeReplayProcessingTimeoutNeverStartsReplacement(t *testing.T) {
	var s *server
	blocked, release := make(chan struct{}), make(chan struct{})
	var once, unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(release) }) }
	defer unblock()
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			switch call.Method {
			case shimMethodAttach:

				c.Reply(call, map[string]any{"shimSessionId": "original-child", "nativeSessionId": claudeResumeThreadID, "state": "running", "lastSeq": 2})
				c.Line(1, "recv", claudeInitLine)
				c.Line(2, "recv", `{"type":"result","subtype":"success","is_error":false}`)
				c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "original-child", "lastSeq": 2})
			case shimMethodStart:
				c.Reply(call, map[string]any{"shimSessionId": "replacement-child"})
				unblock()
			default:
				c.Reply(call, nil)
			}
		}
	})
	var store *hostedstore.AttachmentStore
	s, store = claudeResumeServer(t, shim.WSURL())
	t.Cleanup(unblock)
	store.Remember(rememberedClaudeSession(t, "/fake/claude"))
	s.catalogRefresh = func(string) { once.Do(func() { close(blocked); <-release }) }
	response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), "claude replay did not settle") || len(shim.CallsOf(shimMethodStart)) != 0 {
		t.Fatalf("timeout must refuse without replacement: HTTP %d, starts %d, %s", response.Code, len(shim.CallsOf(shimMethodStart)), response.Body.String())
	}
	if _, ok := fixtureAttachmentFor(s, "claude", claudeResumeThreadID); !ok {
		t.Fatal("timeout removed the retryable identity")
	}
}
