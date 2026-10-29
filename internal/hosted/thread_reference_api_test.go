package hosted

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dkroshow/nano/internal/history"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

func TestThreadReferenceRejectsTruncatedNativeUUID(t *testing.T) {
	s, _ := newReferenceService(t, nil)
	status, _ := resolveReference(t, s, "11111111-2222-4333")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

func TestThreadReferenceCatalogCompatibilityInputResolvesExactRow(t *testing.T) {
	row := catalogIdentity{ID: "nth_exact", Provider: "codex", NativeID: threadReferenceUUID}
	s, _ := newReferenceService(t, []catalogIdentity{row})

	status, body := resolveReference(t, s, row.ID)
	if status != http.StatusOK || body.State != "resolved" || body.Target == nil || body.Target.Route != "historical" {
		t.Fatalf("response = %d %#v", status, body)
	}
	if body.Target.CatalogThreadID != row.ID || body.Target.NativeThreadID != threadReferenceUUID {
		t.Fatalf("catalog target = %#v", body.Target)
	}
}

func TestThreadReferenceSameProviderDuplicateCatalogRowsUseCurrentOwner(t *testing.T) {
	rows := []catalogIdentity{
		{ID: "nth_first", Provider: "codex", NativeID: threadReferenceUUID},
		{ID: "nth_second", Provider: "codex", NativeID: threadReferenceUUID},
	}
	s, store := newReferenceService(t, rows)
	s.Executions, _ = newControlAgentExecutionStore(store.Control(), "empty-reader")
	record := createReferenceHostedRecord(t, store, threadReferenceUUID)

	status, body := resolveReference(t, s, threadReferenceUUID)
	if status != http.StatusOK || body.State != "resolved" || body.Target == nil || body.Target.HostedThreadID != record.ID {
		t.Fatalf("response = %d %#v", status, body)
	}
}

func TestThreadReferenceResultsAreNotStored(t *testing.T) {
	s, _ := newReferenceService(t, nil)
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/thread-references/"+threadReferenceUUID, nil)
	request.SetPathValue("reference", threadReferenceUUID)
	s.HandleThreadReference(rec, request)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestThreadReferenceNativeHistoricalFallbackAndHostedDirect(t *testing.T) {
	row := catalogIdentity{ID: "nth_exact", Provider: "codex", NativeID: threadReferenceUUID}
	s, store := newReferenceService(t, []catalogIdentity{row})
	s.Executions, _ = newControlAgentExecutionStore(store.Control(), "empty-reader")
	record := createReferenceHostedRecord(t, store, threadReferenceUUID)

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
	s, store := newReferenceService(t, nil)
	s.Executions, _ = newControlAgentExecutionStore(store.Control(), "empty-reader")
	first := createReferenceHostedRecord(t, store, threadReferenceUUID)
	if err := store.BindSession(first.ID, "nano-"+first.ID, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "ready", ownedthreads.PresentationShared); err != nil {
		t.Fatal(err)
	}
	current := createReferenceHostedRecord(t, store, threadReferenceUUID)

	status, body := resolveReference(t, s, threadReferenceUUID)
	if status != http.StatusOK || body.State != "resolved" || body.Target == nil || body.Target.HostedThreadID != current.ID {
		t.Fatalf("response = %d %#v", status, body)
	}
}

func TestThreadReferenceCrossProviderNativeCollisionDoesNotGuess(t *testing.T) {
	s, store := newReferenceService(t, nil)
	s.Executions, _ = newControlAgentExecutionStore(store.Control(), "empty-reader")
	_ = createReferenceHostedRecord(t, store, threadReferenceUUID)
	other, _, err := fixtureBeginSession(store, "", "reference-claude", "claude", ownedthreads.PresentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(other.ID, "nano-"+other.ID, threadReferenceUUID, "ready", ownedthreads.PresentationShared); err != nil {
		t.Fatal(err)
	}

	status, body := resolveReference(t, s, threadReferenceUUID)
	if status != http.StatusOK || body.State != "unresolved" || body.Reason != "ambiguous" || body.Target != nil {
		t.Fatalf("response = %d %#v", status, body)
	}
}

func TestThreadReferenceUnresolvedSchemaDistinguishesIndexing(t *testing.T) {
	unknown := "99999999-2222-4333-8444-555555555555"
	s, _, saved := newIdentityFixture(t, nil)
	setIdentityIndexState(t, saved, "scanning")

	status, body := resolveReference(t, s, unknown)
	if status != http.StatusOK || body.State != "unresolved" || body.Reason != "not_indexed" || body.Target != nil {
		t.Fatalf("indexing response = %d %#v", status, body)
	}
	setIdentityIndexState(t, saved, "ready")
	status, body = resolveReference(t, s, unknown)
	if status != http.StatusOK || body.State != "unresolved" || body.Reason != "unknown" || body.Target != nil {
		t.Fatalf("unknown response = %d %#v", status, body)
	}
}

func setIdentityIndexState(t *testing.T, saved *history.Store, state history.IndexStateName) {
	t.Helper()
	roots, err := saved.ListRoots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range roots {
		if err := saved.SetIndexState(context.Background(), root.ID, state, nil); err != nil {
			t.Fatal(err)
		}
	}
}
