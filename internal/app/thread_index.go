package app

import (
	"context"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/setup"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

func newThreadCatalogService(ctx context.Context, store *history.Store, options threadindex.Options) (*sidebar.CatalogSource, error) {
	options.NativeIDProvisional = sessions.NativeThreadIDProvisional
	options.ClaudeProjectSlug = setup.ClaudeProjectSlug
	service, err := threadindex.New(ctx, store, options)
	if err != nil {
		return nil, err
	}
	return &sidebar.CatalogSource{Service: service}, nil
}
