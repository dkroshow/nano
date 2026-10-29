package sidebar

import (
	"sort"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// Boot priority tiers put starred rows first, then visible roots and children.
const (
	// BootTierStarred prioritizes operator-starred threads.
	bootTierStarred = iota
	// BootTierTopLevel follows with visible roots.
	bootTierTopLevel
	// BootTierNested follows with visible descendants.
	bootTierNested
	// BootTierUnseen is the fallback for threads outside the projection.
	BootTierUnseen
)

// sidebarVisibleTopLevel is how many top-level rows count as Visible: about
// one screen of the sidebar under its starred rows.
const sidebarVisibleTopLevel = 20

// BootRank is a stored thread's place in the sidebar at boot.
type BootRank struct {
	Tier    int
	Updated time.Time
	Visible bool
}

// BootRanks reads each stored thread's place in the sidebar's default
// view: starred rows, top-level rows and nested rows, with the catalog's
// Updated time. Visible rows are the ones a browser shows first once the
// restart is done: every starred row, and the first sidebarVisibleTopLevel
// top-level rows by recency among the rows the active sidebar will show (rows
// it shows now, plus stored threads, which show once attached). Ordinary
// history rows do not count. A thread the sidebar does not know has no entry
// and ranks as unseen.
func (s *Controller) BootRanks(stored []hostedstore.Attachment) (map[hostedstore.AttachmentKey]BootRank, bool) {
	nodes, ready := s.defaultNodes()
	if !ready {
		return nil, false
	}
	isStored := make(map[hostedstore.AttachmentKey]bool, len(stored))
	for _, rec := range stored {
		isStored[rec.Key()] = true
	}
	ranks := make(map[hostedstore.AttachmentKey]BootRank, len(nodes))
	var topLevel []hostedstore.AttachmentKey
	for _, node := range nodes {
		key := hostedstore.AttachmentKey{Provider: node.thread.Provider, NativeThreadID: node.thread.NativeID}
		r := BootRank{Tier: bootTierNested}
		if Updated := parseCatalogTimePtr(node.thread.UpdatedAt); Updated != nil {
			r.Updated = *Updated
		}
		switch {
		case node.thread.Starred:
			r.Tier, r.Visible = bootTierStarred, true
		case node.parentID == "":
			r.Tier = bootTierTopLevel
			if node.included || isStored[key] {
				topLevel = append(topLevel, key)
			}
		}
		ranks[key] = r
	}
	sort.SliceStable(topLevel, func(i, j int) bool { return ranks[topLevel[i]].Updated.After(ranks[topLevel[j]].Updated) })
	for _, key := range topLevel[:min(len(topLevel), sidebarVisibleTopLevel)] {
		r := ranks[key]
		r.Visible = true
		ranks[key] = r
	}
	return ranks, true
}
