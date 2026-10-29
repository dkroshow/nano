package threadindex

import (
	"context"
	"errors"

	"github.com/dkroshow/nano/internal/history"

	threadvalues "github.com/dkroshow/nano/internal/threads"
)

// SavedPromptRows reads the saved user rows of one provider thread that carry
// the given identities.
func (s *Service) SavedPromptRows(ctx context.Context, nativeThreadID string, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
	if s == nil || nativeThreadID == "" {
		return nil, history.ErrSavedTranscriptUnavailable
	}
	threadID, err := s.ResolveThreadReference(ctx, nativeThreadID)
	if errors.Is(err, ErrCatalogThreadNotFound) {
		return nil, history.ErrSavedTranscriptUnavailable
	}
	if err != nil {
		return nil, err
	}
	return s.LoadSavedUserRows(ctx, threadID, keys)
}

// SavedTurnFinal reads a saved turn's own final answer row; a truncated one is
// read whole through the retained body owner, at the same generation, or not
// at all.
func (s *Service) SavedTurnFinal(ctx context.Context, nativeThreadID, projectionKey, turnKey string) (string, string, error) {
	if s == nil || nativeThreadID == "" {
		return "", "", history.ErrSavedTranscriptUnavailable
	}
	threadID, err := s.ResolveThreadReference(ctx, nativeThreadID)
	if errors.Is(err, ErrCatalogThreadNotFound) {
		return "", "", history.ErrSavedTranscriptUnavailable
	}
	if err != nil {
		return "", "", err
	}
	final, err := s.LoadSavedTurnFinal(ctx, threadID, projectionKey, turnKey)
	if err != nil {
		return "", "", err
	}
	if !final.Truncated {
		return final.Text, threadvalues.FirstNonEmpty(final.NativeItemID, final.ItemID), nil
	}
	body, err := s.LoadTranscriptBody(ctx, threadID, final.ItemID)
	if err != nil {
		return "", "", err
	}
	if body.ProjectionKey != projectionKey || body.ItemID != final.ItemID || body.Truncated {
		return "", "", history.ErrTranscriptBodyUnavailable
	}
	return body.Text, threadvalues.FirstNonEmpty(final.NativeItemID, final.ItemID), nil
}
