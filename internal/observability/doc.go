// Package observability owns bounded request measurements and process logging.
// It does not own sessions, ingestion, restart state, or durable browser evidence;
// that evidence remains in internal/diagnostics and the relevant state owners.
package observability
