package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/threadindex"
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

func newIdentityServer(t *testing.T, rows []catalogThread) (*server, *agentExecutionStore) {
	t.Helper()
	control := newTestControlMetadata(t)
	migrateTestControl(t, control)
	store, err := newControlAgentExecutionStore(control, "identity-test")
	if err != nil {
		t.Fatal(err)
	}
	return &server{
		catalog:         &fakeThreadCatalog{page: catalogPage{Items: rows}},
		agentExecutions: store,
		Registry:        newSessionRegistry(),
	}, store
}

func createIdentityHosted(t *testing.T, store *agentExecutionStore, provider, nativeID string) agentSessionRecord {
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

func resolveIdentity(t *testing.T, s *server, ref string) (int, identityResolveResponse, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/identity/resolve?ref="+ref, nil)
	s.handleIdentityResolve(rec, req)
	var body identityResolveResponse
	if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("invalid response %q", rec.Body.String())
	}
	return rec.Code, body, rec.Body.String()
}

func TestIdentityResolveFollowsRename(t *testing.T) {
	row := catalogThread{Thread: threadindex.Thread{ID: "nth_renamed", Provider: "codex", NativeID: identityNativeOld, Name: namedCatalog("old name"), UpdatedAt: identityStamp("2026-08-20T12:00:00Z")}}
	s, store := newIdentityServer(t, []catalogThread{row})
	record := createIdentityHosted(t, store, "codex", identityNativeOld)

	status, body, raw := resolveIdentity(t, s, "old%20name")
	if status != http.StatusOK || body.HostedThreadID != record.ID {
		t.Fatalf("old name = %d %#v (%s)", status, body, raw)
	}

	s.catalog.(*fakeThreadCatalog).page.Items[0].Name = namedCatalog("new name")
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
