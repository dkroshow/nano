package app

import (
	"context"

	"github.com/dkroshow/nano/internal/history"
)

// posture_view.go — the one posture struct every surface serves (coordination
// plan, decision 4): /api/sessions summaries, /state, and the catalog's
// memory rows all project from sessionPosture instead of hand-assembling
// disjoint field sets. Values come from control.sqlite's immutable hosted
// event log and current-state projection; live transport and retired JSON are
// never a fallback for a hosted identity.

// threadNameReader is the optional read half of the names surface. The names
// store keeps its own truth file (decision 1: names are joined at read time,
// not copied into the posture log); this is how a resumed session picks its
// durable name back up for the boot summary.
type threadNameReader interface {
	ThreadNameFor(ctx context.Context, provider, nativeThreadID string) (string, bool)
}

type threadNameRefReader interface {
	ThreadNameRefFor(ctx context.Context, provider, nativeThreadID string) (*history.ThreadNameRef, bool)
}

// durableThreadName reads the durable user-assigned name for a thread, ""
// when unnamed or when the catalog cannot answer.
func (s *server) durableThreadName(provider, nativeThreadID string) (string, bool) {
	reader, ok := s.catalog.(threadNameReader)
	if !ok || nativeThreadID == "" {
		return "", false
	}
	return reader.ThreadNameFor(context.Background(), provider, nativeThreadID)
}

func (s *server) durableThreadNameRef(provider, nativeThreadID string) (hostedMetadataName, bool) {
	reader, ok := s.catalog.(threadNameRefReader)
	if !ok || nativeThreadID == "" {
		return hostedMetadataName{}, false
	}
	ref, ok := reader.ThreadNameRefFor(context.Background(), provider, nativeThreadID)
	if !ok || ref == nil || ref.Value == "" {
		return hostedMetadataName{}, false
	}
	return hostedMetadataName{Value: ref.Value, Provenance: string(ref.Provenance)}, true
}
