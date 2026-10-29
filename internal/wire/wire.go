// Package wire defines provider-neutral wire events, direction values and JSON
// field readers shared by live provider adapters.
package wire

// Direction identifies whether a frame crossed toward or away from a provider.
type Direction string

const (
	Send Direction = "send"
	Recv Direction = "recv"
)
