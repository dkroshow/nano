package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sidebar"
	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/threads"
)

func TestNativeRecoveryIdentityAliasSurvives(t *testing.T) {

	control, executions, record := retiredRecoveryFixture(t)
	s := &server{controlMetadata: control, agentExecutions: executions}
	for _, ref := range []string{record.ID, "recovery-old", "recovery-new"} {
		code, body, raw := resolveIdentity(t, s, ref)
		if code != http.StatusOK || body.HostedThreadID != record.ID || body.NativeThreadID != "recovery-new" {
			t.Fatalf("ref=%s code=%d body=%s", ref, code, raw)
		}
	}
}

const (
	identityNativeOld = "11111111-aaaa-4bbb-8ccc-dddddddddddd"
	identityNativeNew = "22222222-aaaa-4bbb-8ccc-dddddddddddd"
)

func newIdentityServer(t *testing.T, rows []sidebar.Thread) (*server, *hostedstore.ExecutionStore) {
	t.Helper()
	control := newTestControlMetadata(t)
	migrateTestControl(t, control)
	store, err := newControlAgentExecutionStore(control, "identity-test")
	if err != nil {
		t.Fatal(err)
	}
	return &server{
		catalog:         fixtureIdentityCatalog(t, rows),
		agentExecutions: store,
		Registry:        newSessionRegistry(),
	}, store
}

func createIdentityHosted(t *testing.T, store *hostedstore.ExecutionStore, provider, nativeID string) threads.SessionRecord {
	t.Helper()
	record, _, err := fixtureBeginSession(store, "", "identity-"+nativeID, provider, presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if nativeID != "" {
		if err := store.BindSession(record.ID, "nano-"+record.ID, nativeID, "ready", presentationShared); err != nil {
			t.Fatal(err)
		}
	}
	updated, ok := store.Session(record.ID)
	if !ok {
		t.Fatal("missing hosted record")
	}
	return updated
}

func namedCatalog(value string) *threadindex.ThreadName {
	return &threadindex.ThreadName{Value: value, Provenance: "manual", Evidence: threadindex.ThreadNameEvidence}
}

func identityStamp(value string) *string { return &value }

func resolveIdentity(t *testing.T, s *server, ref string) (int, hostedmodel.IdentityResolution, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/identity/resolve?ref="+ref, nil)
	s.hostedIdentity().HandleIdentityResolve(rec, req)
	var body hostedmodel.IdentityResolution
	if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("invalid response %q", rec.Body.String())
	}
	return rec.Code, body, rec.Body.String()
}

func TestIdentityResolveFollowsRename(t *testing.T) {
	row := sidebar.Thread{Thread: threadindex.Thread{ID: "nth_renamed", Provider: "codex", NativeID: identityNativeOld, Name: namedCatalog("old name"), UpdatedAt: identityStamp("2026-08-20T12:00:00Z")}}
	s, store := newIdentityServer(t, []sidebar.Thread{row})
	record := createIdentityHosted(t, store, "codex", identityNativeOld)

	status, body, raw := resolveIdentity(t, s, "old%20name")
	if status != http.StatusOK || body.HostedThreadID != record.ID {
		t.Fatalf("old name = %d %#v (%s)", status, body, raw)
	}

	if err := s.catalog.(*threadCatalogService).AssignThreadName("codex", identityNativeOld, "new name"); err != nil {
		t.Fatal(err)
	}
	waitForCatalog(t, func() bool {
		name, _ := s.catalog.(*threadCatalogService).ThreadNameFor(context.Background(), "codex", identityNativeOld)
		return name == "new name"
	})
	status, _, raw = resolveIdentity(t, s, "old%20name")
	if status != http.StatusNotFound {
		t.Fatalf("stale name still resolved: %d (%s)", status, raw)
	}
	status, body, raw = resolveIdentity(t, s, "new%20name")
	if status != http.StatusOK || body.DisplayName != "new name" || body.HostedThreadID != record.ID {
		t.Fatalf("new name = %d %#v (%s)", status, body, raw)
	}
	status, body, raw = resolveIdentity(t, s, record.ID)
	if status != http.StatusOK || body.DisplayName != "new name" {
		t.Fatalf("hosted after rename = %d %#v (%s)", status, body, raw)
	}
}

func TestIdentityResolveLivePendingName(t *testing.T) {
	s, store := newIdentityServer(t, nil)
	record := createIdentityHosted(t, store, "claude", "")
	s.RegisterSession(configuredFixtureSession(t, sessionConfig{ID: "live", Provider: "claude", HostedThreadID: record.ID, DisplayName: "draft name", CreatedAt: time.Now()}, "", "ready", ""))

	status, body, raw := resolveIdentity(t, s, "draft%20name")
	if status != http.StatusOK || body.HostedThreadID != record.ID || body.DisplayName != "draft name" {
		t.Fatalf("pending name = %d %#v (%s)", status, body, raw)
	}
}

// fixtureIdentityCatalog retains the real name and exact-row query boundaries.
func fixtureIdentityCatalog(t *testing.T, rows []sidebar.Thread) *threadCatalogService {
	service, _ := fixtureIdentityCatalogStore(t, rows)
	return service
}
func fixtureIdentityCatalogStore(t *testing.T, rows []sidebar.Thread) (*threadCatalogService, *history.Store) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "catalog.sqlite")
	saved, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = saved.Close() })
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: directory}
	service, err := newThreadCatalogService(context.Background(), saved, threadindex.Options{Roots: []history.SourceRoot{root}, NamesPath: filepath.Join(directory, "names.json"), DisableWatcher: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	db, err := sql.Open("sqlite", statedb.URI(path, "rw"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, row := range rows {
		if row.ID != "" {
			source := root
			source.Provider = history.Provider(row.Provider)
			source.Path = filepath.Join(directory, fmt.Sprint(i))
			if row.Provider == "claude" {
				source.Kind = history.RootClaudeProjects
			}
			persisted, err := saved.EnsureRoot(context.Background(), source)
			if err != nil {
				t.Fatal(err)
			}
			var updated any
			if row.UpdatedAt != nil {
				updated = *row.UpdatedAt
			}
			_, err = db.Exec(`INSERT INTO threads(id,source_root_id,provider,native_thread_id,kind,updated_at,metadata_json,source_status) VALUES(?,?,?,?,?,?,?,?)`, row.ID, persisted.ID, row.Provider, row.NativeID, "interactive", updated, `{}`, "present")
			if err != nil {
				t.Fatal(err)
			}
		}
		if row.Name != nil {
			if err := service.AssignThreadName(row.Provider, row.NativeID, row.Name.Value); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := service.Service.PublishCreationNames(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	return service, saved
}
func fixtureNamedCatalog(t *testing.T, provider, native, name string) *threadCatalogService {
	return fixtureIdentityCatalog(t, []sidebar.Thread{{Thread: threadindex.Thread{Provider: provider, NativeID: native, Name: namedCatalog(name)}}})
}
