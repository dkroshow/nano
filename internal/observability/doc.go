// Package observability owns bounded request measurements, process logging,
// restart progress values, host-monitor reads, browser validation, transcript
// audits and bounded content-free thread diagnostics.
// It does not own sessions, ingestion, restart state, or durable browser evidence;
// that evidence remains in internal/diagnostics and the relevant state owners.
//
// Read handlers may depend on threadindex, hosted/store, history, diagnostics
// and HTTP transport helpers. They must not import app, sessions, hosted or
// sidebar, which consume these measurements and evidence.
package observability
