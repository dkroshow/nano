// Package sessions owns live provider attachments and their runtime state.
// Session is the identity and construction root. Its concrete owners are:
//
//   - Delivery: prompt admission, sends, settlement and parked interactions;
//   - State: atomic lifecycle, native identity, turn, replay and posture facts;
//   - Runtime: provider handles, handshake serialization and the core event loop;
//   - Wire: ordered raw evidence, the retained spool and event subscribers;
//   - Lifecycle: attachment close and the fenced transcript repair operation;
//   - View: summaries, HTTP reads, live projection access and view caches.
//
// Owners retain their existing locks. Provider handle publication and readiness
// share State's lock so observers cannot see half of that transition. Session
// construction connects the owners before publishing the attachment; callers
// use the owner accessors directly, without Session forwarding methods.
//
// LiveTextLoop remains the sole saved/live reconciliation loop. Pane queues and
// warm sizing share this package so parsed evidence and saved rows keep their
// event order. The package also owns pending/resting boot attachment facts;
// hosted schedules boot registrations and performs canonical admission.
//
// Hosted identity and admission, the thread catalog, and browser warm-stream
// orchestration belong to their respective owners. Sessions do not import app.
package sessions
