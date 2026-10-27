package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
		sessions:        map[string]*session{},
	}, store
}

func createIdentityHosted(t *testing.T, store *agentExecutionStore, provider, nativeID string) agentSessionRecord {
	t.Helper()
	record, _, err := store.beginSession("", "identity-"+nativeID, provider, presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if nativeID != "" {
		if err := store.bindSession(record.ID, "nano-"+record.ID, nativeID, "ready", presentationShared); err != nil {
			t.Fatal(err)
		}
	}
	updated, ok := store.session(record.ID)
	if !ok {
		t.Fatal("missing hosted record")
	}
	return updated
}

func namedCatalog(value string) *catalogThreadName {
	return &catalogThreadName{Value: value, Provenance: "manual", Evidence: threadNameEvidence}
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

func TestIdentityResolveRejectsMissingRef(t *testing.T) {
	s, _ := newIdentityServer(t, nil)
	rec := httptest.NewRecorder()
	s.handleIdentityResolve(rec, httptest.NewRequest(http.MethodGet, "/api/v1/identity/resolve", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestIdentityResolveUnknownRefIs404(t *testing.T) {
	s, _ := newIdentityServer(t, nil)
	status, _, raw := resolveIdentity(t, s, "no-such-thread")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (%s)", status, http.StatusNotFound, raw)
	}
}

func TestIdentityResolveNameNativeAndHosted(t *testing.T) {
	row := catalogThread{
		ID: "nth_named", Provider: "codex", NativeID: identityNativeOld,
		Name: namedCatalog("graph classifier"), UpdatedAt: identityStamp("2026-08-20T12:00:00Z"),
	}
	s, store := newIdentityServer(t, []catalogThread{row})
	record := createIdentityHosted(t, store, "codex", identityNativeOld)

	want := identityResolveResponse{
		DisplayName: "graph classifier", NativeThreadID: identityNativeOld,
		HostedThreadID: record.ID, Provider: "codex", MatchCount: 1,
	}
	for _, ref := range []string{"graph%20classifier", identityNativeOld, record.ID} {
		status, body, raw := resolveIdentity(t, s, ref)
		if status != http.StatusOK {
			t.Fatalf("ref %s status = %d (%s)", ref, status, raw)
		}
		if body != want {
			t.Fatalf("ref %s = %#v, want %#v", ref, body, want)
		}
	}
}

func TestIdentityResolveDuplicateNamesPickMostRecentlyUpdated(t *testing.T) {
	s, _ := newIdentityServer(t, []catalogThread{
		{
			ID: "nth_older", Provider: "codex", NativeID: identityNativeOld,
			Name: namedCatalog("shared name"), UpdatedAt: identityStamp("2026-01-01T00:00:00Z"),
		},
		{
			ID: "nth_newer", Provider: "claude", NativeID: identityNativeNew,
			Name: namedCatalog("shared name"), UpdatedAt: identityStamp("2026-08-20T00:00:00Z"),
		},
	})
	status, body, raw := resolveIdentity(t, s, "shared%20name")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, raw)
	}
	if body.DisplayName != "shared name" || body.NativeThreadID != identityNativeNew ||
		body.Provider != "claude" || body.MatchCount != 2 {
		t.Fatalf("duplicate resolve = %#v", body)
	}
}

func TestIdentityResolveFollowsRename(t *testing.T) {
	row := catalogThread{
		ID: "nth_renamed", Provider: "codex", NativeID: identityNativeOld,
		Name: namedCatalog("old name"), UpdatedAt: identityStamp("2026-08-20T12:00:00Z"),
	}
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

func TestIdentityResolveSurvivesResumeRebind(t *testing.T) {
	row := catalogThread{
		ID: "nth_resume", Provider: "codex", NativeID: identityNativeOld,
		Name: namedCatalog("resume me"), UpdatedAt: identityStamp("2026-08-20T12:00:00Z"),
	}
	s, store := newIdentityServer(t, []catalogThread{row})
	record := createIdentityHosted(t, store, "codex", identityNativeOld)
	if err := store.bindSession(record.ID, "nano-resume", identityNativeOld, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}

	for _, ref := range []string{"resume%20me", identityNativeOld, record.ID} {
		status, body, raw := resolveIdentity(t, s, ref)
		if status != http.StatusOK || body.NativeThreadID != identityNativeOld || body.HostedThreadID != record.ID {
			t.Fatalf("resume ref %s = %d %#v (%s)", ref, status, body, raw)
		}
	}
}

func TestIdentityResolveFollowsClaudeRespawn(t *testing.T) {
	row := catalogThread{
		ID: "nth_claude", Provider: "claude", NativeID: identityNativeOld,
		Name: namedCatalog("claude owner"), UpdatedAt: identityStamp("2026-08-20T12:00:00Z"),
	}
	s, store := newIdentityServer(t, []catalogThread{row})
	record := createIdentityHosted(t, store, "claude", identityNativeOld)
	if err := store.bindSession(record.ID, "nano-respawn", identityNativeNew, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}

	wantNative, wantHosted := identityNativeNew, record.ID
	for _, ref := range []string{"claude%20owner", identityNativeOld, identityNativeNew, record.ID} {
		status, body, raw := resolveIdentity(t, s, ref)
		if status != http.StatusOK {
			t.Fatalf("respawn ref %s status = %d (%s)", ref, status, raw)
		}
		if body.DisplayName != "claude owner" || body.NativeThreadID != wantNative ||
			body.HostedThreadID != wantHosted || body.Provider != "claude" {
			t.Fatalf("respawn ref %s = %#v", ref, body)
		}
	}
}

func TestIdentityResolveLivePendingName(t *testing.T) {
	s, store := newIdentityServer(t, nil)
	record := createIdentityHosted(t, store, "claude", "")
	s.sessions["live"] = &session{id: "live",
		provider:       "claude",
		hostedThreadID: record.ID,
		displayName:    "draft name",
		createdAt:      time.Now(),
		lifecycleState: testLifecycleSnapshot("ready", false)}

	status, body, raw := resolveIdentity(t, s, "draft%20name")
	if status != http.StatusOK || body.HostedThreadID != record.ID || body.DisplayName != "draft name" {
		t.Fatalf("pending name = %d %#v (%s)", status, body, raw)
	}
}

func TestIdentityResolveLabelsClaudeProvisionalUntilProviderUUIDArrives(t *testing.T) {
	s, store := newIdentityServer(t, nil)
	record := createIdentityHosted(t, store, "claude", "cs_provisional")

	status, body, raw := resolveIdentity(t, s, record.ID)
	if status != http.StatusOK || !body.NativeThreadIDProvisional {
		t.Fatalf("provisional resolve = %d %#v (%s)", status, body, raw)
	}

	resolveTestProvisional(t, store.control, record.ID, "claude", "cs_provisional", identityNativeNew)
	if err := store.bindSession(record.ID, "nano-real", identityNativeNew, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	status, body, raw = resolveIdentity(t, s, record.ID)
	if status != http.StatusOK || body.NativeThreadID != identityNativeNew || body.NativeThreadIDProvisional {
		t.Fatalf("provider UUID resolve = %d %#v (%s)", status, body, raw)
	}
}

func TestIdentityResolveResultsAreNotStored(t *testing.T) {
	s, store := newIdentityServer(t, []catalogThread{{
		Provider: "codex", NativeID: identityNativeOld, Name: namedCatalog("cached"),
	}})
	record := createIdentityHosted(t, store, "codex", identityNativeOld)
	rec := httptest.NewRecorder()
	s.handleIdentityResolve(rec, httptest.NewRequest(http.MethodGet, "/api/v1/identity/resolve?ref="+record.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}
