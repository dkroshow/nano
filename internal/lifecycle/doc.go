// Package lifecycle defines the provider-neutral state and permissions of a
// hosted thread. Its one job is to turn qualified provider observations and
// explicit attachment commands into a connection/work state and ordered effects.
//
// Lifecycle may depend only on the standard library. Provider adapters translate
// protocols into observations; coordination owns I/O, retries, locks, and queue
// delivery; surfaces consume snapshots. Lifecycle must not import those callers,
// threads storage, history, transcript, HTTP, or provider packages. It must not
// retain a session pointer, inspect rendered messages, or perform side effects.
// The adapter contract covers connection/work evidence and declared capabilities.
//
// # State and evidence
//
// Transition applies the state and evidence rules below.
// The unchanged rows in specification_test.go exercise the production function.
// The session owns one Snapshot initialized from the threads store attachment
// generation. All live writes use Transition; the old status, turnActive and
// threadStatus fields are gone. A source boundary test rejects their
// reintroduction.
//
// Connection has seven values: starting, ready, reconnecting, parked,
// unreachable, detached, exited. Work has four: idle, accepted, working,
// waiting_on_you. Work is meaningful only when ready. The canonical
// disconnected representation uses idle as an inactive placeholder, never as
// evidence that the provider is idle. The other eighteen disconnected/non-idle
// combinations are rejected. Consumers derive labels and permissions from the
// connection axis whenever it is not ready; the sidebar must never label a
// disconnected thread from its idle placeholder. State.Label and Permissions
// apply that precedence. Validation rejects malformed connection/work pairs.
//
// Every reconstructed attachment begins at starting. Stored native identities,
// settings, and timing anchors survive, but stored activity cannot promote it.
// An adapter publishes one current snapshot after recovery evidence has settled.
// Historical replay alone cannot assert present activity. A fresh transport with
// no init-dependent handshake may supply readiness, so a provider that emits init
// only after its first prompt does not deadlock. Identity changes are separate
// durable facts; readiness never invents a native identity.
//
// New starts a nonzero attachment epoch. Boot and a resume from parked,
// unreachable, detached or exited require a strictly newer epoch and reset the
// sequence. The coordinator chooses epochs that cannot repeat across restarts.
// Every other actionable observation carries the current epoch and a newer
// sequence. Earlier, duplicate and unrequested-future attachment observations
// are ignored. A close for an old attachment cannot close its replacement.
// The coordinator serializes transitions and queue effects; concurrent callers
// must not independently apply an observation to the same old snapshot.
//
// Adapters qualify native identity and current turn identity before publishing
// observations. Epoch and sequence checks here do not replace that qualification.
// A sequence is the order of qualified evidence, not a late callback's arrival
// time. Adapters emit stale_evidence for replay or unbound/mismatched turn facts,
// and ready_before_replay_settled while recovery evidence is still incomplete.
// Turn evidence must identify the current accepted turn, and answered means the
// final pending interaction has resolved. Late acceptance cannot regress working
// or waiting to accepted. A stale observation, cached state, or transport silence
// cannot complete a turn. Repeated ready+idle evidence does not trigger a drain.
// Queue drain is requested only when a transition arrives at ready+idle; the
// coordinator still checks its reservation, earlier queue entries, and unresolved
// delivery before sending. A send reservation is not provider acceptance.
//
// # Permissions and effects
//
// Send now is limited to ready+idle. Starting, accepted, working, and reconnecting
// accept queued text. Unreachable and exited refuse and retain caller text.
// Waiting on you queues a later prompt without answering the pending interaction;
// parked queues and requests reconnect; detached refuses until explicit resume.
// Reservations, requireIdle admission, and interaction bookkeeping share these
// permissions. Releasing a reservation may retry drain without changing work.
// No acknowledgement fabricates provider completion.
// A failed transition returns the unchanged snapshot and no drain effect.
// Invalid admission returns reject_invalid_state; callers retain their text.
//
// Explicit resume of exited/detached/unreachable starts a new attachment attempt
// with the same hosted identity. It never grants permission to create replacement
// provider state. A missing provider thread can remain unreachable after that
// attempt. Closed/detached attachments ignore delayed provider callbacks; only an
// explicit new attachment command leaves them. Delegating has no lifecycle state:
// child activity remains a separate derived read.
package lifecycle
