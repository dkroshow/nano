package lifecycle

import "errors"

type Connection string

const (
	Starting     Connection = "starting"
	Ready        Connection = "ready"
	Reconnecting Connection = "reconnecting"
	Parked       Connection = "parked"
	Unreachable  Connection = "unreachable"
	Detached     Connection = "detached"
	Exited       Connection = "exited"
)

type Work string

const (
	Idle         Work = "idle"
	Accepted     Work = "accepted"
	Working      Work = "working"
	WaitingOnYou Work = "waiting_on_you"
)

type State struct {
	Connection Connection
	Work       Work
}

func (s State) String() string { return string(s.Connection) + "/" + string(s.Work) }

// Label must not turn a disconnected idle placeholder into an idle work label.
func (s State) Label() string {
	if !s.Connection.valid() {
		return "invalid"
	}
	if s.Connection != Ready {
		return string(s.Connection)
	}
	if !s.Work.valid() {
		return "invalid"
	}
	return string(s.Work)
}

func (c Connection) valid() bool {
	switch c {
	case Starting, Ready, Reconnecting, Parked, Unreachable, Detached, Exited:
		return true
	}
	return false
}

func (w Work) valid() bool {
	switch w {
	case Idle, Accepted, Working, WaitingOnYou:
		return true
	}
	return false
}

func (s State) valid() bool {
	return s.Connection.valid() && s.Work.valid() && (s.Connection == Ready || s.Work == Idle)
}

type Event string

const (
	EventBoot                     Event = "boot"
	EventResume                   Event = "resume"
	EventReadyIdle                Event = "ready_idle"
	EventReadyAccepted            Event = "ready_accepted"
	EventReadyWorking             Event = "ready_working"
	EventReadyWaiting             Event = "ready_waiting"
	EventAccepted                 Event = "accepted"
	EventStarted                  Event = "started"
	EventWaiting                  Event = "waiting"
	EventAnswered                 Event = "answered"
	EventFinished                 Event = "finished"
	EventLinkLost                 Event = "link_lost"
	EventRetriesExhausted         Event = "retries_exhausted"
	EventRetryRequested           Event = "retry_requested"
	EventNativeMissing            Event = "native_missing"
	EventRuntimeExited            Event = "runtime_exited"
	EventDetach                   Event = "detach"
	EventClose                    Event = "close"
	EventStaleEvidence            Event = "stale_evidence"
	EventStoredState              Event = "stored_state"
	EventReadyBeforeReplaySettled Event = "ready_before_replay_settled"
)

func (e Event) valid() bool {
	switch e {
	case EventBoot, EventResume, EventReadyIdle, EventReadyAccepted,
		EventReadyWorking, EventReadyWaiting, EventAccepted, EventStarted,
		EventWaiting, EventAnswered, EventFinished, EventLinkLost,
		EventRetriesExhausted, EventRetryRequested, EventNativeMissing,
		EventRuntimeExited, EventDetach, EventClose, EventStaleEvidence,
		EventStoredState, EventReadyBeforeReplaySettled:
		return true
	}
	return false
}

var (
	ErrInvalidState = errors.New("lifecycle: invalid connection/work state")
	ErrInvalidEvent = errors.New("lifecycle: invalid event")
	ErrInvalidEpoch = errors.New("lifecycle: attachment epoch must be nonzero and advance on a new attachment")
)

// Snapshot is an immutable value owned by the coordinator. Epoch identifies one
// attachment attempt; Sequence orders qualified evidence within that attempt.
// Native identity, current turn identity and pending interactions are qualified
// by the provider adapter before it emits an observation; they are not stored here.
type Snapshot struct {
	State    State
	Epoch    uint64
	Sequence uint64
}

