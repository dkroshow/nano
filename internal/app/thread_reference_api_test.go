package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threads"
)

// The integration fixture decodes the public wire contract independently of
// the hosted controller's private response values.
type threadReferenceTarget struct {
	Route           string `json:"route"`
	HostedThreadID  string `json:"hostedThreadId,omitempty"`
	SessionID       string `json:"sessionId,omitempty"`
	CatalogThreadID string `json:"catalogThreadId,omitempty"`
	Provider        string `json:"provider,omitempty"`
	NativeThreadID  string `json:"nativeThreadId,omitempty"`
}
type threadReferenceResponse struct {
	Reference string                 `json:"reference"`
	State     string                 `json:"state"`
	Reason    string                 `json:"reason,omitempty"`
	Target    *threadReferenceTarget `json:"target,omitempty"`
}

const threadReferenceUUID = "11111111-2222-4333-8444-555555555555"

func TestNativeRecoveryRetiredAliasKeepsHistoricalRouteWhileReplacementLive(t *testing.T) {
	s, store := newThreadReferenceServer(t, nil)
	record := createReferenceHostedRecord(t, store, threadReferenceUUID)
	const replacement = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const sessionID = "replacement-session"
	if err := store.BindSession(record.ID, sessionID, replacement, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Control().CanonicalDatabase().Exec(`CREATE TABLE IF NOT EXISTS hosted_native_recoveries(hosted_thread_id TEXT,old_native_id TEXT,new_native_id TEXT,state TEXT,record_json TEXT,client_recovery_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Control().CanonicalDatabase().Exec(`INSERT INTO hosted_native_recoveries(hosted_thread_id,old_native_id,new_native_id,state,record_json,client_recovery_id) VALUES(?,?,?,'activated','{}','reader-test')`, record.ID, threadReferenceUUID, replacement); err != nil {
		t.Fatal(err)
	}
	s.RegisterSession(configuredFixtureSession(t, sessionConfig{ID: sessionID, HostedThreadID: record.ID, Provider: "codex", AgentExecutions: store, Metadata: store.Control(), CreatedAt: time.Now()}, replacement, "ready", ""))
	status, body := resolveReference(t, s, threadReferenceUUID)
	if status != http.StatusOK || body.State != "resolved" || body.Target == nil || body.Target.Route != "historical" || body.Target.NativeThreadID != threadReferenceUUID || body.Target.HostedThreadID != record.ID {
		t.Fatalf("retired native must retain historical route while replacement is live: HTTP=%d response=%+v target=%+v", status, body, body.Target)
	}
	status, body = resolveReference(t, s, replacement)
	if status != http.StatusOK || body.Target == nil || body.Target.Route != "live" || body.Target.NativeThreadID != replacement || body.Target.HostedThreadID != record.ID {
		t.Fatalf("current native lost live preference: %d %+v", status, body.Target)
	}
}

func newThreadReferenceServer(t *testing.T, rows []sidebar.Thread) (*server, *hostedstore.ExecutionStore) {
	t.Helper()
	control := newTestControlMetadata(t)
	store, err := newControlAgentExecutionStore(control, "reference-test")
	if err != nil {
		t.Fatal(err)
	}
	return &server{
		catalog:         fixtureIdentityCatalog(t, rows),
		agentExecutions: store,
		Registry:        newSessionRegistry(),
	}, store
}

func createReferenceHostedRecord(t *testing.T, store *hostedstore.ExecutionStore, nativeID string) threads.SessionRecord {
	t.Helper()
	record, _, err := fixtureBeginSession(store, "", "reference", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(record.ID, "nano-"+record.ID, nativeID, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	updated, ok := store.Session(record.ID)
	if !ok {
		t.Fatal("missing hosted record")
	}
	return updated
}

func resolveReference(t *testing.T, s *server, reference string) (int, threadReferenceResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/thread-references/"+reference, nil)
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, req)
	var body threadReferenceResponse
	if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("invalid response %q", rec.Body.String())
	}
	return rec.Code, body
}

func TestThreadReferenceNativeLiveHostedContinuation(t *testing.T) {
	s, store := newThreadReferenceServer(t, nil)
	s.agentExecutions, _ = newControlAgentExecutionStore(store.Control(), "empty-reader")
	record := createReferenceHostedRecord(t, store, threadReferenceUUID)
	s.RegisterSession(configuredFixtureSession(t, sessionConfig{ID: "live-session", Provider: "codex", HostedThreadID: record.ID, CreatedAt: time.Now()}, threadReferenceUUID, "ready", ""))

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
