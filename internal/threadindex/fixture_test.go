package threadindex

import (
	"context"
	"net/http"
	"strings"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/setup"
)

func newTestCatalogService(ctx context.Context, store *history.Store, options Options) (*Service, error) {
	options.NativeIDProvisional = func(provider, id string) bool { return provider == "claude" && strings.HasPrefix(id, "cs_") }
	options.ClaudeProjectSlug = setup.ClaudeProjectSlug
	return New(ctx, store, options)
}
func catalogTestHandler(h *Handlers) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("PUT /api/v1/threads/{threadId}/name", h.HandleThreadNameSet)
	m.HandleFunc("DELETE /api/v1/threads/{threadId}/name", h.HandleThreadNameClear)
	return m
}
