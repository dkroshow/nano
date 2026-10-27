package lifecycle

import (
	"strings"
	"testing"
)

// Contract accepted at cb0f313. These exact rows exercise
// the production Transition and Permissions functions, not a second reducer.
// Every combination of the seven connection and four work values has an outcome
// for every declared event.
//
// Accepted rules: work is canonicalized to idle outside ready (other pairs
// reject); waiting queues without answering; parked queues and re-arms; detached
// requires explicit resume; ready_before_replay_settled preserves state. A ready_*
// event is a current, ordered snapshot, after replay settles. Stale attachment,
// native-thread or turn evidence is stale_evidence, including late replay.
// Drain requests consideration of the queue only; reservation/FIFO wiring is
// intentionally outside the pure package. Invalid input preserves state and
// emits no effects. boot creates a new attachment epoch using the same identity.
var transitionSpecification = []struct {
	state, event, outcome string
	drain                 bool
}{
	// starting/idle
	{"starting/idle", "boot", "starting/idle", false},
	{"starting/idle", "resume", "starting/idle", false},
	{"starting/idle", "ready_idle", "ready/idle", true},
	{"starting/idle", "ready_accepted", "ready/accepted", false},
	{"starting/idle", "ready_working", "ready/working", false},
	{"starting/idle", "ready_waiting", "ready/waiting_on_you", false},
	{"starting/idle", "accepted", "starting/idle", false},
	{"starting/idle", "started", "starting/idle", false},
	{"starting/idle", "waiting", "starting/idle", false},
	{"starting/idle", "answered", "starting/idle", false},
	{"starting/idle", "finished", "starting/idle", false},
	{"starting/idle", "link_lost", "reconnecting/idle", false},
	{"starting/idle", "retries_exhausted", "parked/idle", false},
	{"starting/idle", "retry_requested", "starting/idle", false},
	{"starting/idle", "native_missing", "unreachable/idle", false},
	{"starting/idle", "runtime_exited", "exited/idle", false},
	{"starting/idle", "detach", "detached/idle", false},
	{"starting/idle", "close", "exited/idle", false},
	{"starting/idle", "stale_evidence", "starting/idle", false},
	{"starting/idle", "stored_state", "starting/idle", false},
	{"starting/idle", "invalid_event", "reject", false},
	{"starting/idle", "ready_before_replay_settled", "starting/idle", false},
	// ready/idle
	{"ready/idle", "boot", "starting/idle", false},
	{"ready/idle", "resume", "ready/idle", false},
	{"ready/idle", "ready_idle", "ready/idle", false},
	{"ready/idle", "ready_accepted", "ready/accepted", false},
	{"ready/idle", "ready_working", "ready/working", false},
	{"ready/idle", "ready_waiting", "ready/waiting_on_you", false},
	{"ready/idle", "accepted", "ready/accepted", false},
	{"ready/idle", "started", "ready/working", false},
	{"ready/idle", "waiting", "ready/waiting_on_you", false},
	{"ready/idle", "answered", "ready/idle", false},
	{"ready/idle", "finished", "ready/idle", false},
	{"ready/idle", "link_lost", "reconnecting/idle", false},
	{"ready/idle", "retries_exhausted", "ready/idle", false},
	{"ready/idle", "retry_requested", "ready/idle", false},
	{"ready/idle", "native_missing", "unreachable/idle", false},
	{"ready/idle", "runtime_exited", "exited/idle", false},
	{"ready/idle", "detach", "detached/idle", false},
	{"ready/idle", "close", "exited/idle", false},
	{"ready/idle", "stale_evidence", "ready/idle", false},
	{"ready/idle", "stored_state", "ready/idle", false},
	{"ready/idle", "invalid_event", "reject", false},
	{"ready/idle", "ready_before_replay_settled", "ready/idle", false},
	// ready/accepted
	{"ready/accepted", "boot", "starting/idle", false},
	{"ready/accepted", "resume", "ready/accepted", false},
	{"ready/accepted", "ready_idle", "ready/idle", true},
	{"ready/accepted", "ready_accepted", "ready/accepted", false},
	{"ready/accepted", "ready_working", "ready/working", false},
	{"ready/accepted", "ready_waiting", "ready/waiting_on_you", false},
	{"ready/accepted", "accepted", "ready/accepted", false},
	{"ready/accepted", "started", "ready/working", false},
	{"ready/accepted", "waiting", "ready/waiting_on_you", false},
	{"ready/accepted", "answered", "ready/accepted", false},
	{"ready/accepted", "finished", "ready/idle", true},
	{"ready/accepted", "link_lost", "reconnecting/idle", false},
	{"ready/accepted", "retries_exhausted", "ready/accepted", false},
	{"ready/accepted", "retry_requested", "ready/accepted", false},
	{"ready/accepted", "native_missing", "unreachable/idle", false},
	{"ready/accepted", "runtime_exited", "exited/idle", false},
	{"ready/accepted", "detach", "detached/idle", false},
	{"ready/accepted", "close", "exited/idle", false},
	{"ready/accepted", "stale_evidence", "ready/accepted", false},
	{"ready/accepted", "stored_state", "ready/accepted", false},
	{"ready/accepted", "invalid_event", "reject", false},
	{"ready/accepted", "ready_before_replay_settled", "ready/accepted", false},
	// ready/working
	{"ready/working", "boot", "starting/idle", false},
	{"ready/working", "resume", "ready/working", false},
	{"ready/working", "ready_idle", "ready/idle", true},
	{"ready/working", "ready_accepted", "ready/accepted", false},
	{"ready/working", "ready_working", "ready/working", false},
	{"ready/working", "ready_waiting", "ready/waiting_on_you", false},
	{"ready/working", "accepted", "ready/working", false},
	{"ready/working", "started", "ready/working", false},
	{"ready/working", "waiting", "ready/waiting_on_you", false},
	{"ready/working", "answered", "ready/working", false},
	{"ready/working", "finished", "ready/idle", true},
	{"ready/working", "link_lost", "reconnecting/idle", false},
	{"ready/working", "retries_exhausted", "ready/working", false},
	{"ready/working", "retry_requested", "ready/working", false},
	{"ready/working", "native_missing", "unreachable/idle", false},
	{"ready/working", "runtime_exited", "exited/idle", false},
	{"ready/working", "detach", "detached/idle", false},
	{"ready/working", "close", "exited/idle", false},
	{"ready/working", "stale_evidence", "ready/working", false},
	{"ready/working", "stored_state", "ready/working", false},
	{"ready/working", "invalid_event", "reject", false},
	{"ready/working", "ready_before_replay_settled", "ready/working", false},
	// ready/waiting_on_you
	{"ready/waiting_on_you", "boot", "starting/idle", false},
	{"ready/waiting_on_you", "resume", "ready/waiting_on_you", false},
	{"ready/waiting_on_you", "ready_idle", "ready/idle", true},
	{"ready/waiting_on_you", "ready_accepted", "ready/accepted", false},
	{"ready/waiting_on_you", "ready_working", "ready/working", false},
	{"ready/waiting_on_you", "ready_waiting", "ready/waiting_on_you", false},
	{"ready/waiting_on_you", "accepted", "ready/waiting_on_you", false},
	{"ready/waiting_on_you", "started", "ready/waiting_on_you", false},
	{"ready/waiting_on_you", "waiting", "ready/waiting_on_you", false},
	{"ready/waiting_on_you", "answered", "ready/working", false},
	{"ready/waiting_on_you", "finished", "ready/idle", true},
	{"ready/waiting_on_you", "link_lost", "reconnecting/idle", false},
	{"ready/waiting_on_you", "retries_exhausted", "ready/waiting_on_you", false},
	{"ready/waiting_on_you", "retry_requested", "ready/waiting_on_you", false},
	{"ready/waiting_on_you", "native_missing", "unreachable/idle", false},
	{"ready/waiting_on_you", "runtime_exited", "exited/idle", false},
	{"ready/waiting_on_you", "detach", "detached/idle", false},
	{"ready/waiting_on_you", "close", "exited/idle", false},
	{"ready/waiting_on_you", "stale_evidence", "ready/waiting_on_you", false},
	{"ready/waiting_on_you", "stored_state", "ready/waiting_on_you", false},
	{"ready/waiting_on_you", "invalid_event", "reject", false},
	{"ready/waiting_on_you", "ready_before_replay_settled", "ready/waiting_on_you", false},
	// reconnecting/idle
	{"reconnecting/idle", "boot", "starting/idle", false},
	{"reconnecting/idle", "resume", "reconnecting/idle", false},
	{"reconnecting/idle", "ready_idle", "ready/idle", true},
	{"reconnecting/idle", "ready_accepted", "ready/accepted", false},
	{"reconnecting/idle", "ready_working", "ready/working", false},
	{"reconnecting/idle", "ready_waiting", "ready/waiting_on_you", false},
	{"reconnecting/idle", "accepted", "reconnecting/idle", false},
	{"reconnecting/idle", "started", "reconnecting/idle", false},
	{"reconnecting/idle", "waiting", "reconnecting/idle", false},
	{"reconnecting/idle", "answered", "reconnecting/idle", false},
	{"reconnecting/idle", "finished", "reconnecting/idle", false},
	{"reconnecting/idle", "link_lost", "reconnecting/idle", false},
	{"reconnecting/idle", "retries_exhausted", "parked/idle", false},
	{"reconnecting/idle", "retry_requested", "reconnecting/idle", false},
	{"reconnecting/idle", "native_missing", "unreachable/idle", false},
	{"reconnecting/idle", "runtime_exited", "exited/idle", false},
	{"reconnecting/idle", "detach", "detached/idle", false},
	{"reconnecting/idle", "close", "exited/idle", false},
	{"reconnecting/idle", "stale_evidence", "reconnecting/idle", false},
	{"reconnecting/idle", "stored_state", "reconnecting/idle", false},
	{"reconnecting/idle", "invalid_event", "reject", false},
	{"reconnecting/idle", "ready_before_replay_settled", "reconnecting/idle", false},
	// parked/idle
	{"parked/idle", "boot", "starting/idle", false},
	{"parked/idle", "resume", "starting/idle", false},
	{"parked/idle", "ready_idle", "ready/idle", true},
	{"parked/idle", "ready_accepted", "ready/accepted", false},
	{"parked/idle", "ready_working", "ready/working", false},
	{"parked/idle", "ready_waiting", "ready/waiting_on_you", false},
	{"parked/idle", "accepted", "parked/idle", false},
	{"parked/idle", "started", "parked/idle", false},
	{"parked/idle", "waiting", "parked/idle", false},
	{"parked/idle", "answered", "parked/idle", false},
	{"parked/idle", "finished", "parked/idle", false},
	{"parked/idle", "link_lost", "parked/idle", false},
	{"parked/idle", "retries_exhausted", "parked/idle", false},
	{"parked/idle", "retry_requested", "reconnecting/idle", false},
	{"parked/idle", "native_missing", "unreachable/idle", false},
	{"parked/idle", "runtime_exited", "exited/idle", false},
	{"parked/idle", "detach", "detached/idle", false},
	{"parked/idle", "close", "exited/idle", false},
	{"parked/idle", "stale_evidence", "parked/idle", false},
	{"parked/idle", "stored_state", "parked/idle", false},
	{"parked/idle", "invalid_event", "reject", false},
	{"parked/idle", "ready_before_replay_settled", "parked/idle", false},
	// unreachable/idle
	{"unreachable/idle", "boot", "starting/idle", false},
	{"unreachable/idle", "resume", "starting/idle", false},
	{"unreachable/idle", "ready_idle", "unreachable/idle", false},
	{"unreachable/idle", "ready_accepted", "unreachable/idle", false},
	{"unreachable/idle", "ready_working", "unreachable/idle", false},
	{"unreachable/idle", "ready_waiting", "unreachable/idle", false},
	{"unreachable/idle", "accepted", "unreachable/idle", false},
	{"unreachable/idle", "started", "unreachable/idle", false},
	{"unreachable/idle", "waiting", "unreachable/idle", false},
	{"unreachable/idle", "answered", "unreachable/idle", false},
	{"unreachable/idle", "finished", "unreachable/idle", false},
	{"unreachable/idle", "link_lost", "unreachable/idle", false},
	{"unreachable/idle", "retries_exhausted", "unreachable/idle", false},
	{"unreachable/idle", "retry_requested", "unreachable/idle", false},
	{"unreachable/idle", "native_missing", "unreachable/idle", false},
	{"unreachable/idle", "runtime_exited", "exited/idle", false},
	{"unreachable/idle", "detach", "detached/idle", false},
	{"unreachable/idle", "close", "exited/idle", false},
	{"unreachable/idle", "stale_evidence", "unreachable/idle", false},
	{"unreachable/idle", "stored_state", "unreachable/idle", false},
	{"unreachable/idle", "invalid_event", "reject", false},
	{"unreachable/idle", "ready_before_replay_settled", "unreachable/idle", false},
	// detached/idle
	{"detached/idle", "boot", "starting/idle", false},
	{"detached/idle", "resume", "starting/idle", false},
	{"detached/idle", "ready_idle", "detached/idle", false},
	{"detached/idle", "ready_accepted", "detached/idle", false},
	{"detached/idle", "ready_working", "detached/idle", false},
	{"detached/idle", "ready_waiting", "detached/idle", false},
	{"detached/idle", "accepted", "detached/idle", false},
	{"detached/idle", "started", "detached/idle", false},
	{"detached/idle", "waiting", "detached/idle", false},
	{"detached/idle", "answered", "detached/idle", false},
	{"detached/idle", "finished", "detached/idle", false},
	{"detached/idle", "link_lost", "detached/idle", false},
	{"detached/idle", "retries_exhausted", "detached/idle", false},
	{"detached/idle", "retry_requested", "detached/idle", false},
	{"detached/idle", "native_missing", "detached/idle", false},
	{"detached/idle", "runtime_exited", "detached/idle", false},
	{"detached/idle", "detach", "detached/idle", false},
	{"detached/idle", "close", "exited/idle", false},
	{"detached/idle", "stale_evidence", "detached/idle", false},
	{"detached/idle", "stored_state", "detached/idle", false},
	{"detached/idle", "invalid_event", "reject", false},
	{"detached/idle", "ready_before_replay_settled", "detached/idle", false},
	// exited/idle
	{"exited/idle", "boot", "starting/idle", false},
	{"exited/idle", "resume", "starting/idle", false},
	{"exited/idle", "ready_idle", "exited/idle", false},
	{"exited/idle", "ready_accepted", "exited/idle", false},
	{"exited/idle", "ready_working", "exited/idle", false},
	{"exited/idle", "ready_waiting", "exited/idle", false},
	{"exited/idle", "accepted", "exited/idle", false},
	{"exited/idle", "started", "exited/idle", false},
	{"exited/idle", "waiting", "exited/idle", false},
	{"exited/idle", "answered", "exited/idle", false},
	{"exited/idle", "finished", "exited/idle", false},
	{"exited/idle", "link_lost", "exited/idle", false},
	{"exited/idle", "retries_exhausted", "exited/idle", false},
	{"exited/idle", "retry_requested", "exited/idle", false},
	{"exited/idle", "native_missing", "exited/idle", false},
	{"exited/idle", "runtime_exited", "exited/idle", false},
	{"exited/idle", "detach", "exited/idle", false},
	{"exited/idle", "close", "exited/idle", false},
	{"exited/idle", "stale_evidence", "exited/idle", false},
	{"exited/idle", "stored_state", "exited/idle", false},
	{"exited/idle", "invalid_event", "reject", false},
	{"exited/idle", "ready_before_replay_settled", "exited/idle", false},
	// starting/accepted
	{"starting/accepted", "boot", "reject", false},
	{"starting/accepted", "resume", "reject", false},
	{"starting/accepted", "ready_idle", "reject", false},
	{"starting/accepted", "ready_accepted", "reject", false},
	{"starting/accepted", "ready_working", "reject", false},
	{"starting/accepted", "ready_waiting", "reject", false},
	{"starting/accepted", "accepted", "reject", false},
	{"starting/accepted", "started", "reject", false},
	{"starting/accepted", "waiting", "reject", false},
	{"starting/accepted", "answered", "reject", false},
	{"starting/accepted", "finished", "reject", false},
	{"starting/accepted", "link_lost", "reject", false},
	{"starting/accepted", "retries_exhausted", "reject", false},
	{"starting/accepted", "retry_requested", "reject", false},
	{"starting/accepted", "native_missing", "reject", false},
	{"starting/accepted", "runtime_exited", "reject", false},
	{"starting/accepted", "detach", "reject", false},
	{"starting/accepted", "close", "reject", false},
	{"starting/accepted", "stale_evidence", "reject", false},
	{"starting/accepted", "stored_state", "reject", false},
	{"starting/accepted", "invalid_event", "reject", false},
	{"starting/accepted", "ready_before_replay_settled", "reject", false},
	// starting/working
	{"starting/working", "boot", "reject", false},
	{"starting/working", "resume", "reject", false},
	{"starting/working", "ready_idle", "reject", false},
	{"starting/working", "ready_accepted", "reject", false},
	{"starting/working", "ready_working", "reject", false},
	{"starting/working", "ready_waiting", "reject", false},
	{"starting/working", "accepted", "reject", false},
	{"starting/working", "started", "reject", false},
	{"starting/working", "waiting", "reject", false},
	{"starting/working", "answered", "reject", false},
	{"starting/working", "finished", "reject", false},
	{"starting/working", "link_lost", "reject", false},
	{"starting/working", "retries_exhausted", "reject", false},
	{"starting/working", "retry_requested", "reject", false},
	{"starting/working", "native_missing", "reject", false},
	{"starting/working", "runtime_exited", "reject", false},
	{"starting/working", "detach", "reject", false},
	{"starting/working", "close", "reject", false},
	{"starting/working", "stale_evidence", "reject", false},
	{"starting/working", "stored_state", "reject", false},
	{"starting/working", "invalid_event", "reject", false},
	{"starting/working", "ready_before_replay_settled", "reject", false},
	// starting/waiting_on_you
	{"starting/waiting_on_you", "boot", "reject", false},
	{"starting/waiting_on_you", "resume", "reject", false},
	{"starting/waiting_on_you", "ready_idle", "reject", false},
	{"starting/waiting_on_you", "ready_accepted", "reject", false},
	{"starting/waiting_on_you", "ready_working", "reject", false},
	{"starting/waiting_on_you", "ready_waiting", "reject", false},
	{"starting/waiting_on_you", "accepted", "reject", false},
	{"starting/waiting_on_you", "started", "reject", false},
	{"starting/waiting_on_you", "waiting", "reject", false},
	{"starting/waiting_on_you", "answered", "reject", false},
	{"starting/waiting_on_you", "finished", "reject", false},
	{"starting/waiting_on_you", "link_lost", "reject", false},
	{"starting/waiting_on_you", "retries_exhausted", "reject", false},
	{"starting/waiting_on_you", "retry_requested", "reject", false},
	{"starting/waiting_on_you", "native_missing", "reject", false},
	{"starting/waiting_on_you", "runtime_exited", "reject", false},
	{"starting/waiting_on_you", "detach", "reject", false},
	{"starting/waiting_on_you", "close", "reject", false},
	{"starting/waiting_on_you", "stale_evidence", "reject", false},
	{"starting/waiting_on_you", "stored_state", "reject", false},
	{"starting/waiting_on_you", "invalid_event", "reject", false},
	{"starting/waiting_on_you", "ready_before_replay_settled", "reject", false},
	// reconnecting/accepted
	{"reconnecting/accepted", "boot", "reject", false},
	{"reconnecting/accepted", "resume", "reject", false},
	{"reconnecting/accepted", "ready_idle", "reject", false},
	{"reconnecting/accepted", "ready_accepted", "reject", false},
	{"reconnecting/accepted", "ready_working", "reject", false},
	{"reconnecting/accepted", "ready_waiting", "reject", false},
	{"reconnecting/accepted", "accepted", "reject", false},
	{"reconnecting/accepted", "started", "reject", false},
	{"reconnecting/accepted", "waiting", "reject", false},
	{"reconnecting/accepted", "answered", "reject", false},
	{"reconnecting/accepted", "finished", "reject", false},
	{"reconnecting/accepted", "link_lost", "reject", false},
	{"reconnecting/accepted", "retries_exhausted", "reject", false},
	{"reconnecting/accepted", "retry_requested", "reject", false},
	{"reconnecting/accepted", "native_missing", "reject", false},
	{"reconnecting/accepted", "runtime_exited", "reject", false},
	{"reconnecting/accepted", "detach", "reject", false},
	{"reconnecting/accepted", "close", "reject", false},
	{"reconnecting/accepted", "stale_evidence", "reject", false},
	{"reconnecting/accepted", "stored_state", "reject", false},
	{"reconnecting/accepted", "invalid_event", "reject", false},
	{"reconnecting/accepted", "ready_before_replay_settled", "reject", false},
	// reconnecting/working
	{"reconnecting/working", "boot", "reject", false},
	{"reconnecting/working", "resume", "reject", false},
	{"reconnecting/working", "ready_idle", "reject", false},
	{"reconnecting/working", "ready_accepted", "reject", false},
	{"reconnecting/working", "ready_working", "reject", false},
	{"reconnecting/working", "ready_waiting", "reject", false},
	{"reconnecting/working", "accepted", "reject", false},
	{"reconnecting/working", "started", "reject", false},
	{"reconnecting/working", "waiting", "reject", false},
	{"reconnecting/working", "answered", "reject", false},
	{"reconnecting/working", "finished", "reject", false},
	{"reconnecting/working", "link_lost", "reject", false},
	{"reconnecting/working", "retries_exhausted", "reject", false},
	{"reconnecting/working", "retry_requested", "reject", false},
	{"reconnecting/working", "native_missing", "reject", false},
	{"reconnecting/working", "runtime_exited", "reject", false},
	{"reconnecting/working", "detach", "reject", false},
	{"reconnecting/working", "close", "reject", false},
	{"reconnecting/working", "stale_evidence", "reject", false},
	{"reconnecting/working", "stored_state", "reject", false},
	{"reconnecting/working", "invalid_event", "reject", false},
	{"reconnecting/working", "ready_before_replay_settled", "reject", false},
	// reconnecting/waiting_on_you
	{"reconnecting/waiting_on_you", "boot", "reject", false},
	{"reconnecting/waiting_on_you", "resume", "reject", false},
	{"reconnecting/waiting_on_you", "ready_idle", "reject", false},
	{"reconnecting/waiting_on_you", "ready_accepted", "reject", false},
	{"reconnecting/waiting_on_you", "ready_working", "reject", false},
	{"reconnecting/waiting_on_you", "ready_waiting", "reject", false},
	{"reconnecting/waiting_on_you", "accepted", "reject", false},
	{"reconnecting/waiting_on_you", "started", "reject", false},
	{"reconnecting/waiting_on_you", "waiting", "reject", false},
	{"reconnecting/waiting_on_you", "answered", "reject", false},
	{"reconnecting/waiting_on_you", "finished", "reject", false},
	{"reconnecting/waiting_on_you", "link_lost", "reject", false},
	{"reconnecting/waiting_on_you", "retries_exhausted", "reject", false},
	{"reconnecting/waiting_on_you", "retry_requested", "reject", false},
	{"reconnecting/waiting_on_you", "native_missing", "reject", false},
	{"reconnecting/waiting_on_you", "runtime_exited", "reject", false},
	{"reconnecting/waiting_on_you", "detach", "reject", false},
	{"reconnecting/waiting_on_you", "close", "reject", false},
	{"reconnecting/waiting_on_you", "stale_evidence", "reject", false},
	{"reconnecting/waiting_on_you", "stored_state", "reject", false},
	{"reconnecting/waiting_on_you", "invalid_event", "reject", false},
	{"reconnecting/waiting_on_you", "ready_before_replay_settled", "reject", false},
	// parked/accepted
	{"parked/accepted", "boot", "reject", false},
	{"parked/accepted", "resume", "reject", false},
	{"parked/accepted", "ready_idle", "reject", false},
	{"parked/accepted", "ready_accepted", "reject", false},
	{"parked/accepted", "ready_working", "reject", false},
	{"parked/accepted", "ready_waiting", "reject", false},
	{"parked/accepted", "accepted", "reject", false},
	{"parked/accepted", "started", "reject", false},
	{"parked/accepted", "waiting", "reject", false},
	{"parked/accepted", "answered", "reject", false},
	{"parked/accepted", "finished", "reject", false},
	{"parked/accepted", "link_lost", "reject", false},
	{"parked/accepted", "retries_exhausted", "reject", false},
	{"parked/accepted", "retry_requested", "reject", false},
	{"parked/accepted", "native_missing", "reject", false},
	{"parked/accepted", "runtime_exited", "reject", false},
	{"parked/accepted", "detach", "reject", false},
	{"parked/accepted", "close", "reject", false},
	{"parked/accepted", "stale_evidence", "reject", false},
	{"parked/accepted", "stored_state", "reject", false},
	{"parked/accepted", "invalid_event", "reject", false},
	{"parked/accepted", "ready_before_replay_settled", "reject", false},
	// parked/working
	{"parked/working", "boot", "reject", false},
	{"parked/working", "resume", "reject", false},
	{"parked/working", "ready_idle", "reject", false},
	{"parked/working", "ready_accepted", "reject", false},
	{"parked/working", "ready_working", "reject", false},
	{"parked/working", "ready_waiting", "reject", false},
	{"parked/working", "accepted", "reject", false},
	{"parked/working", "started", "reject", false},
	{"parked/working", "waiting", "reject", false},
	{"parked/working", "answered", "reject", false},
	{"parked/working", "finished", "reject", false},
	{"parked/working", "link_lost", "reject", false},
	{"parked/working", "retries_exhausted", "reject", false},
	{"parked/working", "retry_requested", "reject", false},
	{"parked/working", "native_missing", "reject", false},
	{"parked/working", "runtime_exited", "reject", false},
	{"parked/working", "detach", "reject", false},
	{"parked/working", "close", "reject", false},
	{"parked/working", "stale_evidence", "reject", false},
	{"parked/working", "stored_state", "reject", false},
	{"parked/working", "invalid_event", "reject", false},
	{"parked/working", "ready_before_replay_settled", "reject", false},
	// parked/waiting_on_you
	{"parked/waiting_on_you", "boot", "reject", false},
	{"parked/waiting_on_you", "resume", "reject", false},
	{"parked/waiting_on_you", "ready_idle", "reject", false},
	{"parked/waiting_on_you", "ready_accepted", "reject", false},
	{"parked/waiting_on_you", "ready_working", "reject", false},
	{"parked/waiting_on_you", "ready_waiting", "reject", false},
	{"parked/waiting_on_you", "accepted", "reject", false},
	{"parked/waiting_on_you", "started", "reject", false},
	{"parked/waiting_on_you", "waiting", "reject", false},
	{"parked/waiting_on_you", "answered", "reject", false},
	{"parked/waiting_on_you", "finished", "reject", false},
	{"parked/waiting_on_you", "link_lost", "reject", false},
	{"parked/waiting_on_you", "retries_exhausted", "reject", false},
	{"parked/waiting_on_you", "retry_requested", "reject", false},
	{"parked/waiting_on_you", "native_missing", "reject", false},
	{"parked/waiting_on_you", "runtime_exited", "reject", false},
	{"parked/waiting_on_you", "detach", "reject", false},
	{"parked/waiting_on_you", "close", "reject", false},
	{"parked/waiting_on_you", "stale_evidence", "reject", false},
	{"parked/waiting_on_you", "stored_state", "reject", false},
	{"parked/waiting_on_you", "invalid_event", "reject", false},
	{"parked/waiting_on_you", "ready_before_replay_settled", "reject", false},
	// unreachable/accepted
	{"unreachable/accepted", "boot", "reject", false},
	{"unreachable/accepted", "resume", "reject", false},
	{"unreachable/accepted", "ready_idle", "reject", false},
	{"unreachable/accepted", "ready_accepted", "reject", false},
	{"unreachable/accepted", "ready_working", "reject", false},
	{"unreachable/accepted", "ready_waiting", "reject", false},
	{"unreachable/accepted", "accepted", "reject", false},
	{"unreachable/accepted", "started", "reject", false},
	{"unreachable/accepted", "waiting", "reject", false},
	{"unreachable/accepted", "answered", "reject", false},
	{"unreachable/accepted", "finished", "reject", false},
	{"unreachable/accepted", "link_lost", "reject", false},
	{"unreachable/accepted", "retries_exhausted", "reject", false},
	{"unreachable/accepted", "retry_requested", "reject", false},
	{"unreachable/accepted", "native_missing", "reject", false},
	{"unreachable/accepted", "runtime_exited", "reject", false},
	{"unreachable/accepted", "detach", "reject", false},
	{"unreachable/accepted", "close", "reject", false},
	{"unreachable/accepted", "stale_evidence", "reject", false},
	{"unreachable/accepted", "stored_state", "reject", false},
	{"unreachable/accepted", "invalid_event", "reject", false},
	{"unreachable/accepted", "ready_before_replay_settled", "reject", false},
	// unreachable/working
	{"unreachable/working", "boot", "reject", false},
	{"unreachable/working", "resume", "reject", false},
	{"unreachable/working", "ready_idle", "reject", false},
	{"unreachable/working", "ready_accepted", "reject", false},
	{"unreachable/working", "ready_working", "reject", false},
	{"unreachable/working", "ready_waiting", "reject", false},
	{"unreachable/working", "accepted", "reject", false},
	{"unreachable/working", "started", "reject", false},
	{"unreachable/working", "waiting", "reject", false},
	{"unreachable/working", "answered", "reject", false},
	{"unreachable/working", "finished", "reject", false},
	{"unreachable/working", "link_lost", "reject", false},
	{"unreachable/working", "retries_exhausted", "reject", false},
	{"unreachable/working", "retry_requested", "reject", false},
	{"unreachable/working", "native_missing", "reject", false},
	{"unreachable/working", "runtime_exited", "reject", false},
	{"unreachable/working", "detach", "reject", false},
	{"unreachable/working", "close", "reject", false},
	{"unreachable/working", "stale_evidence", "reject", false},
	{"unreachable/working", "stored_state", "reject", false},
	{"unreachable/working", "invalid_event", "reject", false},
	{"unreachable/working", "ready_before_replay_settled", "reject", false},
	// unreachable/waiting_on_you
	{"unreachable/waiting_on_you", "boot", "reject", false},
	{"unreachable/waiting_on_you", "resume", "reject", false},
	{"unreachable/waiting_on_you", "ready_idle", "reject", false},
	{"unreachable/waiting_on_you", "ready_accepted", "reject", false},
	{"unreachable/waiting_on_you", "ready_working", "reject", false},
	{"unreachable/waiting_on_you", "ready_waiting", "reject", false},
	{"unreachable/waiting_on_you", "accepted", "reject", false},
	{"unreachable/waiting_on_you", "started", "reject", false},
	{"unreachable/waiting_on_you", "waiting", "reject", false},
	{"unreachable/waiting_on_you", "answered", "reject", false},
	{"unreachable/waiting_on_you", "finished", "reject", false},
	{"unreachable/waiting_on_you", "link_lost", "reject", false},
	{"unreachable/waiting_on_you", "retries_exhausted", "reject", false},
	{"unreachable/waiting_on_you", "retry_requested", "reject", false},
	{"unreachable/waiting_on_you", "native_missing", "reject", false},
	{"unreachable/waiting_on_you", "runtime_exited", "reject", false},
	{"unreachable/waiting_on_you", "detach", "reject", false},
	{"unreachable/waiting_on_you", "close", "reject", false},
	{"unreachable/waiting_on_you", "stale_evidence", "reject", false},
	{"unreachable/waiting_on_you", "stored_state", "reject", false},
	{"unreachable/waiting_on_you", "invalid_event", "reject", false},
	{"unreachable/waiting_on_you", "ready_before_replay_settled", "reject", false},
	// detached/accepted
	{"detached/accepted", "boot", "reject", false},
	{"detached/accepted", "resume", "reject", false},
	{"detached/accepted", "ready_idle", "reject", false},
	{"detached/accepted", "ready_accepted", "reject", false},
	{"detached/accepted", "ready_working", "reject", false},
	{"detached/accepted", "ready_waiting", "reject", false},
	{"detached/accepted", "accepted", "reject", false},
	{"detached/accepted", "started", "reject", false},
	{"detached/accepted", "waiting", "reject", false},
	{"detached/accepted", "answered", "reject", false},
	{"detached/accepted", "finished", "reject", false},
	{"detached/accepted", "link_lost", "reject", false},
	{"detached/accepted", "retries_exhausted", "reject", false},
	{"detached/accepted", "retry_requested", "reject", false},
	{"detached/accepted", "native_missing", "reject", false},
	{"detached/accepted", "runtime_exited", "reject", false},
	{"detached/accepted", "detach", "reject", false},
	{"detached/accepted", "close", "reject", false},
	{"detached/accepted", "stale_evidence", "reject", false},
	{"detached/accepted", "stored_state", "reject", false},
	{"detached/accepted", "invalid_event", "reject", false},
	{"detached/accepted", "ready_before_replay_settled", "reject", false},
	// detached/working
	{"detached/working", "boot", "reject", false},
	{"detached/working", "resume", "reject", false},
	{"detached/working", "ready_idle", "reject", false},
	{"detached/working", "ready_accepted", "reject", false},
	{"detached/working", "ready_working", "reject", false},
	{"detached/working", "ready_waiting", "reject", false},
	{"detached/working", "accepted", "reject", false},
	{"detached/working", "started", "reject", false},
	{"detached/working", "waiting", "reject", false},
	{"detached/working", "answered", "reject", false},
	{"detached/working", "finished", "reject", false},
	{"detached/working", "link_lost", "reject", false},
	{"detached/working", "retries_exhausted", "reject", false},
	{"detached/working", "retry_requested", "reject", false},
	{"detached/working", "native_missing", "reject", false},
	{"detached/working", "runtime_exited", "reject", false},
	{"detached/working", "detach", "reject", false},
	{"detached/working", "close", "reject", false},
	{"detached/working", "stale_evidence", "reject", false},
	{"detached/working", "stored_state", "reject", false},
	{"detached/working", "invalid_event", "reject", false},
	{"detached/working", "ready_before_replay_settled", "reject", false},
	// detached/waiting_on_you
	{"detached/waiting_on_you", "boot", "reject", false},
	{"detached/waiting_on_you", "resume", "reject", false},
	{"detached/waiting_on_you", "ready_idle", "reject", false},
	{"detached/waiting_on_you", "ready_accepted", "reject", false},
	{"detached/waiting_on_you", "ready_working", "reject", false},
	{"detached/waiting_on_you", "ready_waiting", "reject", false},
	{"detached/waiting_on_you", "accepted", "reject", false},
	{"detached/waiting_on_you", "started", "reject", false},
	{"detached/waiting_on_you", "waiting", "reject", false},
	{"detached/waiting_on_you", "answered", "reject", false},
	{"detached/waiting_on_you", "finished", "reject", false},
	{"detached/waiting_on_you", "link_lost", "reject", false},
	{"detached/waiting_on_you", "retries_exhausted", "reject", false},
	{"detached/waiting_on_you", "retry_requested", "reject", false},
	{"detached/waiting_on_you", "native_missing", "reject", false},
	{"detached/waiting_on_you", "runtime_exited", "reject", false},
	{"detached/waiting_on_you", "detach", "reject", false},
	{"detached/waiting_on_you", "close", "reject", false},
	{"detached/waiting_on_you", "stale_evidence", "reject", false},
	{"detached/waiting_on_you", "stored_state", "reject", false},
	{"detached/waiting_on_you", "invalid_event", "reject", false},
	{"detached/waiting_on_you", "ready_before_replay_settled", "reject", false},
	// exited/accepted
	{"exited/accepted", "boot", "reject", false},
	{"exited/accepted", "resume", "reject", false},
	{"exited/accepted", "ready_idle", "reject", false},
	{"exited/accepted", "ready_accepted", "reject", false},
	{"exited/accepted", "ready_working", "reject", false},
	{"exited/accepted", "ready_waiting", "reject", false},
	{"exited/accepted", "accepted", "reject", false},
	{"exited/accepted", "started", "reject", false},
	{"exited/accepted", "waiting", "reject", false},
	{"exited/accepted", "answered", "reject", false},
	{"exited/accepted", "finished", "reject", false},
	{"exited/accepted", "link_lost", "reject", false},
	{"exited/accepted", "retries_exhausted", "reject", false},
	{"exited/accepted", "retry_requested", "reject", false},
	{"exited/accepted", "native_missing", "reject", false},
	{"exited/accepted", "runtime_exited", "reject", false},
	{"exited/accepted", "detach", "reject", false},
	{"exited/accepted", "close", "reject", false},
	{"exited/accepted", "stale_evidence", "reject", false},
	{"exited/accepted", "stored_state", "reject", false},
	{"exited/accepted", "invalid_event", "reject", false},
	{"exited/accepted", "ready_before_replay_settled", "reject", false},
	// exited/working
	{"exited/working", "boot", "reject", false},
	{"exited/working", "resume", "reject", false},
	{"exited/working", "ready_idle", "reject", false},
	{"exited/working", "ready_accepted", "reject", false},
	{"exited/working", "ready_working", "reject", false},
	{"exited/working", "ready_waiting", "reject", false},
	{"exited/working", "accepted", "reject", false},
	{"exited/working", "started", "reject", false},
	{"exited/working", "waiting", "reject", false},
	{"exited/working", "answered", "reject", false},
	{"exited/working", "finished", "reject", false},
	{"exited/working", "link_lost", "reject", false},
	{"exited/working", "retries_exhausted", "reject", false},
	{"exited/working", "retry_requested", "reject", false},
	{"exited/working", "native_missing", "reject", false},
	{"exited/working", "runtime_exited", "reject", false},
	{"exited/working", "detach", "reject", false},
	{"exited/working", "close", "reject", false},
	{"exited/working", "stale_evidence", "reject", false},
	{"exited/working", "stored_state", "reject", false},
	{"exited/working", "invalid_event", "reject", false},
	{"exited/working", "ready_before_replay_settled", "reject", false},
	// exited/waiting_on_you
	{"exited/waiting_on_you", "boot", "reject", false},
	{"exited/waiting_on_you", "resume", "reject", false},
	{"exited/waiting_on_you", "ready_idle", "reject", false},
	{"exited/waiting_on_you", "ready_accepted", "reject", false},
	{"exited/waiting_on_you", "ready_working", "reject", false},
	{"exited/waiting_on_you", "ready_waiting", "reject", false},
	{"exited/waiting_on_you", "accepted", "reject", false},
	{"exited/waiting_on_you", "started", "reject", false},
	{"exited/waiting_on_you", "waiting", "reject", false},
	{"exited/waiting_on_you", "answered", "reject", false},
	{"exited/waiting_on_you", "finished", "reject", false},
	{"exited/waiting_on_you", "link_lost", "reject", false},
	{"exited/waiting_on_you", "retries_exhausted", "reject", false},
	{"exited/waiting_on_you", "retry_requested", "reject", false},
	{"exited/waiting_on_you", "native_missing", "reject", false},
	{"exited/waiting_on_you", "runtime_exited", "reject", false},
	{"exited/waiting_on_you", "detach", "reject", false},
	{"exited/waiting_on_you", "close", "reject", false},
	{"exited/waiting_on_you", "stale_evidence", "reject", false},
	{"exited/waiting_on_you", "stored_state", "reject", false},
	{"exited/waiting_on_you", "invalid_event", "reject", false},
	{"exited/waiting_on_you", "ready_before_replay_settled", "reject", false},
}

