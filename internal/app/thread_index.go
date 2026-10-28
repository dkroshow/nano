package app

import (
	"context"
	"net/http"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/setup"
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
func (s *threadCatalogService) LoadThread(ctx context.Context, id string) (catalogThread, error) {
	t, e := s.Service.LoadThread(ctx, id)
	return catalogThread{Thread: t}, e
}
func (s *threadCatalogService) NamedThreads(ctx context.Context, name string) ([]catalogThread, error) {
	rows, e := s.Service.NamedThreads(ctx, name)
	return catalogRows(rows), e
}
func (s *threadCatalogService) PublishCreationNames(ctx context.Context, records []creationNameProjectionRecord) error {
	rows := make([]threadindex.CreationNameRecord, len(records))
	for i, r := range records {
		rows[i] = threadindex.CreationNameRecord{Provider: r.Provider, NativeThreadID: r.NativeThreadID, Name: r.Name}
	}
	return s.Service.PublishCreationNames(ctx, rows)
}
func catalogRows(rows []threadindex.Thread) []catalogThread {
	if rows == nil {
		return nil
	}
	out := make([]catalogThread, len(rows))
	for i, r := range rows {
		out[i] = catalogThread{Thread: r}
	}
	return out
}
func mapCatalogThread(t history.Thread, sources *[]threadindex.SourceDetail) catalogThread {
	return catalogThread{Thread: threadindex.MapThread(t, sources)}
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
