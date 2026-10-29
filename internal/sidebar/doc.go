// Package sidebar owns the visible-thread projection, fleet transitions,
// operator stars, and browser warm-stream membership. Its controller reads
// concrete catalog, session, hosted-store, and mail owners; it does not copy
// their durable facts or own live session state. The saved/live reconciliation
// loop, pane queues, and warm sizing remain in sessions. App composes routes
// and boot orchestration.
package sidebar
