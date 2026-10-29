package store

import (
	"testing"
)

// Publications can arrive out of order; a revision that moved back would hide
// the hint a stale pane needs to re-read its metadata.
func TestLatestRevisionNeverMovesBack(t *testing.T) {
	store := &MetadataStore{}
	store.publishChange("nht_a", 5, "model")
	store.publishChange("nht_a", 3, "effort")
	if got := store.LatestRevision("nht_a"); got != 5 {
		t.Fatalf("latestRevision = %d, want 5", got)
	}
}
