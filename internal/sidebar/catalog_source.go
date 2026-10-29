package sidebar

import (
	"context"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/threadindex"
)

// CatalogSource adapts the saved index to visible rows before live overlays.
type CatalogSource struct {
	*threadindex.Service
}

// ListThreads reads saved catalog rows without live overlays.
func (s *CatalogSource) ListThreads(ctx context.Context, q threadindex.ListQuery) (CatalogPage, error) {
	p, e := s.Service.ListThreads(ctx, q)
	return CatalogPage{Items: catalogRows(p.Items), NextCursor: p.NextCursor, CatalogVersion: p.CatalogVersion}, e
}

// LoadThread reads one saved row without live overlays.
func (s *CatalogSource) LoadThread(ctx context.Context, id string) (Thread, error) {
	t, e := s.Service.LoadThread(ctx, id)
	return Thread{Thread: t}, e
}

// NamedThreads finds saved rows by display name.
func (s *CatalogSource) NamedThreads(ctx context.Context, name string) ([]Thread, error) {
	rows, e := s.Service.NamedThreads(ctx, name)
	return catalogRows(rows), e
}

func catalogRows(rows []threadindex.Thread) []Thread {
	if rows == nil {
		return nil
	}
	out := make([]Thread, len(rows))
	for i, r := range rows {
		out[i] = Thread{Thread: r}
	}
	return out
}

func catalogIdentities(keys []hostedstore.AttachmentKey) []history.ThreadIdentity {
	result := make([]history.ThreadIdentity, len(keys))
	for i, key := range keys {
		result[i] = history.ThreadIdentity{Provider: history.Provider(key.Provider), NativeThreadID: key.NativeThreadID}
	}
	return result
}
