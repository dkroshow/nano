package sessions

import (
	"context"

	"github.com/dkroshow/nano/internal/history"
)

// AttachedCatalog is the catalog side of an attachment's file holds.
// An owner is a Nano session id: each session is exactly one attachment
// generation, and a re-attach is a new session.
type AttachedCatalog interface {
	// BindAttachedSource holds the owner's native files. With background,
	// their catch-up pass waits behind foreground sources, for a bounded time.
	BindAttachedSource(owner, provider, nativeID, cwd, known string, background bool)
	// ReleaseAttachedSource releases the attachment’s catalog file holds.
	ReleaseAttachedSource(owner string)
	// AwaitAttachedCurrent waits until the owner's saved files are proven
	// current, and reports false when the owner is released or cancel closes
	// first. With fresh, proof taken before the call does not count.
	// neverPrompted, when set, reads the caller's evidence that its Grok
	// session was never prompted.
	AwaitAttachedCurrent(owner string, fresh bool, neverPrompted func() bool, cancel <-chan struct{}) bool
}

// SavedPromptReader reads saved user rows by delivered prompts' exact
// identities, from the session's current native thread only.
type SavedPromptReader func(context.Context, history.SavedUserKeys) ([]history.SavedUserRow, error)

// savedPromptNativeReader reads saved user rows of a named native thread.
type savedPromptNativeReader func(context.Context, string, history.SavedUserKeys) ([]history.SavedUserRow, error)

// savedTurnFinalReader reads the final answer text and its native message id
// of one saved turn of a named native thread at one projection generation.
type savedTurnFinalReader func(ctx context.Context, nativeThreadID, projectionKey, turnKey string) (string, string, error)
