package sidebar

import (
	"context"
	"fmt"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/threadindex"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type countingCatalog struct {
	*CatalogSource
	lists atomic.Int64
}

func (c *countingCatalog) ListThreads(ctx context.Context, q threadindex.ListQuery) (CatalogPage, error) {
	c.lists.Add(1)
	return c.CatalogSource.ListThreads(ctx, q)
}

// TestNativeThreadIDFilterChunksUnderParameterLimit catches an identity read
// that exceeds SQLite's parameter limit or confuses providers. Boundary:
// threadsByIdentity against a real store with 1,200
// identities, one native id present under two providers and one asked for
// under a provider that does not hold it; and the store's own refusal of one
// read above history.MaxThreadIdentities.
func TestNativeThreadIDFilterChunksUnderParameterLimit(t *testing.T) {
	ctx := context.Background()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	codex := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}
	claude := history.SourceRoot{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: t.TempDir()}
	for _, root := range []history.SourceRoot{codex, claude} {
		if _, err := store.EnsureRoot(ctx, root); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	court := []historyCourt{{final: true, text: "Done."}}
	seedSidebarThread(t, store, codex, "shared-native", "", history.ThreadMain, now, court)
	// The same native id under a second provider needs only its thread row.
	sharedUpdated := now.Add(-time.Minute)
	if _, err := store.ApplySource(ctx, testSidebarSource(claude, "shared-native.jsonl", "shared-native", history.ThreadMain, "", &sharedUpdated)); err != nil {
		t.Fatal(err)
	}
	seedSidebarThread(t, store, codex, "codex-only", "", history.ThreadMain, now.Add(-2*time.Minute), court)
	// A guardian is a subagent source whose thread the parser classifies guardian.
	guardianUpdated := now.Add(-3 * time.Minute)
	guardian := testSidebarSource(codex, "guardian-native.jsonl", "guardian-native", history.ThreadSubagent, "codex-only", &guardianUpdated)
	guardian.Result.Projection.Kind = history.ThreadGuardian
	if _, err := store.ApplySource(ctx, guardian); err != nil {
		t.Fatal(err)
	}
	catalog := &countingCatalog{CatalogSource: &CatalogSource{Service: newStoredCatalog(t, store)}}
	s := &CatalogViews{Catalog: catalog}

	keys := make([]hostedstore.AttachmentKey, 1200)
	for i := range keys {
		keys[i] = hostedstore.AttachmentKey{Provider: "codex", NativeThreadID: fmt.Sprintf("absent-%04d", i)}
	}
	keys[0] = hostedstore.AttachmentKey{Provider: "codex", NativeThreadID: "shared-native"}
	keys[600] = hostedstore.AttachmentKey{Provider: "claude", NativeThreadID: "shared-native"}
	keys[1199] = hostedstore.AttachmentKey{Provider: "codex", NativeThreadID: "codex-only"}
	keys[1198] = hostedstore.AttachmentKey{Provider: "grok", NativeThreadID: "codex-only"}
	keys[900] = hostedstore.AttachmentKey{Provider: "codex", NativeThreadID: "guardian-native"}
	found, err := s.threadsByIdentity(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	if reads := catalog.lists.Load(); reads != 3 {
		t.Fatalf("1,200 identities took %d reads; want 3 (500, 500, 200)", reads)
	}
	if len(found) != 4 {
		t.Fatalf("found %d threads, want 4: %#v", len(found), found)
	}
	// A live guardian is found too, as the former point read found it.
	for _, key := range []hostedstore.AttachmentKey{keys[0], keys[600], keys[900], keys[1199]} {
		if row, ok := found[key]; !ok || row.Provider != key.Provider || row.NativeID != key.NativeThreadID {
			t.Errorf("%s/%s = %#v, %v", key.Provider, key.NativeThreadID, row, ok)
		}
	}
	// The read itself is keyed by provider: asking for the claude copy returns
	// only the claude row.
	page, err := store.ListThreads(ctx, history.ListThreadsParams{IncludeGuardians: true, Limit: 10,
		Identities: []history.ThreadIdentity{{Provider: history.ProviderClaude, NativeThreadID: "shared-native"}}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Provider != history.ProviderClaude {
		t.Fatalf("claude/shared-native read = %#v, %v", page.Items, err)
	}
	if _, err := store.ListThreads(ctx, history.ListThreadsParams{Identities: make([]history.ThreadIdentity, history.MaxThreadIdentities+1), Limit: 10}); err == nil {
		t.Fatal("the store accepted an identity read above its cap")
	}
}
