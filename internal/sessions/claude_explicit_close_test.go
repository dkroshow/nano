package sessions

import (
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/testsupport/claudeshim"
)

func TestClaudeExplicitCloseWaitsBeyondShimGrace(t *testing.T) {
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
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
