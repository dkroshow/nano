package threadindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// newNamedCatalogService builds a ticker-disabled service over a temp root with
// the given native thread ids as fixtures and a temp-dir truth file (never the
// real ~/.nanod files). It returns the service, the nano id for each
// native id, and the truth-file path.
func newNamedCatalogService(t *testing.T, nativeIDs ...string) (*Service, map[string]string, string) {
	t.Helper()
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for i, nativeID := range nativeIDs {
		writeClaudeCatalogFixture(t, filepath.Join(projectPath, nativeID+".jsonl"), nativeID,
			fmt.Sprintf("2026-07-21T10:0%d:00Z", i), "Rename target "+nativeID)
	}
	namesPath := filepath.Join(t.TempDir(), "thread-names.json")
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots:     []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}},
		NamesPath: namesPath, DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	service.RequestRefresh() // the explicit discovery a new store needs

	ids := make(map[string]string, len(nativeIDs))
	waitForCatalog(t, func() bool {
		page, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 50})
		if err != nil || len(page.Items) != len(nativeIDs) {
			return false
		}
		for _, item := range page.Items {
			ids[item.NativeID] = item.ID
		}
		return len(ids) == len(nativeIDs)
	})
	return service, ids, namesPath
}

func TestRenameEndpointWritesTruthFileAndConverges(t *testing.T) {
	nativeID := "77777777-7777-4777-8777-777777777777"
	service, ids, namesPath := newNamedCatalogService(t, nativeID)
	threadID := ids[nativeID]

	// Leading/trailing whitespace is trimmed; the response record is the new name.
	record, err := service.RenameThread(context.Background(), threadID, "  graph classifier  ")
	if err != nil {
		t.Fatal(err)
	}
	if record.Value != "graph classifier" || record.Provenance != "manual" || record.Evidence != "user_assigned" {
		t.Fatalf("response record = %#v", record)
	}

	// The truth file is the single write: one record, manual provenance, a set
	// updatedAt, valid under the loader (byte-exact format is covered by the
	// history-package writer test).
	written, err := history.LoadThreadNames(namesPath)
	if err != nil || len(written) != 1 {
		t.Fatalf("load written truth file: %d records, %v", len(written), err)
	}
	got := written[0]
	if got.Provider != history.ProviderClaude || got.NativeThreadID != nativeID ||
		got.Name != "graph classifier" || got.Provenance != history.ThreadNameManual || got.UpdatedAt == nil {
		t.Fatalf("written record = %#v", got)
	}
	raw, _ := os.ReadFile(namesPath)
	if !strings.HasSuffix(string(raw), "}\n") || !strings.Contains(string(raw), "\n  \"names\": [") {
		t.Fatalf("truth file is not 2-space-indented with a trailing newline:\n%s", raw)
	}

	// RequestRefresh (fired by RenameThread) drives the scan → sync → overlay
	// chain without a manual poll.
	waitForCatalog(t, func() bool {
		page, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
		return err == nil && len(page.Items) == 1 && page.Items[0].Name != nil &&
			page.Items[0].Name.Value == "graph classifier" && page.Items[0].Name.Provenance == "manual"
	})
}

func TestThreadNameRefForPreservesTruthProvenance(t *testing.T) {
	nativeID := "66666666-6666-4666-8666-666666666666"
	service, _, _ := newNamedCatalogService(t, nativeID)
	now := time.Now().UTC()
	for _, provenance := range []history.ThreadNameProvenance{history.ThreadNameManual, history.ThreadNameHistoricalImport} {
		t.Run(string(provenance), func(t *testing.T) {
			changed, err := service.store.SyncThreadNames(context.Background(), []history.ThreadNameRecord{{
				Provider: history.ProviderClaude, NativeThreadID: nativeID, Name: "durable name", Provenance: provenance, UpdatedAt: &now,
			}})
			if err != nil || !changed {
				t.Fatalf("sync name provenance %s: changed=%v err=%v", provenance, changed, err)
			}
			ref, ok := service.ThreadNameRefFor(context.Background(), "claude", nativeID)
			if !ok || ref == nil || ref.Value != "durable name" || ref.Provenance != provenance {
				t.Fatalf("ThreadNameRefFor = %+v, %v", ref, ok)
			}
		})
	}
}