// The waiting/parked/detached cells and waiting-before-busy availability were
// accepted in e8fe9874. A refusal keeps caller text. requireIdle and
// delivery reservations will be wired later using these lifecycle permissions.
var permissionSpecification = []struct{ state, submit, availability string }{
	{"starting/idle", "queue", "busy"},
	{"ready/idle", "send", "available"},
	{"ready/accepted", "queue", "busy"},
	{"ready/working", "queue", "busy"},
	{"ready/waiting_on_you", "queue", "awaiting-human"},
	{"reconnecting/idle", "queue", "busy"},
	{"parked/idle", "queue_reconnect", "unreachable"},
	{"unreachable/idle", "refuse_keep_text", "unreachable"},
	{"detached/idle", "refuse_keep_text", "unreachable"},
	{"exited/idle", "refuse_keep_text", "unreachable"},
	{"starting/accepted", "reject_invalid_state", "invalid"},
	{"starting/working", "reject_invalid_state", "invalid"},
	{"starting/waiting_on_you", "reject_invalid_state", "invalid"},
	{"reconnecting/accepted", "reject_invalid_state", "invalid"},
	{"reconnecting/working", "reject_invalid_state", "invalid"},
	{"reconnecting/waiting_on_you", "reject_invalid_state", "invalid"},
	{"parked/accepted", "reject_invalid_state", "invalid"},
	{"parked/working", "reject_invalid_state", "invalid"},
	{"parked/waiting_on_you", "reject_invalid_state", "invalid"},
	{"unreachable/accepted", "reject_invalid_state", "invalid"},
	{"unreachable/working", "reject_invalid_state", "invalid"},
	{"unreachable/waiting_on_you", "reject_invalid_state", "invalid"},
	{"detached/accepted", "reject_invalid_state", "invalid"},
	{"detached/working", "reject_invalid_state", "invalid"},
	{"detached/waiting_on_you", "reject_invalid_state", "invalid"},
	{"exited/accepted", "reject_invalid_state", "invalid"},
	{"exited/working", "reject_invalid_state", "invalid"},
	{"exited/waiting_on_you", "reject_invalid_state", "invalid"},
}

