// Package threads owns durable hosted-thread state: identity, metadata,
// execution sessions, operations, and attachment generations.
//
// Attachment allocation uses the existing state tables without a second
// identity store or generation counter. A stored generation fences an
// attachment; it does not establish provider readiness or activity.
//
// The package depends only on the standard library, the shared state database
// opener, and the SQLite driver. It does not own transactional turns, feature
// data, diagnostics, provider connections, transcripts, or lifecycle state.
package threads
