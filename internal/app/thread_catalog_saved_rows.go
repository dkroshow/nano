package app

import (
	"context"
	"errors"

	"github.com/dkroshow/nano/internal/history"
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

// savedRowsCatalog reads a thread's saved user rows by exact identity.
type savedRowsCatalog interface {
	LoadSavedUserRows(context.Context, string, history.SavedUserKeys) ([]history.SavedUserRow, error)
}

func (s *threadCatalogService) LoadSavedUserRows(ctx context.Context, threadID string, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
	return s.store.ReadSavedUserRows(ctx, threadID, keys)
}

func (s *threadCatalogService) LoadSavedTurnFinal(ctx context.Context, threadID, projectionKey, turnKey string) (history.SavedTurnFinal, error) {
	return s.store.ReadSavedTurnFinal(ctx, threadID, projectionKey, turnKey)
}

// savedPromptReader reads saved user rows by delivered prompts' exact
// identities, from the session's current native thread only.
type savedPromptReader func(context.Context, history.SavedUserKeys) ([]history.SavedUserRow, error)

// savedPromptNativeReader reads saved user rows of a named native thread.
type savedPromptNativeReader func(context.Context, string, history.SavedUserKeys) ([]history.SavedUserRow, error)

// savedTurnFinalReader reads the final answer text and its native message id
// of one saved turn of a named native thread at one projection generation.
type savedTurnFinalReader func(ctx context.Context, nativeThreadID, projectionKey, turnKey string) (string, string, error)

func (s *server) savedPromptsFor(sess *session) savedPromptReader {
	return func(ctx context.Context, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
		return s.savedPromptRows(ctx, sess.currentThreadID(), keys)
	}
}

// savedPromptRows reads the saved user rows of one provider thread that carry
// the given identities.
func (s *server) savedPromptRows(ctx context.Context, nativeThreadID string, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
	if s.catalog == nil || nativeThreadID == "" {
		return nil, history.ErrSavedTranscriptUnavailable
	}
	resolver, resolves := s.catalog.(threadReferenceResolver)
	catalog, loads := s.catalog.(savedRowsCatalog)
	if !resolves || !loads {
		return nil, history.ErrSavedTranscriptUnavailable
	}
	threadID, err := resolver.ResolveThreadReference(ctx, nativeThreadID)
	if errors.Is(err, errCatalogThreadNotFound) {
		return nil, history.ErrSavedTranscriptUnavailable
	}
	if err != nil {
		return nil, err
	}
	return catalog.LoadSavedUserRows(ctx, threadID, keys)
}

// savedTurnFinal reads a saved turn's own final answer row; a truncated one is
// read whole through the retained body owner, at the same generation, or not
// at all.
func (s *server) savedTurnFinal(ctx context.Context, nativeThreadID, projectionKey, turnKey string) (string, string, error) {
	if s.catalog == nil || nativeThreadID == "" {
		return "", "", history.ErrSavedTranscriptUnavailable
	}
	resolver, resolves := s.catalog.(threadReferenceResolver)
	catalog, loads := s.catalog.(interface {
		LoadSavedTurnFinal(context.Context, string, string, string) (history.SavedTurnFinal, error)
		LoadTranscriptBody(context.Context, string, string) (history.TranscriptRetainedBody, error)
	})
	if !resolves || !loads {
		return "", "", history.ErrSavedTranscriptUnavailable
	}
	threadID, err := resolver.ResolveThreadReference(ctx, nativeThreadID)
	if errors.Is(err, errCatalogThreadNotFound) {
		return "", "", history.ErrSavedTranscriptUnavailable
	}
	if err != nil {
		return "", "", err
	}
	final, err := catalog.LoadSavedTurnFinal(ctx, threadID, projectionKey, turnKey)
	if err != nil {
		return "", "", err
	}
	if !final.Truncated {
		return final.Text, threadvalues.FirstNonEmpty(final.NativeItemID, final.ItemID), nil
	}
	body, err := catalog.LoadTranscriptBody(ctx, threadID, final.ItemID)
	if err != nil {
		return "", "", err
	}
	if body.ProjectionKey != projectionKey || body.ItemID != final.ItemID || body.Truncated {
		return "", "", history.ErrTranscriptBodyUnavailable
	}
	return body.Text, threadvalues.FirstNonEmpty(final.NativeItemID, final.ItemID), nil
}

var _ savedRowsCatalog = (*threadCatalogService)(nil)
