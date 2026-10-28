package threadindex

import (
	"context"
)

// Namer is the rename write surface. It resolves the nano id to the
// truth-file key, writes only the file (never the store), and requests a refresh
// so the existing scan chain converges every open tab. Optional so a catalog
// without a names file (or a test stub) reports the endpoint unavailable.
type Namer interface {
	RenameThread(ctx context.Context, id, name string) (ThreadName, error)
	ClearThreadName(ctx context.Context, id string) error
	// AssignThreadName writes by the truth-file's own key — the spawn-time
	// path, used before any catalog row exists for the thread.
	AssignThreadName(provider, nativeID, name string) error
	// AssignThreadNameIfEmpty applies a staged pre-bind operator rename without
	// overwriting a name already stored for the provider identity.
	AssignThreadNameIfEmpty(provider, nativeID, name string) error
}
