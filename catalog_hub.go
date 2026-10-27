package main

import "github.com/dkroshow/nano/internal/catalog"

type catalogChange = catalog.Change
type catalogHub = catalog.Hub

func newCatalogHub() *catalogHub { return catalog.NewHub() }

func mergeCatalogChanges(left, right catalogChange) catalogChange {
	return catalog.MergeChanges(left, right)
}
