// Package threadindex owns the saved-thread catalog service, provider-file
// watching and ingestion, name projection, bounded transcript reads and their
// HTTP API. It builds on catalog, history and transcriptparse without importing
// app, sessions or hosted. Provider identity/path rules enter as explicit
// functions; committed transcript changes leave through an injected callback.
//
// The live loop in app still owns saved/live text reconciliation and pane rows.
// Sidebar projection, runtime, mail and setup response joins remain in app.
// Hosted identity and execution facts belong to hosted, never to this index.
package threadindex
