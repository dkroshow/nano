package app

import (
	"context"
	"net/http"

	"github.com/dkroshow/nano/internal/core/prompts"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/sessions"
)

func (s *server) handleCompletedTurns(w http.ResponseWriter, r *http.Request, id string) {
	feed := hostedmodel.CompletedFeed{Source: s.completedTurns, Executions: s.agentExecutions, Metadata: s.controlMetadata, Prompts: s.turns,
		ResolveAnswer: func(ctx context.Context, id string, record prompts.Record, resolve func(string) string) (string, string, string, string, error) {
			answer, err := s.attachmentCoordinator().ClaudeAnswers(id).Answer(ctx, sessions.ClaudeSettlementEntry(record.Outbox, record.NativeThreadID, "", resolve))
			return answer.FinalID, answer.Outcome, answer.NativeID, answer.TurnID, err
		}}
	feed.HandleCompletedTurns(w, r, id)
}
