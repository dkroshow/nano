package threadindex

import (
	"context"
	"fmt"
)

// AmbiguousReferenceError is a prefix that matches more than one thread.
// It carries the candidates so the caller can pick, rather than guessing.
type AmbiguousReferenceError struct{ Candidates []string }

// Error reports the domain error’s user-facing message.
func (e *AmbiguousReferenceError) Error() string {
	return fmt.Sprintf("thread reference matches %d threads", len(e.Candidates))
}

// ReferenceResolver is the optional id-resolution surface. A catalog
// without it (a stub in tests) simply takes the reference verbatim, which is
// exactly the exact-id case.
type ReferenceResolver interface {
	ResolveThreadReference(ctx context.Context, reference string) (string, error)
}

// maxThreadReferenceCandidates bounds ambiguous reference responses.
const maxThreadReferenceCandidates = 10

// ResolveReference uses the optional catalog resolver, or treats the reference
// as an exact id when a caller has no resolver.
func ResolveReference(ctx context.Context, resolver ReferenceResolver, reference string) (string, error) {
	if resolver == nil {
		return reference, nil
	}
	return resolver.ResolveThreadReference(ctx, reference)
}
