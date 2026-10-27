// Package claude interprets Claude Code file and wire records through shared assembly. It may
// depend only on model, private parsing helpers and the standard library.
// Ingestion and standalone consumers may use its readers.
//
// Growing-source checkpoints keep unfinished turns and the most recent turn
// in each native scope, including trailing completion and usage context. Once
// a new prompt retires a settled turn, its items, messages and replay receipts
// leave continuation state. Consumers retain all emitted changes separately.
// An unmarked identity seen after eviction is emitted with a new local key;
// the consumer's persisted identity index decides whether it is the same row.
// A 4 MiB warning diagnoses oversized open state without dropping evidence.
package claude
