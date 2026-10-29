package app

import (
	"context"
	"net/http"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/setup"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

type threadCatalogServiceOptions = threadindex.Options

// threadCatalogService composes saved catalog rows with the app's response types.
type threadCatalogService struct {
	*threadindex.Service
}

func newThreadCatalogService(ctx context.Context, store *history.Store, options threadCatalogServiceOptions) (*threadCatalogService, error) {
	options.NativeIDProvisional = sessions.NativeThreadIDProvisional
	options.ClaudeProjectSlug = setup.ClaudeProjectSlug
	service, err := threadindex.New(ctx, store, options)
	if err != nil {
		return nil, err
	}
	return &threadCatalogService{Service: service}, nil
}
func (s *threadCatalogService) ListThreads(ctx context.Context, q threadindex.ListQuery) (catalogPage, error) {
	p, e := s.Service.ListThreads(ctx, q)
	return catalogPage{Items: catalogRows(p.Items), NextCursor: p.NextCursor, CatalogVersion: p.CatalogVersion}, e
}
func (s *threadCatalogService) LoadThread(ctx context.Context, id string) (sidebar.Thread, error) {
	t, e := s.Service.LoadThread(ctx, id)
	return sidebar.Thread{Thread: t}, e
}
func (s *threadCatalogService) NamedThreads(ctx context.Context, name string) ([]sidebar.Thread, error) {
	rows, e := s.Service.NamedThreads(ctx, name)
	return catalogRows(rows), e
}
func catalogRows(rows []threadindex.Thread) []sidebar.Thread {
	if rows == nil {
		return nil
	}
	out := make([]sidebar.Thread, len(rows))
	for i, r := range rows {
		out[i] = sidebar.Thread{Thread: r}
	}
	return out
}
func mapCatalogThread(t history.Thread, sources *[]threadindex.SourceDetail) sidebar.Thread {
	return sidebar.Thread{Thread: threadindex.MapThread(t, sources)}
}
func writeCatalogBackendError(w http.ResponseWriter, err error) {
	threadindex.WriteCatalogBackendError(w, err)
}
func writeTranscriptBackendError(w http.ResponseWriter, err error) {
	threadindex.WriteTranscriptBackendError(w, err)
}
func writeCatalogJSON(w http.ResponseWriter, status int, value any) {
	threadindex.WriteCatalogJSON(w, status, value)
}

func catalogIdentities(keys []attachmentKey) []history.ThreadIdentity {
	result := make([]history.ThreadIdentity, len(keys))
	for i, key := range keys {
		result[i] = history.ThreadIdentity{Provider: history.Provider(key.Provider), NativeThreadID: key.NativeThreadID}
	}
	return result
}
