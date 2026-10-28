// Package hosted owns the values exchanged with hosted identity, metadata,
// admission and execution storage. Durable identity and generation allocation
// remain in threads. Provider connections, live session state, catalog reads and
// live transcript views belong to their own consumers.
package hosted
