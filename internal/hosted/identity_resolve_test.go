package hosted

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIdentityResolveRejectsMissingRef(t *testing.T) {
	s, _ := newIdentityService(t, nil)
	rec := httptest.NewRecorder()
	s.HandleIdentityResolve(rec, httptest.NewRequest(http.MethodGet, "/api/v1/identity/resolve", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestIdentityResolveUnknownRefIs404(t *testing.T) {
	s, _ := newIdentityService(t, nil)
	status, _, raw := resolveIdentity(t, s, "no-such-thread")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (%s)", status, http.StatusNotFound, raw)
	}
}

func TestIdentityResolveNameNativeAndHosted(t *testing.T) {
	row := CatalogIdentity{
		ID: "nth_named", Provider: "codex", NativeID: identityNativeOld,
		Name: namedCatalog("graph classifier"), UpdatedAt: identityStamp("2026-08-20T12:00:00Z"),
	}
	s, store := newIdentityService(t, []CatalogIdentity{row})
	record := createIdentityHosted(t, store, "codex", identityNativeOld)

	want := IdentityResolution{
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
	s, _ := newIdentityService(t, []CatalogIdentity{
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

func TestIdentityResolveSurvivesResumeRebind(t *testing.T) {
	row := CatalogIdentity{
		ID: "nth_resume", Provider: "codex", NativeID: identityNativeOld,
		Name: namedCatalog("resume me"), UpdatedAt: identityStamp("2026-08-20T12:00:00Z"),
	}
	s, store := newIdentityService(t, []CatalogIdentity{row})
	record := createIdentityHosted(t, store, "codex", identityNativeOld)
	if err := store.BindSession(record.ID, "nano-resume", identityNativeOld, "ready", presentationShared); err != nil {
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
	row := CatalogIdentity{
		ID: "nth_claude", Provider: "claude", NativeID: identityNativeOld,
		Name: namedCatalog("claude owner"), UpdatedAt: identityStamp("2026-08-20T12:00:00Z"),
	}
	s, store := newIdentityService(t, []CatalogIdentity{row})
	record := createIdentityHosted(t, store, "claude", identityNativeOld)
	if err := store.BindSession(record.ID, "nano-respawn", identityNativeNew, "ready", presentationShared); err != nil {
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

func TestIdentityResolveLabelsClaudeProvisionalUntilProviderUUIDArrives(t *testing.T) {
	s, store := newIdentityService(t, nil)
	record := createIdentityHosted(t, store, "claude", "cs_provisional")

	status, body, raw := resolveIdentity(t, s, record.ID)
	if status != http.StatusOK || !body.NativeThreadIDProvisional {
		t.Fatalf("provisional resolve = %d %#v (%s)", status, body, raw)
	}

	resolveTestProvisional(t, store.Control(), record.ID, "claude", "cs_provisional", identityNativeNew)
	if err := store.BindSession(record.ID, "nano-real", identityNativeNew, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	status, body, raw = resolveIdentity(t, s, record.ID)
	if status != http.StatusOK || body.NativeThreadID != identityNativeNew || body.NativeThreadIDProvisional {
		t.Fatalf("provider UUID resolve = %d %#v (%s)", status, body, raw)
	}
}

func TestIdentityResolveResultsAreNotStored(t *testing.T) {
	s, store := newIdentityService(t, []CatalogIdentity{{
		Provider: "codex", NativeID: identityNativeOld, Name: namedCatalog("cached"),
	}})
	record := createIdentityHosted(t, store, "codex", identityNativeOld)
	rec := httptest.NewRecorder()
	s.HandleIdentityResolve(rec, httptest.NewRequest(http.MethodGet, "/api/v1/identity/resolve?ref="+record.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}
