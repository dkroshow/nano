package observability

const (
	// ThreadDiagnosticLimit bounds a thread’s retained diagnostics.
	ThreadDiagnosticLimit = 32
	// threadDiagnosticSummaryInterval bounds repeat logging frequency.
	threadDiagnosticSummaryInterval = 256
)

// ThreadDiagnostic is one content-free fact a thread reports as
// threadDiagnostics: a delivery record parked at boot, or a session
// event that could not be applied. Repeats coalesce by code.
type ThreadDiagnostic struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Source      string `json:"source"`
	ActionID    string `json:"actionId,omitempty"`
	Occurrences uint64 `json:"occurrences"`
	reported    uint64
}

// ThreadDiagnosticLog identifies the log emitted after a bounded append.
type ThreadDiagnosticLog uint8

const (
	// DiagnosticLogNone suppresses repeated diagnostics until the next summary.
	DiagnosticLogNone ThreadDiagnosticLog = iota
	// diagnosticLogFirst logs the first occurrence of a diagnostic code.
	diagnosticLogFirst
	// DiagnosticLogSummary logs an aggregate of repeated occurrences.
	DiagnosticLogSummary
)

// AppendThreadDiagnostic coalesces by code while retaining bounded repeat counts.
func AppendThreadDiagnostic(prior []ThreadDiagnostic, diagnostic ThreadDiagnostic) ([]ThreadDiagnostic, ThreadDiagnostic, ThreadDiagnosticLog) {
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
		next := make([]ThreadDiagnostic, 0, len(prior))
		next = append(next, prior[:i]...)
		next = append(next, prior[i+1:]...)
		next = append(next, updated)
		if updated.Occurrences/threadDiagnosticSummaryInterval > updated.reported/threadDiagnosticSummaryInterval {
			updated.reported = updated.Occurrences
			next[len(next)-1] = updated
			return next, updated, DiagnosticLogSummary
		}
		return next, ThreadDiagnostic{}, DiagnosticLogNone
	}
	diagnostic.reported = diagnostic.Occurrences
	prior = append(prior, diagnostic)
	if len(prior) > ThreadDiagnosticLimit {
		prior = append([]ThreadDiagnostic(nil), prior[len(prior)-ThreadDiagnosticLimit:]...)
	}
	return prior, diagnostic, diagnosticLogFirst
}
