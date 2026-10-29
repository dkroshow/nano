// Package sessions owns live provider attachments, provider wiring, lifecycle
// application, prompt delivery, ordered wire events, live projection, and the
// saved/live transcript reconciliation loop. Pane queues and warm sizing share
// this owner so parsed evidence and saved rows pass through one ordered loop.
// It also owns pending/resting boot attachment facts; hosted schedules boot
// registrations and performs canonical attachment admission.
//
// Hosted identity and admission, the thread catalog, and browser warm-stream
// orchestration belong to their respective owners. Sessions do not import app.
package sessions
