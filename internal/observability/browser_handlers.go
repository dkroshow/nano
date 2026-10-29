package observability

import (
	"github.com/dkroshow/nano/internal/diagnostics"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// BrowserHandlers accepts browser observations and serves bounded diagnostic views.
type BrowserHandlers struct {
	browserDiagnostics    *diagnostics.Store
	transcriptValidations *ValidationStore
	controlMetadata       *hostedstore.MetadataStore
	controlMetadataErr    string
}

// NewBrowserHandlers wires diagnostic interpretation to its retained evidence owners.
func NewBrowserHandlers(events *diagnostics.Store, validations *ValidationStore, metadata *hostedstore.MetadataStore, metadataError string) *BrowserHandlers {
	return &BrowserHandlers{browserDiagnostics: events, transcriptValidations: validations, controlMetadata: metadata, controlMetadataErr: metadataError}
}
func diagnosticToken(value string, max int) string { return diagnostics.Token(value, max) }
func diagnosticTime(value string) string           { return diagnostics.Time(value) }
