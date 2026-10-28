package observability

// RestartStep identifies a progress mark in one process restart.
type RestartStep int

const (
	// RestartPicked marks selection of an attachment for resume.
	RestartPicked RestartStep = iota
	// RestartLock marks acquisition of the provider resume lock.
	RestartLock
	// RestartRegistered marks publication of the session shell.
	RestartRegistered
	// RestartAttached marks completion of the provider attachment.
	RestartAttached
	// RestartLive marks provider readiness after replay settlement.
	RestartLive
)
