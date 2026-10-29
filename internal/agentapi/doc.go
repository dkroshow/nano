// Package agentapi owns the HTTP and application adapters for agent mail,
// heartbeat configuration, feedback, and completion subscriptions. It consumes
// the concrete hosted identity/control services, sidebar projection, diagnostics
// store, and feature owners. Mail, heartbeat, feedback and subscriptions retain
// their existing stores and schedulers; this package owns no second durable
// record and does not own session state or saved/live transcript reconciliation.
package agentapi
