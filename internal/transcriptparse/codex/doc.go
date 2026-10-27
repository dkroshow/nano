// Package codex interprets Codex file and wire records through shared assembly. It may
// depend only on model, private parsing helpers and the standard library.
// Ingestion and standalone consumers may use its readers.
// Growing-source continuation retires earlier settled turns while retaining
// the latest turn, unfinished turns and pending request context. Consumers store
// emitted rows separately and resolve identities seen after eviction. Open
// continuation above 4 MiB is diagnosed without discarding unfinished evidence.
package codex
