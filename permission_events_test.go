package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func permissionTestSession(t *testing.T, provider, mode string) (*session, *controlMetadataStore) {
	t.Helper()
	store := newTestControlMetadata(t)
	hostedID := "nht_permission_test"
	if err := store.ensureHostedThread(hostedID, provider, presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	sess := &session{id: "nano-permission-test",
		provider:             provider,
		hostedThreadID:       hostedID,
		threadID:             "native-permission-test",
		permissionMode:       mode,
		metadata:             store,
		attachmentGeneration: 1,
		subs:                 map[chan wireEvent]struct{}{},
		interactions:         map[string]pendingInteraction{},
		lifecycleState:       testLifecycleSnapshot("ready", false)}
	return sess, store
}

func TestPermissionEventsPersistRequestAndResolutionWithoutCommandText(t *testing.T) {
	sess, store := permissionTestSession(t, "grok", "bypassPermissions")
	secret := "token-super-secret"
	p := pendingInteraction{
		ID: "request-7", Method: grokPermissionMethod, Kind: kindToolPermission,
		ThreadID: sess.threadID, ItemID: "call-7", CreatedAt: "2026-08-18T10:00:00Z",
		Params: json.RawMessage(`{"tool_name":"run_terminal_command","input":{"command":"echo ` + secret + `\nrg needle | head"}}`),
	}
	if err := sess.recordPermissionRequest(p); err != nil {
		t.Fatal(err)
	}
	// Replayed requests are idempotent.
	if err := sess.recordPermissionRequest(p); err != nil {
		t.Fatal(err)
	}
	if err := sess.recordPermissionResolution(p, decisionAccept, "nano_bypass", "autoApproved"); err != nil {
		t.Fatal(err)
	}
	page, err := store.permissionEventsAfter(sess.hostedThreadID, 0, 100)
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
	sess, store := permissionTestSession(t, "grok", "bypassPermissions")
	grok := &grokSession{sess: sess}
	params := json.RawMessage(`{
		"sessionId":"native-permission-test",
		"toolCall":{"toolCallId":"call-9","title":"Run command","rawInput":{"command":"echo safe"},
		"_meta":{"x.ai/tool":{"name":"run_terminal_command","label":"Run Command"}}},
		"options":[{"optionId":"allow-once","kind":"allow_once"},{"optionId":"reject-once","kind":"reject_once"}]
	}`)
	result, rpcErr := grok.handleServerRequest("9", grokPermissionMethod, params)
	if rpcErr != nil || result == protocol.Deferred {
		t.Fatalf("bypass result = %#v, rpc error = %v", result, rpcErr)
	}
	body, _ := json.Marshal(result)
	if !strings.Contains(string(body), `"optionId":"allow-once"`) {
		t.Fatalf("bypass response = %s", body)
	}
	if got := sess.pendingInteractionList(); len(got) != 0 {
		t.Fatalf("bypass parked an operator card: %#v", got)
	}
	page, err := store.permissionEventsAfter(sess.hostedThreadID, 0, 100)
	if err != nil || len(page.Events) != 2 || page.Events[1].DecisionSource != "nano_bypass" {
		t.Fatalf("audit page = %#v, error = %v", page, err)
	}
}

func TestGrokDefaultPermissionStillParksAndLogs(t *testing.T) {
	sess, store := permissionTestSession(t, "grok", "")
	grok := &grokSession{sess: sess}
	params := json.RawMessage(`{"sessionId":"native-permission-test","toolCall":{"toolCallId":"call-10","rawInput":{"command":"echo safe"},"_meta":{"x.ai/tool":{"name":"run_terminal_command"}}}}`)
	result, rpcErr := grok.handleServerRequest("10", grokPermissionMethod, params)
	if rpcErr != nil || result != protocol.Deferred {
		t.Fatalf("default result = %#v, rpc error = %v", result, rpcErr)
	}
	if got := sess.pendingInteractionList(); len(got) != 1 {
		t.Fatalf("default pending = %#v", got)
	}
	page, err := store.permissionEventsAfter(sess.hostedThreadID, 0, 100)
	if err != nil || len(page.Events) != 1 || page.Events[0].EventKind != permissionEventRequested {
		t.Fatalf("audit page = %#v, error = %v", page, err)
	}
}

func TestPermissionEventsAPIIsPagedByHostedThread(t *testing.T) {
	sess, store := permissionTestSession(t, "grok", "bypassPermissions")
	p := pendingInteraction{ID: "request-api", Method: grokPermissionMethod, Kind: kindToolPermission, Params: json.RawMessage(`{"tool_name":"read_file"}`)}
	if err := sess.recordPermissionRequest(p); err != nil {
		t.Fatal(err)
	}
	executions, err := newControlAgentExecutionStore(store, "test-run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executions.ensureSession(sess.hostedThreadID, "grok", presentationShared); err != nil {
		t.Fatal(err)
	}
	server := &server{sessions: map[string]*session{}, controlMetadata: store, agentExecutions: executions}
	rec := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/hosted-threads/"+sess.hostedThreadID+"/permission-events?limit=1", nil))
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

func TestPermissionInteractionClassification(t *testing.T) {
	tests := []struct {
		name string
		p    pendingInteraction
		want bool
	}{
		{"codex command", pendingInteraction{Kind: kindCommandApproval}, true},
		{"codex file", pendingInteraction{Kind: kindFileChangeApproval}, true},
		{"claude tool", pendingInteraction{Kind: kindToolPermission}, true},
		{"grok permission", pendingInteraction{Method: grokPermissionMethod}, true},
		{"grok question", pendingInteraction{Method: grokAskQuestionMethod, Kind: kindToolPermission}, false},
		{"user input", pendingInteraction{Kind: kindUserInput}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isPermissionInteraction(test.p); got != test.want {
				t.Fatalf("isPermissionInteraction = %v, want %v", got, test.want)
			}
		})
	}
}
