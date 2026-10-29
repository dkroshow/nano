package threadindex

import (
	"github.com/dkroshow/nano/internal/catalog"
)

func newCatalogHub() *catalog.Hub { return catalog.NewHub() }

func mergeCatalogChanges(left, right catalog.Change) catalog.Change {
	return catalog.MergeChanges(left, right)
}
