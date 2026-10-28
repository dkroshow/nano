package threadindex

import (
	"context"

	"github.com/dkroshow/nano/internal/history"
)

// LoadSavedUserRows returns saved user rows matching exact delivered identities.
func (s *Service) LoadSavedUserRows(ctx context.Context, threadID string, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
	return s.store.ReadSavedUserRows(ctx, threadID, keys)
}

// LoadSavedTurnFinal returns the saved final answer for one projection and turn.
func (s *Service) LoadSavedTurnFinal(ctx context.Context, threadID, projectionKey, turnKey string) (history.SavedTurnFinal, error) {
	return s.store.ReadSavedTurnFinal(ctx, threadID, projectionKey, turnKey)
}
