package app

import (
	"context"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
)

// waitTurnSendDone waits for a persisted delivery result and any registered
// delivery work, using the same boundaries available to application callers.
func waitTurnSendDone(t *testing.T, sess *session, actionID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		attempt, err := sess.PromptStore().GetAttempt(context.Background(), actionID)
		if err == nil && attempt.State != prompts.JournalQueued && attempt.State != prompts.JournalSending {
			for _, done := range sess.InFlightDeliveries() {
				select {
				case <-done:
				case <-time.After(time.Until(deadline)):
					t.Fatalf("send operation %s did not finish", actionID)
				}
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("send operation %s did not finish", actionID)
}
