package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

// catalogStoreFixture seeds the real index. Metadata-only rows deliberately
// have no source file, as when a provider has retired its transcript.
type catalogStoreFixture struct {
	source *sidebar.CatalogSource
	store  *history.Store
	db     *sql.DB
	root   history.PersistedRoot
}

func newCatalogStoreFixture(t *testing.T) *catalogStoreFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}
	persisted, err := store.EnsureRoot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newThreadCatalogService(context.Background(), store, threadindex.Options{Roots: []history.SourceRoot{root}, NamesPath: filepath.Join(t.TempDir(), "names.json"), DisableWatcher: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(source.Close)
	waitForCatalog(t, func() bool {
		status, err := source.IndexStatus(context.Background())
		return err == nil && status.State == "ready"
	})
	return &catalogStoreFixture{source: source, store: store, db: db, root: persisted}
}

func (f *catalogStoreFixture) row(t *testing.T, row sidebar.Thread) {
	t.Helper()
	if row.ID == "" {
		return
	}
	if row.Provider == "" {
		row.Provider = "codex"
	}
	if row.NativeID == "" {
		row.NativeID = row.ID
	}
	if row.Kind == "" || row.Kind == "thread" {
		row.Kind = "main"
	}
	if row.Interaction == "" {
		row.Interaction = "interactive"
	}
	// Convert through JSON because the wire cells and durable cells deliberately
	// use different types but the same documented field representation.
	b, err := json.Marshal(row.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.db.Exec(`INSERT OR REPLACE INTO threads (id,source_root_id,provider,native_thread_id,kind,parent_native_id,title,preview,created_at,updated_at,metadata_json,archived,source_status,interaction) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, row.ID, f.root.ID, row.Provider, row.NativeID, row.Kind, row.ParentNativeID, row.Title, row.Preview, row.CreatedAt, row.UpdatedAt, b, row.Archived, "present", row.Interaction)
	if err != nil {
		t.Fatal(err)
	}
}
func (f *catalogStoreFixture) version(t *testing.T, version int64) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE catalog_state SET catalog_version=?`, version); err != nil {
		t.Fatal(err)
	}
}
func (f *catalogStoreFixture) corrupt(t *testing.T, native string) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE threads SET metadata_json='invalid-json' WHERE native_thread_id=?`, native); err != nil {
		t.Fatal(err)
	}
}

func (f *catalogStoreFixture) transcript(t *testing.T, id string, pages map[string]threadindex.TranscriptResponse, bodies map[string]history.TranscriptRetainedBody) {
	t.Helper()
	if id == "" {
		id = "nth_one"
	}
	var exists int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM threads WHERE id=?`, id).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists == 0 {
		f.row(t, sidebar.Thread{Thread: threadindex.Thread{ID: id, Provider: "codex", NativeID: id, Kind: "main"}})
	}
	path := filepath.Join(f.root.Root.Path, id+".jsonl")
	result, err := f.db.Exec(`INSERT INTO source_files (source_root_id,canonical_path,classification,status,parser_version) VALUES (?,?,'main','present','fixture')`, f.root.ID, path)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`INSERT INTO thread_sources(thread_id,source_file_id,identity_evidence) VALUES (?,?,'native_durable')`, id, sourceID); err != nil {
		t.Fatal(err)
	}
	var items []history.TranscriptItem
	seen := map[string]bool{}
	for _, page := range pages {
		for i, item := range page.Items {
			if seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			b, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			var stored history.TranscriptItem
			if err = json.Unmarshal(b, &stored); err != nil {
				t.Fatal(err)
			}
			stored.SourceKey = item.ID
			stored.SourceLineSHA256 = strings.Repeat("a", 64)
			stored.Status = history.ItemStatusCompleted
			if stored.ProjectedSeq == 0 {
				stored.ProjectedSeq = int64(i + 1)
			}
			if stored.Category == "" {
				stored.Category = history.CategoryMessages
			}
			if stored.Role == "" {
				if stored.Type == history.ItemUserMessage {
					stored.Role = history.RoleUser
				} else if stored.Type == history.ItemAgentMessage {
					stored.Role = history.RoleAssistant
				}
			}
			items = append(items, stored)
		}
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].ProjectedSeq < items[j].ProjectedSeq })
	turns := []history.TranscriptTurn{}
	turnAt := map[string]int{}
	for i := range items {
		if items[i].ProjectedSeq == 0 {
			items[i].ProjectedSeq = int64(i + 1)
		}
		if items[i].TurnKey == "" {
			items[i].TurnKey = fmt.Sprintf("turn-%d", i+1)
		}
		pos, ok := turnAt[items[i].TurnKey]
		if !ok {
			pos = len(turns)
			turnAt[items[i].TurnKey] = pos
			turns = append(turns, history.TranscriptTurn{Key: items[i].TurnKey, Ordinal: pos + 1, StartSeq: items[i].ProjectedSeq, Status: history.TurnCompleted, CompletionEvidence: history.CompletionTaskComplete})
		}
		items[i].TurnOrdinal = pos + 1
		turns[pos].EndSeq = items[i].ProjectedSeq
	}

	var retained []history.TranscriptBody
	for _, body := range bodies {
		retained = append(retained, history.TranscriptBody{ItemID: body.ItemID, ContentRef: body.ContentRef, Text: body.Text, OriginalBytes: body.OriginalBytes, StoredBytes: body.StoredBytes, Truncated: body.Truncated})
	}
	_, err = f.store.ApplyTranscript(context.Background(), history.TranscriptApplyRequest{Mode: history.TranscriptRebuild, ThreadID: id, SourceID: sourceID, SourceGeneration: 1, Provider: history.ProviderCodex, ParserVersion: "fixture", CompletionRuleVersion: "fixture", Result: history.TranscriptProjectionResult{ProjectionKey: "fixture-" + id, ParserState: json.RawMessage(`{}`), Items: items, Bodies: retained, Turns: turns}})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *catalogRowsFixture) source(t *testing.T) *sidebar.CatalogSource {
	t.Helper()
	if f.stored != nil {
		return f.stored.source
	}
	f.stored = newCatalogStoreFixture(t)
	for _, row := range f.page.Items {
		if row.UpdatedAt == nil {
			v := "2026-08-01T00:00:00Z"
			row.UpdatedAt = &v
		}
		f.stored.row(t, row)
	}
	var exists int
	if err := f.stored.db.QueryRow(`SELECT COUNT(*) FROM threads WHERE id=?`, f.thread.ID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists == 0 {
		f.stored.row(t, f.thread)
	}
	for _, row := range f.rescued {
		f.stored.row(t, row)
	}
	for key := range f.indexed {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) == 2 {
			var n int
			if err := f.stored.db.QueryRow(`SELECT COUNT(*) FROM threads WHERE provider=? AND native_thread_id=?`, parts[0], parts[1]).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				f.stored.row(t, sidebar.Thread{Thread: threadindex.Thread{ID: "nth_" + parts[1], Provider: parts[0], NativeID: parts[1], Kind: "subagent"}})
			}
		}
	}
	f.stored.version(t, f.page.CatalogVersion)
	if f.listErr != nil && f.listErr != threadindex.ErrInvalidThreadCatalogCursor {
		f.stored.store.Close()
	}
	return f.stored.source
}

