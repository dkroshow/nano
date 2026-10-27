package lifecycle

type Submission string

const (
	SendNow            Submission = "send"
	QueuePrompt        Submission = "queue"
	QueueAndReconnect  Submission = "queue_reconnect"
	RefuseKeepText     Submission = "refuse_keep_text"
	RejectInvalidState Submission = "reject_invalid_state"
)

type Availability string

const (
	Available     Availability = "available"
	Busy          Availability = "busy"
	AwaitingHuman Availability = "awaiting-human"
	Unavailable   Availability = "unreachable"
	Invalid       Availability = "invalid"
)

type Permission struct {
	Submit       Submission
	Availability Availability
}

// Permissions applies connection first. Work only decides admission when ready.
// QueuePrompt never answers an interaction; RefuseKeepText preserves caller text.
func Permissions(state State) (Permission, error) {
	if !state.valid() {
		return Permission{RejectInvalidState, Invalid}, ErrInvalidState
	}
	switch state.Connection {
	case Starting, Reconnecting:
		return Permission{QueuePrompt, Busy}, nil
	case Parked:
		return Permission{QueueAndReconnect, Unavailable}, nil
	case Unreachable, Detached, Exited:
		return Permission{RefuseKeepText, Unavailable}, nil
	}
	switch state.Work {
	case Idle:
		return Permission{SendNow, Available}, nil
	case WaitingOnYou:
		return Permission{QueuePrompt, AwaitingHuman}, nil
	default:
		return Permission{QueuePrompt, Busy}, nil
	}
}
