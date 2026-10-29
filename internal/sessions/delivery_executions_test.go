package sessions

import (
	"context"
	"fmt"
	"time"

	"github.com/dkroshow/nano/internal/threads"
)

// unlinkedPromptExecutions represents the empty operation store used by human-prompt tests.
// It reports the absence of an operation; operation persistence has separate owner tests.
type unlinkedPromptExecutions struct{ Executions }

func (*unlinkedPromptExecutions) PersistedTurnForTransportAction(context.Context, string, string) (threads.TurnRecord, bool, error) {
	return threads.TurnRecord{}, false, nil
}
func (*unlinkedPromptExecutions) NoteHumanPrompt(string, time.Time) error {
	return fmt.Errorf("unknown hosted thread")
}
func (*unlinkedPromptExecutions) TurnForTransportAction(string) (threads.TurnRecord, bool) {
	return threads.TurnRecord{}, false
}
