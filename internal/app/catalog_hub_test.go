package app

import "github.com/dkroshow/nano/internal/catalog"

type catalogHub = catalog.Hub

func newCatalogHub() *catalogHub { return catalog.NewHub() }
