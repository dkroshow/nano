package hosted

import (
	"context"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/sessions"
)

// CompletedFeed constructs the read feed over the same answer and prompt owners.
func (s *AttachmentCoordinator) CompletedFeed() *CompletedFeed {
	return &CompletedFeed{Source: s.CompletedTurns, Executions: s.Executions, Metadata: s.Metadata, Prompts: s.Turns,
		ResolveAnswer: func(ctx context.Context, id string, record prompts.Record, resolve func(string) string) (string, string, string, string, error) {
			answer, err := s.ClaudeAnswers(id).Answer(ctx, sessions.ClaudeSettlementEntry(record.Outbox, record.NativeThreadID, "", resolve))
			return answer.FinalID, answer.Outcome, answer.NativeID, answer.TurnID, err
		}}
}
