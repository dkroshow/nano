package hosted

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/setup"
	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/threadindex"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

const (
	identityNativeOld   = "11111111-aaaa-4bbb-8ccc-dddddddddddd"
	identityNativeNew   = "22222222-aaaa-4bbb-8ccc-dddddddddddd"
	threadReferenceUUID = "11111111-2222-4333-8444-555555555555"
)

func newIdentityService(t *testing.T, rows []catalogIdentity) (*IdentityService, *hostedstore.ExecutionStore) {
	service, store, _ := newIdentityFixture(t, rows)
	return service, store
}
func newIdentityFixture(t *testing.T, rows []catalogIdentity) (*IdentityService, *hostedstore.ExecutionStore, *history.Store) {
	t.Helper()
	control := newTestControlMetadata(t)
	migrateTestControl(t, control)
	executions, err := newControlAgentExecutionStore(control, "identity-test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	saved, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = saved.Close() })
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}
	catalog, err := threadindex.New(context.Background(), saved, threadindex.Options{Roots: []history.SourceRoot{root}, DisableWatcher: true, NativeIDProvisional: sessions.NativeThreadIDProvisional, ClaudeProjectSlug: setup.ClaudeProjectSlug})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(catalog.Close)
	// Startup writes ready asynchronously. Wait for that write before a test
	// injects another index state, or startup can overwrite its scanning state.
	waitForOwnerCatalog(t, func() bool {
		status, err := saved.IndexStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return len(status.Roots) == 1 && status.Roots[0].State == history.IndexReady
	})
	db, err := sql.Open("sqlite", statedb.URI(path, "rw"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	names := []history.ThreadNameRecord{}
	for i, row := range rows {
		source := root
		source.Provider = history.Provider(row.Provider)
		source.Path = filepath.Join(root.Path, fmt.Sprint(i))
		if row.Provider == "claude" {
			source.Kind = history.RootClaudeProjects
		}
		persisted, err := saved.EnsureRoot(context.Background(), source)
		if err != nil {
			t.Fatal(err)
		}
		id := row.ID
		if id == "" {
			id = fmt.Sprintf("nth_fixture_%d", i)
		}
		var updated any
		if row.UpdatedAt != "" {
			updated = row.UpdatedAt
		}
		_, err = db.Exec(`INSERT INTO threads(id,source_root_id,provider,native_thread_id,kind,updated_at,metadata_json,source_status) VALUES(?,?,?,?,?,?,?,?)`, id, persisted.ID, row.Provider, row.NativeID, "interactive", updated, `{}`, "present")
		if err != nil {
			t.Fatal(err)
		}
		if row.Name != "" {
			names = append(names, history.ThreadNameRecord{Provider: history.Provider(row.Provider), NativeThreadID: row.NativeID, Name: row.Name, Provenance: history.ThreadNameManual})
		}
	}
	if _, err = saved.SyncThreadNames(context.Background(), names); err != nil {
		t.Fatal(err)
	}
	return &IdentityService{Executions: executions, Catalog: catalog, CatalogErrors: CatalogErrors{NotFound: threadindex.ErrCatalogThreadNotFound}}, executions, saved
}
func newReferenceService(t *testing.T, rows []catalogIdentity) (*IdentityService, *hostedstore.ExecutionStore) {
	return newIdentityService(t, rows)
}
func createIdentityHosted(t *testing.T, store *hostedstore.ExecutionStore, provider, native string) ownedthreads.SessionRecord {
	t.Helper()
	record, _, err := fixtureBeginSession(store, "", "identity-"+native, provider, ownedthreads.PresentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if native != "" {
		if err := store.BindSession(record.ID, "nano-"+record.ID, native, "ready", ownedthreads.PresentationShared); err != nil {
			t.Fatal(err)
		}
	}
	updated, ok := store.Session(record.ID)
	if !ok {
		t.Fatal("missing hosted record")
	}
	return updated
}
func createReferenceHostedRecord(t *testing.T, store *hostedstore.ExecutionStore, native string) ownedthreads.SessionRecord {
	return createIdentityHosted(t, store, "codex", native)
}
func namedCatalog(value string) string  { return value }
func identityStamp(value string) string { return value }
func resolveIdentity(t *testing.T, s *IdentityService, ref string) (int, IdentityResolution, string) {
	t.Helper()
	response := httptest.NewRecorder()
	s.HandleIdentityResolve(response, httptest.NewRequest(http.MethodGet, "/api/v1/identity/resolve?ref="+ref, nil))
	var body IdentityResolution
	if response.Code == http.StatusOK && json.Unmarshal(response.Body.Bytes(), &body) != nil {
		t.Fatalf("invalid response %q", response.Body.String())
	}
	return response.Code, body, response.Body.String()
}
func resolveReference(t *testing.T, s *IdentityService, ref string) (int, threadReferenceResponse) {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/thread-references/"+ref, nil)
	request.SetPathValue("reference", ref)
	s.HandleThreadReference(response, request)

	var body threadReferenceResponse
	if response.Code == http.StatusOK && json.Unmarshal(response.Body.Bytes(), &body) != nil {
		t.Fatalf("invalid response %q", response.Body.String())
	}
	return response.Code, body
}
