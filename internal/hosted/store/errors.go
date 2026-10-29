package store

import (
	"errors"
)

// ErrOperationReadUnavailable reports missing durable operation identity or storage.
var ErrOperationReadUnavailable = errors.New("persisted operation read needs the control store, a hosted thread and an action")
