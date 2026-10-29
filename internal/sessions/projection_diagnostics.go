package sessions

import (
	"encoding/json"
	"log"
	"slices"

	observability "github.com/dkroshow/nano/internal/observability"
)

// RecordDiagnostic adds one diagnostic to the thread's bounded list. With
// once, a diagnostic already listed for the same code, source and action is
// not counted again.
func (p *LiveProjection) RecordDiagnostic(diagnostic observability.ThreadDiagnostic, once bool) {
	if p == nil || diagnostic.Code == "" {
		return
	}
	p.diagnosticsMu.Lock()
	defer p.diagnosticsMu.Unlock()
	if once && slices.ContainsFunc(p.diagnostics, func(prior observability.ThreadDiagnostic) bool {
		return prior.Code == diagnostic.Code && prior.Source == diagnostic.Source && prior.ActionID == diagnostic.ActionID
	}) {
		return
	}
	var logged observability.ThreadDiagnostic
	var logKind observability.ThreadDiagnosticLog
	p.diagnostics, logged, logKind = observability.AppendThreadDiagnostic(p.diagnostics, diagnostic)
	if logKind == observability.DiagnosticLogNone {
		return
	}
	structured, _ := json.Marshal(logged)
	prefix := "thread diagnostic " + logged.Source
	if logKind == observability.DiagnosticLogSummary {
		prefix = "thread diagnostic repeat summary"
	}
	log.Printf("%s provider=%q hostedThreadId=%q diagnostic=%s", prefix, p.provider, p.hostedThreadID, structured)
}

// ThreadDiagnosticsSnapshot copies the bounded list.
func (p *LiveProjection) ThreadDiagnosticsSnapshot() []observability.ThreadDiagnostic {
	if p == nil {
		return nil
	}
	p.diagnosticsMu.Lock()
	defer p.diagnosticsMu.Unlock()
	return slices.Clone(p.diagnostics)
}
