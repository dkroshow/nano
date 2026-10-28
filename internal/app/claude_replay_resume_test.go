package app

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	coresession "github.com/dkroshow/nano/internal/core/session"
)

// Exercise the callers too: an attach-level error alone does not prove that
// Resume refuses rather than starting a second writer through its fallback.
func TestClaudeResumeReplayFailureNeverStartsReplacement(t *testing.T) {
	for _, mode := range []string{"manual", "boot"} {
		t.Run(mode, func(t *testing.T) {
			var s *server
			var failOnce sync.Once
			shim := newFakeShim(t, func(c *shimConn) {
				for {
					call, ok := c.next()
					if !ok {
						return
					}
					switch call.Method {
					case shimMethodAttach:
						sess := s.sessionForThread("claude", claudeResumeThreadID)
						if sess == nil {
							t.Error("missing registered resume shell")
							return
						}
						failOnce.Do(sess.coreSession().Close)
						c.reply(call, map[string]any{"shimSessionId": "original-child", "nativeSessionId": claudeResumeThreadID, "state": "running", "lastSeq": 1})
						c.line(1, "recv", claudeInitLine)
						c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "original-child", "lastSeq": 1})
					case shimMethodStart:
						c.reply(call, map[string]any{"shimSessionId": "replacement-child"})
					default:
						c.reply(call, nil)
					}
				}
			})
			var store *attachmentStore
			s, store = claudeResumeServer(t, shim.wsURL())
			store.remember(rememberedClaudeSession(t, "/fake/claude"))
			if mode == "manual" {
				response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
				if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), errClaudeReplayUnsettled.Error()) {
					t.Fatalf("Resume must refuse replay failure: HTTP %d, %s", response.Code, response.Body.String())
				}
			} else {
				seedHostedAttachmentFixture(t, s, "claude", claudeResumeThreadID)
				sess, err := s.spawnSession(spawnRequest{providerRequest: providerRequest{Provider: "claude", Mode: "resume", ThreadID: claudeResumeThreadID, bootReattach: &bootProviderControl{}}})
				if err != nil {
					t.Fatal(err)
				}
				waitForClaude(t, "failed boot attachment discarded", func() bool {
					s.mu.Lock()
					defer s.mu.Unlock()
					return s.sessions[sess.id] == nil || len(shim.callsOf(shimMethodStart)) > 0
				})
			}
			if starts := len(shim.callsOf(shimMethodStart)); starts != 0 {
				t.Fatalf("replay failure started %d replacements", starts)
			}
			if _, ok := s.attachmentFor("claude", claudeResumeThreadID); !ok {
				t.Fatal("replay failure removed the retryable identity")
			}
			// The failed attachment must not poison the durable identity: the
			// next attempt can attach to that same surviving child.
			response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
			if response.Code != http.StatusOK || len(shim.callsOf(shimMethodStart)) != 0 {
				t.Fatalf("retry must attach without replacement: HTTP %d, %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestClaudeResumeReplayProcessingTimeoutNeverStartsReplacement(t *testing.T) {
	var s *server
	blocked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	shim := newFakeShim(t, func(c *shimConn) {
		for {
			call, ok := c.next()
			if !ok {
				return
			}
			switch call.Method {
			case shimMethodAttach:
				sess := s.sessionForThread("claude", claudeResumeThreadID)
				if sess == nil {
					t.Error("missing registered resume shell")
					return
				}
				sess.coreSession().ObserveAsync(coresession.AdapterEvent{LegacyApply: func() error {
					close(blocked)
					<-release
					return nil
				}})
				<-blocked
				c.reply(call, map[string]any{"shimSessionId": "original-child", "nativeSessionId": claudeResumeThreadID, "state": "running", "lastSeq": 1})
				c.line(1, "recv", claudeInitLine)
				c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "original-child", "lastSeq": 1})
			case shimMethodStart:
				c.reply(call, map[string]any{"shimSessionId": "replacement-child"})
				unblock()
			default:
				c.reply(call, nil)
			}
		}
	})
	var store *attachmentStore
	s, store = claudeResumeServer(t, shim.wsURL())
	t.Cleanup(unblock)
	store.remember(rememberedClaudeSession(t, "/fake/claude"))
	// The blocked apply never settles, so the resume waits out the replay
	// timeout. Shorten it; the production 30s default adds nothing here.
	previous := claudeShimReplayTimeout
	claudeShimReplayTimeout = 300 * time.Millisecond
	t.Cleanup(func() { claudeShimReplayTimeout = previous })
	response := postClaudeResume(t, s, resumeBody(claudeResumeThreadID))
	if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), errClaudeReplayUnsettled.Error()) || len(shim.callsOf(shimMethodStart)) != 0 {
		t.Fatalf("timeout must refuse without replacement: HTTP %d, starts %d, %s", response.Code, len(shim.callsOf(shimMethodStart)), response.Body.String())
	}
	if _, ok := s.attachmentFor("claude", claudeResumeThreadID); !ok {
		t.Fatal("timeout removed the retryable identity")
	}
}
