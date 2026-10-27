// Package grok interprets Grok Build file and wire records through shared assembly. It may
// depend only on model, private parsing helpers and the standard library.
// Ingestion and standalone consumers may use its readers.
// Growing-source continuation retires earlier settled turns while retaining
// the latest turn per session, unfinished turns and pending request context.
// Explicit replay outside retained state emits an observation and native
// endpoint for consumer resolution, without reopening item or turn assembly.
// Open continuation above 4 MiB is diagnosed without dropping evidence.
package grok
