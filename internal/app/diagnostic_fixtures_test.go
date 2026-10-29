package app

import (
	"github.com/dkroshow/nano/internal/diagnostics"
	"github.com/dkroshow/nano/internal/observability"
	"github.com/dkroshow/nano/internal/sessions"
)

func newBrowserDiagnosticStore(capacity int) *diagnostics.Store {
	return diagnostics.NewStore(capacity, sessions.NewID())
}

const (
	threadDiagnosticLimit      = observability.ThreadDiagnosticLimit
	threadDiagnosticLogNone    = observability.DiagnosticLogNone
	threadDiagnosticLogSummary = observability.DiagnosticLogSummary
)
