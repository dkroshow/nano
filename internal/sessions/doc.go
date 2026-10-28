// Package sessions owns live provider attachments, provider wiring, lifecycle
// application, prompt delivery, and ordered wire events. It owns each session's
// synchronization and the receiver methods that apply those transitions.
//
// Hosted identity and admission, the thread catalog, and saved/live view state
// remain with their respective owners. Sessions consume those owners through
// explicit construction inputs and small interfaces; it does not import app.
package sessions
