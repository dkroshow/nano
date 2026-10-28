package sessions

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudeExplicitCloseWaitsBeyondShimGrace(t *testing.T) {
	shim := newFakeShim(t, func(c *shimConn) {
		call, ok := c.Next()
		if !ok {
			return
		}
		time.Sleep(3100 * time.Millisecond)
		c.Reply(call, map[string]any{})
	})
	if err := CloseDetachedClaude(shim.WSURL(), "slow-process"); err != nil {
		t.Fatal(err)
	}
}

// Model the real shim's take-before-reap interval. A concurrent raw close
// sees not_found while the first process is demonstrably not reaped.
func TestClaudeExplicitNotFoundBeforeReapIsUncertain(t *testing.T) {
	started, reap := make(chan struct{}), make(chan struct{})
	var count atomic.Int32
	shim := newFakeShim(t, func(c *shimConn) {
		call, ok := c.Next()
		if !ok {
			return
		}
		if count.Add(1) == 1 {
			close(started)
			<-reap
			c.Reply(call, map[string]any{})
		} else {
			c.ReplyError(call, shimCodeSessionNotFound, "session_not_found")
		}
	})
	first := make(chan error, 1)
	go func() { first <- CloseDetachedClaude(shim.WSURL(), "same-process") }()
	<-started
	err := CloseDetachedClaude(shim.WSURL(), "same-process")
	close(reap)
	if err == nil {
		t.Error("registry absence incorrectly proved process exit")
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}