func (f *fakeMessagesCatalog) source(t *testing.T) *sidebar.CatalogSource {
	t.Helper()
	if f.stored != nil {
		return f.stored.source
	}
	source := f.catalogRowsFixture.source(t)
	id := "nth_one"
	for _, page := range f.pages {
		if page.ThreadID != "" {
			id = page.ThreadID
			break
		}
	}
	f.stored.transcript(t, id, f.pages, f.bodies)
	if f.err != nil {
		if _, err := f.stored.db.Exec(`ALTER TABLE conversation_items RENAME TO fixture_private_items`); err != nil {
			t.Fatal(err)
		}
	}
	if f.bodyErr != nil {
		if _, err := f.stored.db.Exec(`ALTER TABLE conversation_item_bodies RENAME TO unavailable_body_items`); err != nil {
			t.Fatal(err)
		}
	}
	return source
}
func (f *fakeTranscriptCatalog) source(t *testing.T) *sidebar.CatalogSource {
	t.Helper()
	source := f.catalogRowsFixture.source(t)
	f.stored.transcript(t, f.response.ThreadID, map[string]threadindex.TranscriptResponse{"": f.response}, nil)
	return source
}
func (f *catalogEventFixture) source(t *testing.T) *sidebar.CatalogSource {
	t.Helper()
	source := f.catalogRowsFixture.source(t)
	f.stored.row(t, sidebar.Thread{Thread: threadindex.Thread{ID: "nth_changed", Provider: "codex", NativeID: "native-changed", Kind: "main"}})
	f.stored.version(t, f.version)
	return source
}
