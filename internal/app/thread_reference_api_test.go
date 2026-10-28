package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const threadReferenceUUID = "11111111-2222-4333-8444-555555555555"

func TestNativeRecoveryRetiredAliasKeepsHistoricalRouteWhileReplacementLive(t *testing.T) {
	s, store := newThreadReferenceServer(t, nil)
	record := createReferenceHostedRecord(t, store, threadReferenceUUID)
	const replacement = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const sessionID = "replacement-session"
	if err := store.bindSession(record.ID, sessionID, replacement, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if _, err := store.control.db.Exec(`CREATE TABLE IF NOT EXISTS hosted_native_recoveries(hosted_thread_id TEXT,old_native_id TEXT,new_native_id TEXT,state TEXT,record_json TEXT,client_recovery_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.control.db.Exec(`INSERT INTO hosted_native_recoveries(hosted_thread_id,old_native_id,new_native_id,state,record_json,client_recovery_id) VALUES(?,?,?,'activated','{}','reader-test')`, record.ID, threadReferenceUUID, replacement); err != nil {
		t.Fatal(err)
	}
	s.sessions[sessionID] = &session{id: sessionID,
		hostedThreadID:  record.ID,
		provider:        "codex",
		threadID:        replacement,
		agentExecutions: store,
		metadata:        store.control,
		createdAt:       time.Now(),
		lifecycleState:  testLifecycleSnapshot("ready", false)}
	status, body := resolveReference(t, s, threadReferenceUUID)
	if status != http.StatusOK || body.State != "resolved" || body.Target == nil || body.Target.Route != "historical" || body.Target.NativeThreadID != threadReferenceUUID || body.Target.HostedThreadID != record.ID {
		t.Fatalf("retired native must retain historical route while replacement is live: HTTP=%d response=%+v target=%+v", status, body, body.Target)
	}
	status, body = resolveReference(t, s, replacement)
	if status != http.StatusOK || body.Target == nil || body.Target.Route != "live" || body.Target.NativeThreadID != replacement || body.Target.HostedThreadID != record.ID {
		t.Fatalf("current native lost live preference: %d %+v", status, body.Target)
	}
}

func newThreadReferenceServer(t *testing.T, rows []catalogThread) (*server, *agentExecutionStore) {
	t.Helper()
	control := newTestControlMetadata(t)
	store, err := newControlAgentExecutionStore(control, "reference-test")
	if err != nil {
		t.Fatal(err)
	}
	return &server{
		catalog:         &fakeThreadCatalog{page: catalogPage{Items: rows}},
		agentExecutions: store,
		sessionRegistry: sessionRegistry{sessions: map[string]*session{}},
	}, store
}

func createReferenceHostedRecord(t *testing.T, store *agentExecutionStore, nativeID string) agentSessionRecord {
	t.Helper()
	record, _, err := store.beginSession("", "reference", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.bindSession(record.ID, "nano-"+record.ID, nativeID, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	updated, ok := store.session(record.ID)
	if !ok {
		t.Fatal("missing hosted record")
	}
	return updated
}

func resolveReference(t *testing.T, s *server, reference string) (int, threadReferenceResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/thread-references/"+reference, nil)
	applicationHandler(s).ServeHTTP(rec, req)
	var body threadReferenceResponse
	if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("invalid response %q", rec.Body.String())
	}
	return rec.Code, body
}

func TestThreadReferenceRejectsTruncatedNativeUUID(t *testing.T) {
	s, _ := newThreadReferenceServer(t, nil)
	status, _ := resolveReference(t, s, "11111111-2222-4333")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

func TestThreadReferenceCatalogCompatibilityInputResolvesExactRow(t *testing.T) {
	row := catalogThread{ID: "nth_exact", Provider: "codex", NativeID: threadReferenceUUID}
	s, _ := newThreadReferenceServer(t, nil)
	fake := s.catalog.(*fakeThreadCatalog)
	fake.thread = row

	status, body := resolveReference(t, s, row.ID)
	if status != http.StatusOK || body.State != "resolved" || body.Target == nil || body.Target.Route != "historical" {
		t.Fatalf("response = %d %#v", status, body)
	}
	if fake.loadID != row.ID || body.Target.CatalogThreadID != row.ID || body.Target.NativeThreadID != threadReferenceUUID {
		t.Fatalf("catalog target = %#v, loadID = %q", body.Target, fake.loadID)
	}
}

func TestThreadReferenceSameProviderDuplicateCatalogRowsUseCurrentOwner(t *testing.T) {
	rows := []catalogThread{
		{ID: "nth_first", Provider: "codex", NativeID: threadReferenceUUID},
		{ID: "nth_second", Provider: "codex", NativeID: threadReferenceUUID},
	}
	s, store := newThreadReferenceServer(t, rows)
	record := createReferenceHostedRecord(t, store, threadReferenceUUID)
	store.mu.Lock()
	store.sessions = map[string]agentSessionRecord{}
	store.mu.Unlock()

	status, body := resolveReference(t, s, threadReferenceUUID)
	if status != http.StatusOK || body.State != "resolved" || body.Target == nil || body.Target.HostedThreadID != record.ID {
		t.Fatalf("response = %d %#v", status, body)
	}
}

func TestThreadReferenceResultsAreNotStored(t *testing.T) {
	s, _ := newThreadReferenceServer(t, nil)
	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/thread-references/"+threadReferenceUUID, nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestThreadReferenceNativeLiveHostedContinuation(t *testing.T) {
	s, store := newThreadReferenceServer(t, nil)
	record := createReferenceHostedRecord(t, store, threadReferenceUUID)
	s.sessions["live-session"] = &session{id: "live-session",
		provider:       "codex",
		threadID:       threadReferenceUUID,
		hostedThreadID: record.ID,
		createdAt:      time.Now(),
		lifecycleState: testLifecycleSnapshot("ready", false)}
	store.mu.Lock()
	store.sessions = map[string]agentSessionRecord{}
	store.mu.Unlock()

	status, body := resolveReference(t, s, threadReferenceUUID)
	if status != http.StatusOK || body.State != "resolved" || body.Target == nil {
		t.Fatalf("response = %d %#v", status, body)
	}
	if body.Target.Route != "live" || body.Target.HostedThreadID != record.ID || body.Target.SessionID != "live-session" {
		t.Fatalf("target = %#v", body.Target)
	}
	status, body = resolveReference(t, s, record.ID)
	if status != http.StatusOK || body.State != "resolved" || body.Target == nil || body.Target.Route != "live" || body.Target.SessionID != "live-session" {
		t.Fatalf("hosted live response = %d %#v", status, body)
	}
}

func TestThreadReferenceNativeHistoricalFallbackAndHostedDirect(t *testing.T) {
	row := catalogThread{ID: "nth_exact", Provider: "codex", NativeID: threadReferenceUUID}
	s, store := newThreadReferenceServer(t, []catalogThread{row})
	record := createReferenceHostedRecord(t, store, threadReferenceUUID)
	store.mu.Lock()
	store.sessions = map[string]agentSessionRecord{}
	store.mu.Unlock()

	status, body := resolveReference(t, s, threadReferenceUUID)
	if status != http.StatusOK || body.State != "resolved" || body.Target == nil || body.Target.Route != "historical" {
		t.Fatalf("native response = %d %#v", status, body)
	}
	if body.Target.CatalogThreadID != row.ID || body.Target.HostedThreadID != record.ID || body.Reference != threadReferenceUUID {
		t.Fatalf("native target = %#v", body)
	}

	status, body = resolveReference(t, s, record.ID)
	if status != http.StatusOK || body.State != "resolved" || body.Target == nil || body.Target.Route != "historical" {
		t.Fatalf("hosted response = %d %#v", status, body)
	}
	if body.Target.CatalogThreadID != row.ID || body.Reference != record.ID {
		t.Fatalf("hosted target = %#v", body)
	}
}

func TestThreadReferenceActiveOwnerBeatsRetiredNativeHistory(t *testing.T) {
	s, store := newThreadReferenceServer(t, nil)
	first := createReferenceHostedRecord(t, store, threadReferenceUUID)
	if err := store.bindSession(first.ID, "nano-"+first.ID, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	current := createReferenceHostedRecord(t, store, threadReferenceUUID)
	store.mu.Lock()
	store.sessions = map[string]agentSessionRecord{}
	store.mu.Unlock()

	status, body := resolveReference(t, s, threadReferenceUUID)
	if status != http.StatusOK || body.State != "resolved" || body.Target == nil || body.Target.HostedThreadID != current.ID {
		t.Fatalf("response = %d %#v", status, body)
	}
}

func TestThreadReferenceCrossProviderNativeCollisionDoesNotGuess(t *testing.T) {
	s, store := newThreadReferenceServer(t, nil)
	_ = createReferenceHostedRecord(t, store, threadReferenceUUID)
	other, _, err := store.beginSession("", "reference-claude", "claude", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.bindSession(other.ID, "nano-"+other.ID, threadReferenceUUID, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.sessions = map[string]agentSessionRecord{}
	store.mu.Unlock()

	status, body := resolveReference(t, s, threadReferenceUUID)
	if status != http.StatusOK || body.State != "unresolved" || body.Reason != "ambiguous" || body.Target != nil {
		t.Fatalf("response = %d %#v", status, body)
	}
}

func TestThreadReferenceUnresolvedSchemaDistinguishesIndexing(t *testing.T) {
	unknown := "99999999-2222-4333-8444-555555555555"
	s, _ := newThreadReferenceServer(t, nil)
	s.catalog.(*fakeThreadCatalog).status = catalogIndexStatus{State: "scanning"}

	status, body := resolveReference(t, s, unknown)
	if status != http.StatusOK || body.State != "unresolved" || body.Reason != "not_indexed" || body.Target != nil {
		t.Fatalf("indexing response = %d %#v", status, body)
	}
	s.catalog.(*fakeThreadCatalog).status = catalogIndexStatus{State: "ready"}
	status, body = resolveReference(t, s, unknown)
	if status != http.StatusOK || body.State != "unresolved" || body.Reason != "unknown" || body.Target != nil {
		t.Fatalf("unknown response = %d %#v", status, body)
	}
}
