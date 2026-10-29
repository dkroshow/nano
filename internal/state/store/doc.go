// Package statestore owns checked publication, retained backups, source
// fingerprints and size measurements for the canonical SQLite store. It uses
// lower state and record packages; edge/stateopen assembles runtime handles,
// and edge/statewiring supplies the ordered migrations.
package statestore