func TestTransitionTable(t *testing.T) {
	connections := []string{"starting", "ready", "reconnecting", "parked", "unreachable", "detached", "exited"}
	workStates := []string{"idle", "accepted", "working", "waiting_on_you"}
	events := []string{
		"boot", "resume", "ready_idle", "ready_accepted", "ready_working", "ready_waiting",
		"accepted", "started", "waiting", "answered", "finished", "link_lost", "retries_exhausted",
		"retry_requested", "native_missing", "runtime_exited", "detach", "close", "stale_evidence",
		"stored_state", "invalid_event", "ready_before_replay_settled",
	}
	missing := map[[2]string]bool{}
	states := map[string]bool{}
	for _, connection := range connections {
		for _, work := range workStates {
			state := connection + "/" + work
			states[state] = true
			for _, event := range events {
				missing[[2]string{state, event}] = true
			}
		}
	}
	for _, row := range transitionSpecification {
		t.Run(row.state+"/"+row.event, func(t *testing.T) {
			key := [2]string{row.state, row.event}
			if !missing[key] {
				t.Fatal("unknown or duplicate state/event pair")
			}
			delete(missing, key)
			invalid := !strings.HasPrefix(row.state, "ready/") && !strings.HasSuffix(row.state, "/idle")
			if (row.outcome == "reject") != (invalid || row.event == "invalid_event") {
				t.Fatal("invalid state/event must reject without effects")
			}
			if row.outcome != "reject" && !states[row.outcome] {
				t.Fatal("unknown resulting state")
			}
			if row.drain != (row.outcome == "ready/idle" && row.state != "ready/idle") {
				t.Fatal("drain occurs exactly on arrival at ready+idle")
			}
			current := Snapshot{State: specificationState(row.state), Epoch: 4, Sequence: 17}
			observation := Observation{Event: Event(row.event), Epoch: 4, Sequence: 18}
			if observation.Event == EventBoot || observation.Event == EventResume {
				observation.Epoch, observation.Sequence = 5, 0
			}
			got, err := Transition(current, observation)
			if row.outcome == "reject" {
				if err == nil || got != (Outcome{Snapshot: current}) {
					t.Fatalf("rejection must preserve the snapshot without effects: got %+v, err %v", got, err)
				}
			} else if err != nil || got.Snapshot.State.String() != row.outcome || got.Drain != row.drain {
				t.Fatalf("got %+v, err %v; want state %s, drain %t", got, err, row.outcome, row.drain)
			}
			if invalid {
				return
			}
			switch row.event {
			case "boot":
				if row.outcome != "starting/idle" {
					t.Fatal("restart cannot restore stored activity")
				}
			case "stored_state", "stale_evidence", "ready_before_replay_settled":
				if row.outcome != row.state {
					t.Fatal("unqualified evidence cannot change current work")
				}
			case "close":
				if row.outcome != "exited/idle" {
					t.Fatal("close must end the attachment")
				}
			case "accepted":
				if (row.state == "ready/working" || row.state == "ready/waiting_on_you") && row.outcome != row.state {
					t.Fatal("late acceptance must not regress work")
				}
			}
			if (row.state == "detached/idle" || row.state == "exited/idle") && row.event != "boot" && row.event != "resume" && row.event != "close" && row.event != "invalid_event" && row.outcome != row.state {
				t.Fatal("provider callback cannot reopen an ended attachment")
			}
		})
	}
	for pair := range missing {
		t.Errorf("missing state/event pair %v", pair)
	}
}

