// Package hosted owns hosted identity resolution, admission facts and durable
// control adapters. The adapters use the canonical records in threads and the
// core stores; they do not create a second durable owner.
//
// Sessions own live attachment state and provider connections. The catalog and
// sidebar stay in app, which supplies their read projections at hosted's query
// boundaries. App opens the shared control database and composes its migrations;
// hosted opens its pin database and checks metadata integrity through the shared
// state wiring helpers.
package hosted
