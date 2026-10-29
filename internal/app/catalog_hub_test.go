package app

import (
	"github.com/dkroshow/nano/internal/catalog"
)

func newCatalogHub() *catalog.Hub { return catalog.NewHub() }
