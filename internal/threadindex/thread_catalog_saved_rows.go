package threadindex

import (
	"context"

	"github.com/dkroshow/nano/internal/history"
)

func (s *Service) LoadSavedUserRows(ctx context.Context, threadID string, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
	return s.store.ReadSavedUserRows(ctx, threadID, keys)
}

func (s *Service) LoadSavedTurnFinal(ctx context.Context, threadID, projectionKey, turnKey string) (history.SavedTurnFinal, error) {
	return s.store.ReadSavedTurnFinal(ctx, threadID, projectionKey, turnKey)
}
