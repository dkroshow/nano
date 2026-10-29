// Package sidebar owns operator-pinned thread identities, compact runtime
// changes and their replay queues. Its stars database preserves operator
// truth separately from the saved catalog. Live attachment state remains in
// sessions and saved catalog state remains in threadindex.
package sidebar