// Observation contains qualified evidence or an explicit coordinator command.
// Boot and a resume that starts a new attachment supply a strictly newer Epoch.
// Other actionable events use the current Epoch and a strictly newer Sequence.
// The coordinator and adapter share that ordering; they must not assign a fresh
// sequence to late replay or a stale snapshot merely because it arrived later.
type Observation struct {
	Event    Event
	Epoch    uint64
	Sequence uint64
}

type Outcome struct {
	Snapshot Snapshot
	Drain    bool
}

// New reconstructs an attachment without importing last-known activity.
func New(epoch uint64) (Snapshot, error) {
	if epoch == 0 {
		return Snapshot{}, ErrInvalidEpoch
	}
	return Snapshot{State: State{Starting, Idle}, Epoch: epoch}, nil
}

// Transition performs no I/O. Its drain result requests queue consideration;
// callers still enforce reservations, ingress ordering and durable delivery.
func Transition(current Snapshot, observation Observation) (Outcome, error) {
	out := Outcome{Snapshot: current}
	if !current.State.valid() {
		return out, ErrInvalidState
	}
	if current.Epoch == 0 {
		return out, ErrInvalidEpoch
	}
	if !observation.Event.valid() {
		return out, ErrInvalidEvent
	}
	switch observation.Event {
	case EventStaleEvidence, EventStoredState, EventReadyBeforeReplaySettled:
		return out, nil
	}
	state := current.State
	resumeStartsAttachment := observation.Event == EventResume &&
		(state.Connection == Parked || state.Connection == Unreachable || state.Connection == Detached || state.Connection == Exited)
	if observation.Event == EventBoot || resumeStartsAttachment {
		if observation.Epoch <= current.Epoch {
			return out, ErrInvalidEpoch
		}
		out.Snapshot = Snapshot{State: State{Starting, Idle}, Epoch: observation.Epoch}
		return out, nil
	}
	if observation.Event == EventResume {
		return out, nil
	}
	if observation.Epoch != current.Epoch || observation.Sequence <= current.Sequence {
		return out, nil
	}
	// Ending an attachment cannot be undone by a late provider callback.
	if (state.Connection == Detached || state.Connection == Exited) && observation.Event != EventClose {
		return out, nil
	}
	switch observation.Event {
	case EventClose, EventRuntimeExited:
		state = State{Exited, Idle}
	case EventDetach:
		state = State{Detached, Idle}
	case EventNativeMissing:
		state = State{Unreachable, Idle}
	case EventLinkLost:
		if state.Connection == Starting || state.Connection == Ready {
			state = State{Reconnecting, Idle}
		}
	case EventRetriesExhausted:
		if state.Connection == Starting || state.Connection == Reconnecting {
			state = State{Parked, Idle}
		}
	case EventRetryRequested:
		if state.Connection == Parked {
			state = State{Reconnecting, Idle}
		}
	case EventReadyIdle, EventReadyAccepted, EventReadyWorking, EventReadyWaiting:
		if state.Connection != Unreachable {
			state.Connection = Ready
			switch observation.Event {
			case EventReadyIdle:
				state.Work = Idle
			case EventReadyAccepted:
				state.Work = Accepted
			case EventReadyWorking:
				state.Work = Working
			case EventReadyWaiting:
				state.Work = WaitingOnYou
			}
		}
	case EventAccepted, EventStarted, EventWaiting, EventAnswered, EventFinished:
		if state.Connection == Ready {
			switch observation.Event {
			case EventAccepted:
				if state.Work == Idle {
					state.Work = Accepted
				}
			case EventStarted:
				if state.Work != WaitingOnYou {
					state.Work = Working
				}
			case EventWaiting:
				state.Work = WaitingOnYou
			case EventAnswered:
				if state.Work == WaitingOnYou {
					state.Work = Working
				}
			case EventFinished:
				state.Work = Idle
			}
		}
	}
	out.Snapshot.State = state
	out.Snapshot.Sequence = observation.Sequence
	out.Drain = state == (State{Ready, Idle}) && current.State != state
	return out, nil
}