func TestPermissionTable(t *testing.T) {
	seen := map[string]bool{}
	for _, row := range permissionSpecification {
		if seen[row.state] {
			t.Fatalf("duplicate permission state %s", row.state)
		}
		seen[row.state] = true
		t.Run(row.state, func(t *testing.T) {
			got, err := Permissions(specificationState(row.state))
			if string(got.Submit) != row.submit || string(got.Availability) != row.availability || (err != nil) != (row.submit == "reject_invalid_state") {
				t.Fatalf("got %+v, err %v; want %s/%s", got, err, row.submit, row.availability)
			}
			switch row.state {
			case "ready/idle":
				if row.submit != "send" || row.availability != "available" {
					t.Fatal("only ready+idle permits sending")
				}
			case "starting/idle", "ready/accepted", "ready/working", "reconnecting/idle":
				if row.submit != "queue" || row.availability != "busy" {
					t.Fatal("temporary unavailability must queue")
				}
			case "ready/waiting_on_you":
				if row.submit != "queue" || row.availability != "awaiting-human" {
					t.Fatal("waiting queues while retaining human attention")
				}
			case "parked/idle":
				if row.submit != "queue_reconnect" || row.availability != "unreachable" {
					t.Fatal("parked prompt queues and re-arms")
				}
			case "unreachable/idle", "detached/idle", "exited/idle":
				if row.submit != "refuse_keep_text" || row.availability != "unreachable" {
					t.Fatal("refusal must retain caller text")
				}
			default:
				if row.submit != "reject_invalid_state" || row.availability != "invalid" {
					t.Fatal("invalid state fails closed")
				}
			}
		})
	}
	for _, row := range transitionSpecification {
		if !seen[row.state] {
			t.Errorf("missing permissions for %s", row.state)
		}
	}
}
