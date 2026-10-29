// Package threadindex owns the saved-thread catalog service, provider-file
// watching and ingestion, name projection, bounded transcript reads and their
// HTTP API. It builds on catalog, history and transcriptparse without importing
// app, sessions or hosted. Provider identity/path rules enter as explicit
// functions; committed transcript changes leave through an injected callback.
//
// Sessions owns saved/live text reconciliation, pane rows and session setup
// responses. Sidebar owns catalog responses that join runtime, hosted and mail
// facts with the saved-thread projection.
// Hosted identity and execution facts belong to hosted, never to this index.
package threadindex
