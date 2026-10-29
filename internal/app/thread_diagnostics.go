package app

import (
	observability "github.com/dkroshow/nano/internal/observability"
)

type threadDiagnostic = observability.ThreadDiagnostic

type threadDiagnosticLog = observability.ThreadDiagnosticLog

const (
	threadDiagnosticLimit      = observability.ThreadDiagnosticLimit
	threadDiagnosticLogNone    = observability.DiagnosticLogNone
	threadDiagnosticLogSummary = observability.DiagnosticLogSummary
)
