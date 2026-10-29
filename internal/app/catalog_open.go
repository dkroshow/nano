package app

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	"github.com/dkroshow/nano/internal/config"
	"github.com/dkroshow/nano/internal/configpaths"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

func openThreadCatalog(path, namesPath string, noWatch bool) (*history.Store, *sidebar.CatalogSource, string, error) {
	absolute, err := configpaths.CanonicalProspective(path)
	if err != nil {
		return nil, nil, "", fmt.Errorf("resolve thread catalog database: %w", err)
	}
	namesPath, err = configpaths.CanonicalProspective(namesPath)
	if err != nil {
		return nil, nil, absolute, fmt.Errorf("resolve thread names file: %w", err)
	}
	roots, err := history.ResolveStandardRoots(history.RootOptions{})
	if err != nil {
		return nil, nil, absolute, fmt.Errorf("resolve provider roots before opening catalog: %w", err)
	}
	providerHomes, err := config.ProviderHomes()
	if err != nil {
		return nil, nil, absolute, err
	}
	if err := config.ValidateCatalogLocation(absolute, providerHomes); err != nil {
		return nil, nil, absolute, err
	}
	store, err := history.Open(absolute)
	if err != nil {
		return nil, nil, absolute, err
	}
	service, err := newThreadCatalogService(context.Background(), store, threadindex.Options{
		Roots:                               roots,
		NamesPath:                           namesPath,
		DisableWatcher:                      noWatch,
		Logf:                                log.Printf,
		EnableConversationSearchScheduler:   true,
		EnableSupersededProjectionCollector: config.SupersededProjectionCollectionEnabled(),
		EnableParserStateCompaction:         true,
		EnableUsageBackfillScheduler:        true,
		ParserBackfillPauseFile:             filepath.Join(filepath.Dir(absolute), "parser-backfill.pause"),
	})
	if err != nil {
		_ = store.Close()
		return nil, nil, absolute, err
	}
	return store, service, absolute, nil
}
