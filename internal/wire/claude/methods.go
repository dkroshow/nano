package claude

// Methods name the frozen Claude shim JSON-RPC contract.
const (
	MethodStart      = "session/start"
	MethodAttach     = "session/attach"
	MethodInput      = "session/input"
	MethodClose      = "session/close"
	MethodLine       = "session/line"
	MethodExited     = "session/exited"
	MethodReplayDone = "session/replayDone"
)

// Error codes identify the frozen shim refusal categories.
const (
	CodeSessionAttached = -32001
	CodeSessionNotFound = -32002
	CodeSessionExited   = -32003
)
