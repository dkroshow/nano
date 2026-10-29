// Package hosted owns hosted identity resolution, admission and control,
// their HTTP views, and pin storage. Shared metadata, execution and attachment
// adapters live in hosted/store over the existing canonical records.
//
// Sessions owns live attachment state and provider connections; hosted reads
// that state through the concrete session registry. The catalog belongs to
// threadindex, while sidebar composition remains in app until its extraction.
// Controller.Waiting and Controller.Reattach remain construction-time boot
// callbacks until the lifecycle landing moves boot ownership down.
// App composes shared database migrations; hosted opens its pin database.
package hosted
