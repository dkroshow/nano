package threadindex

import (
	"context"
	"github.com/dkroshow/nano/internal/liveview"
)

// SavedTurnEvidence reads a turn by provider-native identity without inferring Claude completion.
func (s *Service) SavedTurnEvidence(ctx context.Context, provider, nativeThreadID, turnID string) (liveview.DurableTurnEvidence, bool) {
	if s == nil || (provider != "codex" && provider != "grok") || turnID == "" || nativeThreadID == "" {
		return liveview.DurableTurnEvidence{}, false
	}
	id, err := s.ResolveThreadReference(ctx, nativeThreadID)
	if err != nil || id == "" {
		return liveview.DurableTurnEvidence{}, false
	}
	thread, err := s.LoadThread(ctx, id)
	if err != nil || thread.Source.Conflict {
		return liveview.DurableTurnEvidence{}, false
	}
	saved, found, err := s.ReadSavedTurnStatus(ctx, id, turnID)
	if err != nil || !found {
		return liveview.DurableTurnEvidence{}, false
	}
	turn := liveview.Turn{ID: turnID, Status: string(saved.Turn.Status), DurationMS: saved.Turn.DurationMS}
	if saved.Turn.StartedAt != nil {
		turn.StartedAtMs = saved.Turn.StartedAt.UnixMilli()
	}
	if saved.Turn.CompletedAt != nil {
		turn.CompletedAtMs = saved.Turn.CompletedAt.UnixMilli()
	}
	return liveview.DurableTurnEvidence{Turn: turn, Newer: saved.Newer}, true
}
