// Package hosted owns hosted identity resolution, admission and control,
// their HTTP views, and pin storage. Shared metadata, execution and attachment
// adapters live in hosted/store over the existing canonical records.
//
// Sessions owns live attachment state and provider connections; hosted reads
// that state through the concrete session registry. The catalog belongs to
// threadindex, and visible-thread composition belongs to sidebar.
// Sessions also owns the pending/resting boot attachment records. Only
// Controller.Reattach remains a construction-time callback until the lifecycle
// landing moves boot orchestration down.
// App composes shared database migrations; hosted opens its pin database.
package hosted
