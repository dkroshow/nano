// Package heartbeat applies wake policy through Nano's public interact seam.
// It depends on interact; the scheduler may use it, while provider, transcript,
// and lifecycle implementations must not depend on it.
package heartbeat
