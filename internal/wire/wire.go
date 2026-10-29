// Package wire defines provider-neutral wire direction values.
package wire

// Direction identifies whether a frame crossed toward or away from a provider.
type Direction string

const (
	Send Direction = "send"
	Recv Direction = "recv"
)