func TestCreationNameProjectionRanksBelowImportedAndManualTruth(t *testing.T) {
	nativeID := "55555555-5555-4555-8555-555555555555"
	service, ids, namesPath := newNamedCatalogService(t, nativeID)
	stamp := time.Date(2026, 9, 11, 14, 0, 0, 0, time.UTC)
	service.SetCreationNameSource(func(context.Context) ([]history.ThreadNameRecord, error) {
		return []history.ThreadNameRecord{{
			Provider: history.ProviderClaude, NativeThreadID: nativeID,
			Name: "creation title", Provenance: history.ThreadNameCreation, UpdatedAt: &stamp,
		}}, nil
	})
	expected := []CreationNameRecord{{Provider: "claude", NativeThreadID: nativeID, Name: "creation title"}}
	if err := service.PublishCreationNames(context.Background(), expected); err != nil {
		t.Fatal(err)
	}
	ref, ok := service.ThreadNameRefFor(context.Background(), "claude", nativeID)
	if !ok || ref.Value != "creation title" || ref.Provenance != history.ThreadNameCreation {
		t.Fatalf("creation projection = %+v, %v", ref, ok)
	}
	if truth, err := history.LoadThreadNames(namesPath); err != nil || len(truth) != 0 {
		t.Fatalf("creation fact leaked into manual names file: %+v, %v", truth, err)
	}

	imported := fmt.Sprintf(`{"version":1,"names":[{"provider":"claude","nativeThreadId":%q,"name":"imported title","provenance":"imported"}]}`, nativeID)
	if err := os.WriteFile(namesPath, []byte(imported), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.PublishCreationNames(context.Background(), expected); err != nil {
		t.Fatal(err)
	}
	ref, ok = service.ThreadNameRefFor(context.Background(), "claude", nativeID)
	if !ok || ref.Value != "imported title" || ref.Provenance != history.ThreadNameHistoricalImport {
		t.Fatalf("imported precedence = %+v, %v", ref, ok)
	}

	if _, err := service.RenameThread(context.Background(), ids[nativeID], "operator title"); err != nil {
		t.Fatal(err)
	}
	if err := service.PublishCreationNames(context.Background(), expected); err != nil {
		t.Fatal(err)
	}
	ref, ok = service.ThreadNameRefFor(context.Background(), "claude", nativeID)
	if !ok || ref.Value != "operator title" || ref.Provenance != history.ThreadNameManual {
		t.Fatalf("manual precedence = %+v, %v", ref, ok)
	}
}

// Boot resumes every named thread, and each publishes a creation name the
// previous run already projected. While another refresh holds the names lock,
// such a publish must return at once: queued for a full refresh of its own,
// it held a Claude boot handshake slot behind every other thread's refresh.
func TestPublishingAnAlreadyProjectedCreationNameSkipsTheNamesRefresh(t *testing.T) {
	nativeID := "66666666-6666-4666-8666-666666666666"
	service, _, _ := newNamedCatalogService(t, nativeID)
	stamp := time.Date(2026, 10, 8, 6, 0, 0, 0, time.UTC)
	var hold atomic.Bool
	entered, release := make(chan struct{}, 1), make(chan struct{})
	service.SetCreationNameSource(func(context.Context) ([]history.ThreadNameRecord, error) {
		if hold.Load() {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
		return []history.ThreadNameRecord{{
			Provider: history.ProviderClaude, NativeThreadID: nativeID,
			Name: "creation title", Provenance: history.ThreadNameCreation, UpdatedAt: &stamp,
		}}, nil
	})
	expected := []CreationNameRecord{{Provider: "claude", NativeThreadID: nativeID, Name: "creation title"}}
	if err := service.PublishCreationNames(context.Background(), expected); err != nil {
		t.Fatal(err)
	}

	hold.Store(true)
	refreshed := make(chan error, 1)
	go func() { refreshed <- service.refreshThreadNames(context.Background()) }()
	<-entered
	published := make(chan error, 1)
	go func() { published <- service.PublishCreationNames(context.Background(), expected) }()
	select {
	case err := <-published:
		close(release)
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		<-published
		t.Fatal("publishing an already-projected creation name waited for another thread's names refresh")
	}
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
}

func TestClearEndpointRemovesRecordAndConverges(t *testing.T) {
	nativeID := "88888888-8888-4888-8888-888888888888"
	service, ids, namesPath := newNamedCatalogService(t, nativeID)
	threadID := ids[nativeID]

	if _, err := service.RenameThread(context.Background(), threadID, "temporary"); err != nil {
		t.Fatal(err)
	}
	waitForCatalog(t, func() bool {
		page, _ := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
		return len(page.Items) == 1 && page.Items[0].Name != nil
	})

	if err := service.ClearThreadName(context.Background(), threadID); err != nil {
		t.Fatal(err)
	}
	if written, err := history.LoadThreadNames(namesPath); err != nil || len(written) != 0 {
		t.Fatalf("clear left records behind: %d, %v", len(written), err)
	}
	waitForCatalog(t, func() bool {
		page, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
		return err == nil && len(page.Items) == 1 && page.Items[0].Name == nil
	})
}

func TestNamedThreadsExactMatch(t *testing.T) {
	first := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	second := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	service, ids, _ := newNamedCatalogService(t, first, second)
	if _, err := service.RenameThread(context.Background(), ids[first], "shared label"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RenameThread(context.Background(), ids[second], "shared label"); err != nil {
		t.Fatal(err)
	}
	waitForCatalog(t, func() bool {
		rows, err := service.NamedThreads(context.Background(), "shared label")
		return err == nil && len(rows) == 2
	})
	rows, err := service.NamedThreads(context.Background(), "shared label")
	if err != nil || len(rows) != 2 {
		t.Fatalf("NamedThreads = %#v, %v", rows, err)
	}
	missing, err := service.NamedThreads(context.Background(), "no such name")
	if err != nil || len(missing) != 0 {
		t.Fatalf("NamedThreads absent = %#v, %v", missing, err)
	}
}

func TestRenameUnknownThreadIsNotFound(t *testing.T) {
	service, _, _ := newNamedCatalogService(t, "99999999-9999-4999-8999-999999999999")
	if _, err := service.RenameThread(context.Background(), "nth_does_not_exist", "x"); !errors.Is(err, ErrCatalogThreadNotFound) {
		t.Fatalf("rename unknown id error = %v", err)
	}
	if err := service.ClearThreadName(context.Background(), "nth_does_not_exist"); !errors.Is(err, ErrCatalogThreadNotFound) {
		t.Fatalf("clear unknown id error = %v", err)
	}
}

func TestRenameValidationErrors(t *testing.T) {
	nativeID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	service, ids, namesPath := newNamedCatalogService(t, nativeID)
	threadID := ids[nativeID]
	for _, name := range []string{"", "   ", strings.Repeat("x", 201)} {
		if _, err := service.RenameThread(context.Background(), threadID, name); !errors.Is(err, errInvalidThreadName) {
			t.Fatalf("name %q error = %v", name, err)
		}
	}
	// A rejected rename must not have created the truth file.
	if _, err := os.Stat(namesPath); !os.IsNotExist(err) {
		t.Fatalf("validation error still wrote the truth file: %v", err)
	}
}

func TestRenameRefusesMalformedTruthFileAndLeavesItByteIdentical(t *testing.T) {
	nativeID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	service, ids, namesPath := newNamedCatalogService(t, nativeID)
	threadID := ids[nativeID]

	malformed := `{"version":1,"names":[`
	if err := os.WriteFile(namesPath, []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RenameThread(context.Background(), threadID, "x"); !errors.Is(err, history.ErrThreadNamesMalformed) {
		t.Fatalf("rename over malformed file error = %v", err)
	}
	raw, err := os.ReadFile(namesPath)
	if err != nil || string(raw) != malformed {
		t.Fatalf("nano rewrote the malformed truth file: %q, %v", string(raw), err)
	}
}

func TestConcurrentRenamesSerializeWithoutLostUpdates(t *testing.T) {
	nativeA := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	nativeB := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	service, ids, namesPath := newNamedCatalogService(t, nativeA, nativeB)

	var wg sync.WaitGroup
	for nativeID, name := range map[string]string{nativeA: "alpha", nativeB: "beta"} {
		wg.Add(1)
		go func(id, value string) {
			defer wg.Done()
			if _, err := service.RenameThread(context.Background(), ids[id], value); err != nil {
				t.Errorf("concurrent rename %s: %v", value, err)
			}
		}(nativeID, name)
	}
	wg.Wait()

	// Without the read-modify-write mutex one write would clobber the other; both
	// records must survive.
	written, err := history.LoadThreadNames(namesPath)
	if err != nil || len(written) != 2 {
		t.Fatalf("expected both records, got %d: %v", len(written), err)
	}
	byNative := map[string]string{}
	for _, record := range written {
		byNative[record.NativeThreadID] = record.Name
	}
	if byNative[nativeA] != "alpha" || byNative[nativeB] != "beta" {
		t.Fatalf("lost update: %#v", byNative)
	}
}

// TestSameSecondSameLengthRewriteIsDetected proves the service's change-detection
// signature uses ModTime().UnixNano(): a same-byte-length rewrite within the same
// wall-clock second must still resync the derived overlay.
func TestSameSecondSameLengthRewriteIsDetected(t *testing.T) {
	nativeID := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	service, _, namesPath := newNamedCatalogService(t, nativeID)

	snapshot := func(name string) string {
		return fmt.Sprintf(`{"version":1,"names":[{"provider":"claude","nativeThreadId":%q,"name":%q,"provenance":"manual","updatedAt":"2026-07-21T18:00:00Z"}]}`, nativeID, name)
	}
	first, second := snapshot("aaaa"), snapshot("bbbb")
	if len(first) != len(second) {
		t.Fatalf("test fixtures must be equal length: %d vs %d", len(first), len(second))
	}
	base := time.Date(2026, 7, 21, 18, 0, 0, 100_000_000, time.UTC)

	if err := os.WriteFile(namesPath, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(namesPath, base, base); err != nil {
		t.Fatal(err)
	}
	service.RequestRefresh()
	waitForCatalog(t, func() bool {
		page, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
		return err == nil && len(page.Items) == 1 && page.Items[0].Name != nil && page.Items[0].Name.Value == "aaaa"
	})

	// Same size, same wall-clock second, sub-second mtime difference only.
	sameSecond := base.Add(500 * time.Millisecond)
	if err := os.WriteFile(namesPath, []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(namesPath, sameSecond, sameSecond); err != nil {
		t.Fatal(err)
	}
	service.RequestRefresh()
	waitForCatalog(t, func() bool {
		page, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
		return err == nil && len(page.Items) == 1 && page.Items[0].Name != nil && page.Items[0].Name.Value == "bbbb"
	})
}

func TestThreadNameHTTPStatusCodes(t *testing.T) {
	nativeID := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	service, ids, namesPath := newNamedCatalogService(t, nativeID)
	threadID := ids[nativeID]
	s := NewHandlers(service)

	call := func(method, id, body string) *httptest.ResponseRecorder {
		var reader *strings.Reader
		if body != "" {
			reader = strings.NewReader(body)
		} else {
			reader = strings.NewReader("")
		}
		req := httptest.NewRequest(method, "/api/v1/threads/"+id+"/name", reader)
		rec := httptest.NewRecorder()
		catalogTestHandler(s).ServeHTTP(rec, req)
		return rec
	}

	// PUT valid → 200 with {"name": {...}}.
	rec := call(http.MethodPut, threadID, `{"name":"named via http"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Name ThreadName `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Name.Value != "named via http" || body.Name.Provenance != "manual" {
		t.Fatalf("PUT body = %s, %v", rec.Body.String(), err)
	}

	// PUT empty name → 400 invalid_name (the endpoint requires non-empty; the UI
	// sends DELETE to clear).
	if rec := call(http.MethodPut, threadID, `{"name":"   "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty PUT status = %d", rec.Code)
	}

	// DELETE → 204.
	if rec := call(http.MethodDelete, threadID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", rec.Code)
	}

	// Unknown id → 404 thread_not_found.
	if rec := call(http.MethodPut, "nth_missing", `{"name":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown-id status = %d", rec.Code)
	}

	// GET is not allowed on the name resource → 405.
	if rec := call(http.MethodGet, threadID, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", rec.Code)
	}

	// Malformed truth file → 409 names_file_malformed.
	if err := os.WriteFile(namesPath, []byte(`{"version":1,"names":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	rec = call(http.MethodPut, threadID, `{"name":"x"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("malformed-file status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil || errBody.Error.Code != "names_file_malformed" {
		t.Fatalf("malformed error body = %s, %v", rec.Body.String(), err)
	}
}
