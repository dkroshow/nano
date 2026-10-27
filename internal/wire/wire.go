// Package wire defines the common live-provider connection boundary.
package wire

import "context"

// Direction identifies whether a frame crossed toward or away from a provider.
type Direction string

const (
	Send Direction = "send"
	Recv Direction = "recv"
)

// Connection is the byte-level contract shared by live provider transports.
type Connection interface {
	Connect(context.Context) error
	Send(context.Context, []byte) error
	Receive() <-chan []byte
	Close() error
	Disconnected() <-chan error
}
