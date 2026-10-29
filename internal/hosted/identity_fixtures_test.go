package hosted

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

const (
	identityNativeOld   = "11111111-aaaa-4bbb-8ccc-dddddddddddd"
	identityNativeNew   = "22222222-aaaa-4bbb-8ccc-dddddddddddd"
	threadReferenceUUID = "11111111-2222-4333-8444-555555555555"
)

type identityFixtureCatalog struct {
	items  []CatalogIdentity
	thread CatalogIdentity
	loadID string
	status string
}

func (c *identityFixtureCatalog) NativeIdentities(_ context.Context, native, provider string) ([]CatalogIdentity, error) {
	rows := []CatalogIdentity{}
	for _, row := range c.items {
		if row.NativeID == native && (provider == "" || provider == row.Provider) {
			rows = append(rows, row)
		}
	}
	return rows, nil
}
func (c *identityFixtureCatalog) NamedIdentities(_ context.Context, name string) ([]CatalogIdentity, error) {
	rows := []CatalogIdentity{}
	for _, row := range c.items {
		if row.Name == name {
			rows = append(rows, row)
		}
	}
	return rows, nil
}
func (c *identityFixtureCatalog) LoadIdentity(_ context.Context, id string) (CatalogIdentity, error) {
	c.loadID = id
	return c.thread, nil
}
func (c *identityFixtureCatalog) IsIndexing(context.Context) bool {
	return c.status == "starting" || c.status == "scanning"
}
func (c *identityFixtureCatalog) DurableName(provider, native string) (string, bool) {
	return "", false
}
func newIdentityService(t *testing.T, rows []CatalogIdentity) (*IdentityService, *hostedstore.ExecutionStore) {
	t.Helper()
	control := newTestControlMetadata(t)
	migrateTestControl(t, control)
	store, err := newControlAgentExecutionStore(control, "identity-test")
	if err != nil {
		t.Fatal(err)
	}
	return &IdentityService{Executions: store, Catalog: &identityFixtureCatalog{items: rows}, CatalogErrors: CatalogErrors{NotFound: errors.New("catalog thread not found")}}, store
}
func newReferenceService(t *testing.T, rows []CatalogIdentity) (*IdentityService, *hostedstore.ExecutionStore) {
	return newIdentityService(t, rows)
}
func createIdentityHosted(t *testing.T, store *hostedstore.ExecutionStore, provider, native string) ownedthreads.SessionRecord {
	t.Helper()
	record, _, err := fixtureBeginSession(store, "", "identity-"+native, provider, ownedthreads.PresentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if native != "" {
		if err := store.BindSession(record.ID, "nano-"+record.ID, native, "ready", ownedthreads.PresentationShared); err != nil {
			t.Fatal(err)
		}
	}
	updated, ok := store.Session(record.ID)
	if !ok {
		t.Fatal("missing hosted record")
	}
	return updated
}
func createReferenceHostedRecord(t *testing.T, store *hostedstore.ExecutionStore, native string) ownedthreads.SessionRecord {
	return createIdentityHosted(t, store, "codex", native)
}
func namedCatalog(value string) string  { return value }
func identityStamp(value string) string { return value }
func resolveIdentity(t *testing.T, s *IdentityService, ref string) (int, IdentityResolution, string) {
	t.Helper()
	response := httptest.NewRecorder()
	s.HandleIdentityResolve(response, httptest.NewRequest(http.MethodGet, "/api/v1/identity/resolve?ref="+ref, nil))
	var body IdentityResolution
	if response.Code == http.StatusOK && json.Unmarshal(response.Body.Bytes(), &body) != nil {
		t.Fatalf("invalid response %q", response.Body.String())
	}
	return response.Code, body, response.Body.String()
}
func resolveReference(t *testing.T, s *IdentityService, ref string) (int, threadReferenceResponse) {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/thread-references/"+ref, nil)
	request.SetPathValue("reference", ref)
	s.HandleThreadReference(response, request)
	var body threadReferenceResponse
	if response.Code == http.StatusOK && json.Unmarshal(response.Body.Bytes(), &body) != nil {
		t.Fatalf("invalid response %q", response.Body.String())
	}
	return response.Code, body
}
