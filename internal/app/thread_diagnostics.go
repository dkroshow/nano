package app

import (
	"encoding/json"
	"log"
	"slices"
)

const (
	threadDiagnosticLimit           = 32
	threadDiagnosticSummaryInterval = 256
)

// threadDiagnostic is one content-free fact a thread reports as
// threadDiagnostics: a delivery record parked at boot, or a session
// event that could not be applied. Repeats coalesce by code.
type threadDiagnostic struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Source      string `json:"source"`
	ActionID    string `json:"actionId,omitempty"`
	Occurrences uint64 `json:"occurrences"`
	reported    uint64
}

type threadDiagnosticLog uint8

const (
	threadDiagnosticLogNone threadDiagnosticLog = iota
	threadDiagnosticLogFirst
	threadDiagnosticLogSummary
)

func appendBoundedThreadDiagnostic(prior []threadDiagnostic, diagnostic threadDiagnostic) ([]threadDiagnostic, threadDiagnostic, threadDiagnosticLog) {
	if diagnostic.Occurrences == 0 {
		diagnostic.Occurrences = 1
	}
	for i, existing := range prior {
		if existing.Code != diagnostic.Code {
			continue
		}
		updated := existing
		if updated.Occurrences == 0 {
			updated.Occurrences = 1
		}
		updated.Occurrences += diagnostic.Occurrences
		// A repeat remains the most recent diagnostic without consuming a
		// second list slot.
		next := make([]threadDiagnostic, 0, len(prior))
		next = append(next, prior[:i]...)
		next = append(next, prior[i+1:]...)
		next = append(next, updated)
		if updated.Occurrences/threadDiagnosticSummaryInterval > updated.reported/threadDiagnosticSummaryInterval {
			updated.reported = updated.Occurrences
			next[len(next)-1] = updated
			return next, updated, threadDiagnosticLogSummary
		}
		return next, threadDiagnostic{}, threadDiagnosticLogNone
	}
	diagnostic.reported = diagnostic.Occurrences
	prior = append(prior, diagnostic)
	if len(prior) > threadDiagnosticLimit {
		prior = append([]threadDiagnostic(nil), prior[len(prior)-threadDiagnosticLimit:]...)
	}
	return prior, diagnostic, threadDiagnosticLogFirst
}

// recordDiagnostic adds one diagnostic to the thread's bounded list. With
// once, a diagnostic already listed for the same code, source and action is
// not counted again.
func (p *liveProjection) recordDiagnostic(diagnostic threadDiagnostic, once bool) {
	if p == nil || diagnostic.Code == "" {
		return
	}
	p.diagnosticsMu.Lock()
	defer p.diagnosticsMu.Unlock()
	if once && slices.ContainsFunc(p.diagnostics, func(prior threadDiagnostic) bool {
		return prior.Code == diagnostic.Code && prior.Source == diagnostic.Source && prior.ActionID == diagnostic.ActionID
	}) {
		return
	}
	var logged threadDiagnostic
	var logKind threadDiagnosticLog
	p.diagnostics, logged, logKind = appendBoundedThreadDiagnostic(p.diagnostics, diagnostic)
	if logKind == threadDiagnosticLogNone {
		return
	}
	structured, _ := json.Marshal(logged)
	prefix := "thread diagnostic " + logged.Source
	if logKind == threadDiagnosticLogSummary {
		prefix = "thread diagnostic repeat summary"
	}
	log.Printf("%s provider=%q hostedThreadId=%q diagnostic=%s", prefix, p.provider, p.hostedThreadID, structured)
}

// threadDiagnosticsSnapshot copies the bounded list.
func (p *liveProjection) threadDiagnosticsSnapshot() []threadDiagnostic {
	if p == nil {
		return nil
	}
	p.diagnosticsMu.Lock()
	defer p.diagnosticsMu.Unlock()
	return slices.Clone(p.diagnostics)
}
