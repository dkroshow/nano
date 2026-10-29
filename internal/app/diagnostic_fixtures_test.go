package app

import (
	"github.com/dkroshow/nano/internal/diagnostics"
	"github.com/dkroshow/nano/internal/sessions"
)

func newBrowserDiagnosticStore(capacity int) *diagnostics.Store {
	return diagnostics.NewStore(capacity, sessions.NewID())
}
