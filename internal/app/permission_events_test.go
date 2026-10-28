package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func permissionTestSession(t *testing.T, provider, mode string) (*providerRuntimeFixture, *controlMetadataStore) {
	t.Helper()
	store := newTestControlMetadata(t)
	hostedID := "nht_permission_test"
	if err := store.ensureHostedThread(hostedID, provider, presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	fixture := newProviderRuntimeFixture(t, sessionConfig{ID: "nano-permission-test", Provider: provider, HostedThreadID: hostedID, PermissionMode: mode, Metadata: store}, "native-permission-test", "")
	return fixture, store
}

func TestPermissionEventsPersistRequestAndResolutionWithoutCommandText(t *testing.T) {
	fixture, store := permissionTestSession(t, "grok", "bypassPermissions")
	sess := fixture.session
	secret := "token-super-secret"
	params, _ := json.Marshal(map[string]any{"sessionId": sess.ThreadID(), "toolCall": map[string]any{"toolCallId": "call-7", "rawInput": map[string]any{"command": "echo " + secret + "\nrg needle | head"}, "_meta": map[string]any{"x.ai/tool": map[string]any{"name": "run_terminal_command"}}}, "options": []any{map[string]any{"optionId": "allow-once", "kind": "allow_once"}}})
	for range 2 {
		fixture.grok.Send(map[string]any{"jsonrpc": "2.0", "id": "request-7", "method": "session/request_permission", "params": json.RawMessage(params)})
		if response := fixture.grok.Response(); len(response.Error) > 0 {
			t.Fatalf("permission request: %s", response.Error)
		}
	}
	page, err := store.permissionEventsAfter(sess.HostedThreadID(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("events = %#v, want request and resolution", page.Events)
	}
	request, resolution := page.Events[0], page.Events[1]
	if request.EventKind != permissionEventRequested || request.PermissionMode != "bypassPermissions" || request.ToolName != "run_terminal_command" {
		t.Fatalf("request = %#v", request)
	}
	if resolution.EventKind != permissionEventResolved || resolution.Decision != decisionAccept || resolution.DecisionSource != "nano_bypass" || resolution.Outcome != "autoApproved" {
		t.Fatalf("resolution = %#v", resolution)
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), secret) || !strings.Contains(request.RequestSummary, "2 lines") || request.RequestSHA256 == "" {
		t.Fatalf("permission audit leaked command or lost safe evidence: %s", encoded)
	}
}

func TestGrokBypassAutoApprovesEscapedPermissionAndAuditsIt(t *testing.T) {
	fixture, store := permissionTestSession(t, "grok", "bypassPermissions")
	sess := fixture.session
	params := json.RawMessage(`{
		"sessionId":"native-permission-test",
		"toolCall":{"toolCallId":"call-9","title":"Run command","rawInput":{"command":"echo safe"},
		"_meta":{"x.ai/tool":{"name":"run_terminal_command","label":"Run Command"}}},
		"options":[{"optionId":"allow-once","kind":"allow_once"},{"optionId":"reject-once","kind":"reject_once"}]
	}`)
	fixture.grok.Send(map[string]any{"jsonrpc": "2.0", "id": "9", "method": "session/request_permission", "params": params})
	response := fixture.grok.Response()
	if len(response.Error) > 0 {
		t.Fatalf("permission response: %s", response.Error)
	}
	body := response.Result
	if !strings.Contains(string(body), `"optionId":"allow-once"`) {
		t.Fatalf("bypass response = %s", body)
	}
	if got := sess.PendingInteractionList(); len(got) != 0 {
		t.Fatalf("bypass parked an operator card: %#v", got)
	}
	page, err := store.permissionEventsAfter(sess.HostedThreadID(), 0, 100)
	if err != nil || len(page.Events) != 2 || page.Events[1].DecisionSource != "nano_bypass" {
		t.Fatalf("audit page = %#v, error = %v", page, err)
	}
}

func TestGrokDefaultPermissionStillParksAndLogs(t *testing.T) {
	fixture, store := permissionTestSession(t, "grok", "")
	sess := fixture.session
	params := json.RawMessage(`{"sessionId":"native-permission-test","toolCall":{"toolCallId":"call-10","rawInput":{"command":"echo safe"},"_meta":{"x.ai/tool":{"name":"run_terminal_command"}}}}`)
	fixture.grok.Send(map[string]any{"jsonrpc": "2.0", "id": "10", "method": "session/request_permission", "params": params})
	waitForClaude(t, "parked permission", func() bool { return len(sess.PendingInteractionList()) == 1 })
	if got := sess.PendingInteractionList(); len(got) != 1 {
		t.Fatalf("default pending = %#v", got)
	}
	page, err := store.permissionEventsAfter(sess.HostedThreadID(), 0, 100)
	if err != nil || len(page.Events) != 1 || page.Events[0].EventKind != permissionEventRequested {
		t.Fatalf("audit page = %#v, error = %v", page, err)
	}
}

func TestPermissionEventsAPIIsPagedByHostedThread(t *testing.T) {
	fixture, store := permissionTestSession(t, "grok", "")
	sess := fixture.session
	fixture.grok.Send(map[string]any{"jsonrpc": "2.0", "id": "request-api", "method": "session/request_permission", "params": map[string]any{"sessionId": sess.ThreadID(), "toolCall": map[string]any{"toolCallId": "call-api", "_meta": map[string]any{"x.ai/tool": map[string]any{"name": "read_file"}}}}})
	waitForClaude(t, "persisted permission request", func() bool {
		page, err := store.permissionEventsAfter(sess.HostedThreadID(), 0, 100)
		return err == nil && len(page.Events) == 1
	})
	executions, err := newControlAgentExecutionStore(store, "test-run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executions.ensureSession(sess.HostedThreadID(), "grok", presentationShared); err != nil {
		t.Fatal(err)
	}
	server := &server{Registry: newSessionRegistry(), controlMetadata: store, agentExecutions: executions}
	rec := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/hosted-threads/"+sess.HostedThreadID()+"/permission-events?limit=1", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"eventKind":"requested"`) || !strings.Contains(rec.Body.String(), `"nextCursor":`) {
		t.Fatalf("permission events API = %d %s", rec.Code, rec.Body.String())
	}
}

func TestControlSchemaTenMigratesPermissionAuditTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sqlite")
	store, err := openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE hosted_permission_events; PRAGMA user_version = 10`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != controlSchemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	var table string
	if err := store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='hosted_permission_events'`).Scan(&table); err != nil || table == "" {
		t.Fatalf("permission audit table = %q, %v", table, err)
	}
}
