package observability

import (
	"github.com/dkroshow/nano/internal/diagnostics"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/testsupport"
	"path/filepath"
	"testing"
)

func newBrowserDiagnosticStore(capacity int) *diagnostics.Store {
	return diagnostics.NewStore(capacity, "test-boot")
}
func newTestControlMetadata(t *testing.T) *hostedstore.MetadataStore {
	t.Helper()
	db, _, err := testsupport.TurnDatabase(t, filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := hostedstore.NewMetadataStore(db, db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
