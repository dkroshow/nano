package lifecycle

// Capabilities declares evidence available from a provider; false means absent.
// A missing native turn id/start time requires adapter correlation/observation
// anchors, never invented provider facts or durable completion authority.
type Capabilities struct {
	NativeTurnID       bool
	NativeTurnStart    bool
	ReadyWithoutInit   bool
	ResumeWorkSnapshot bool
	NativeRecovery     bool
	// BootHandshakeLimit bounds concurrent provider reattachment. Zero means
	// the provider needs no boot-specific limit.
	BootHandshakeLimit int
	// ResumeWithoutTranscript supports a provider-resident, never-prompted
	// thread whose durable transcript has not been created yet.
	ResumeWithoutTranscript bool
}

// Adapter exposes current qualified connection/work evidence. Current reads an
// already-observed snapshot without I/O and returns false when it is not settled.
// A connected snapshot is a complete ready_* observation, not a lossy cache of
// the last delta. Live deltas use the existing ordered provider callbacks.
// Implementations live beside today's provider files and qualify native identity,
// current turn and replay boundaries before publishing evidence. Only the
// coordinator emits attachment commands, never Current. Capability support does
// not authorize native recovery or grant permission to replace provider state.
// A provider may also declare a boot handshake limit; coordination applies that
// bound before provider I/O while every waiting attachment remains starting.
type Adapter interface {
	Capabilities() Capabilities
	Current() (Observation, bool)
}
