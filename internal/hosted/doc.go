// Package hosted owns hosted identity resolution, admission and control,
// their HTTP views, and pin storage. Shared metadata, execution and attachment
// adapters live in hosted/store over the existing canonical records.
//
// Hosted also owns restart progress for durable attachments, including the
// append-only restart record and its read API.
//
// Sessions owns live attachment state and provider connections; hosted reads
// that state through the concrete session registry. The catalog belongs to
// threadindex, and visible-thread composition belongs to sidebar.
// AttachmentCoordinator owns boot ordering, provider resume serialization,
// canonical identity claims, and the shared explicit/boot registration path.
// Sessions owns the pending/resting boot attachment records and handshakes.
// Edge/stateopen assembles the shared databases; hosted opens its pin database.
package hosted
