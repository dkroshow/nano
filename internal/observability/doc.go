// Package observability owns bounded request measurements, process logging,
// and restart progress values.
// It does not own sessions, ingestion, restart state, or durable browser evidence;
// that evidence remains in internal/diagnostics and the relevant state owners.
package observability
