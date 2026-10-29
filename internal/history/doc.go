// Package history discovers native coding-agent files and stores their catalog,
// transcript projections, search evidence and usage. It is read-only toward
// provider-owned files; Store owns the durable derived records.
//
// store.go opens the database and defines its lock discipline; store_read.go
// and store_write.go read and apply catalog metadata. search_* files own search
// projection and eligibility. transcript_* files own saved transcript storage,
// parsing checkpoints, retained values and bounded reads.
//
// transcript_store.go remains large because it keeps the durable transcript
// apply, read and cleanup paths beside their shared transaction helpers and
// Store.writeMu discipline. Splitting those paths is deferred maintenance;
// a split must preserve atomic projection updates and consistent read snapshots.
package history
