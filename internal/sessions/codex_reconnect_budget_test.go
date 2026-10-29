package sessions

import (
	"encoding/json"
	"testing"
	"time"
)

func TestReconnectAuditDeadlineBudgets(t *testing.T) {
	fake := newFakeAppServer(t)
	daemon := fake.daemon()
	daemon.resumeTimeout, daemon.reconnectBackoff = 40*time.Millisecond, time.Millisecond
	sess := reconnectTestSession(t, daemon, "budgets")
	fake.SetReplyDelay("thread/resume", 200*time.Millisecond)
	sess.setStatus("reconnecting")
	daemon.requestSessionRecovery(sess)
	for attempt, expected := range []int64{40, 80, 120} {
		waitReconnectCondition(t, func() bool { return fake.ResumeCount() >= attempt+1 })
		sess.stateOwner.stateMu.Lock()
		raw := append(json.RawMessage(nil), sess.stateOwner.state["codexReconnect"]...)
		sess.stateOwner.stateMu.Unlock()
		var state struct {
			TimeoutMS int64 `json:"timeoutMs"`
		}
		if err := json.Unmarshal(raw, &state); err != nil || state.TimeoutMS != expected {
			t.Fatalf("attempt %d budget: %s, %v", attempt+1, raw, err)
		}
	}
	sess.Close()
}
