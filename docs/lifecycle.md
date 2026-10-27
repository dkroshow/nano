# Hosted lifecycle

`internal/lifecycle` answers two questions: is this attachment connected, and what
work has its provider reported? One pure `Transition` function applies qualified
observations to a snapshot. Callers perform the resulting queue and reconnect
effects under the existing session locks.

Connection describes Nano's attachment to the provider:

| Connection | Meaning |
| --- | --- |
| `starting` | Nano is establishing an attachment and has not yet confirmed its current provider state. |
| `ready` | The attachment is usable and the provider's current state has been confirmed. |
| `reconnecting` | Nano lost the connection and is trying to restore the attachment to the same provider thread. |
| `parked` | Automatic reconnect attempts have paused; a new prompt or reconnect request can trigger another attempt. |
| `unreachable` | The attachment cannot recover the provider thread, for example because its native identity cannot be found. |
| `detached` | The attachment was deliberately detached and stays inactive until explicit resume. |
| `exited` | Nano closed the attachment or its provider runtime ended. |

Work describes what the provider has reported:

| Work | Meaning |
| --- | --- |
| `idle` | The provider has no accepted or running turn and no pending interaction. |
| `accepted` | The provider has acknowledged a turn whose start has not yet been observed. |
| `working` | The provider is executing a turn. |
| `waiting_on_you` | The provider is blocked until a pending question or permission request is answered. |

Work is meaningful only when connection is ready. Otherwise, labels and
permissions come entirely from connection; the inactive work placeholder is not
provider evidence of idle.

| Connection / work | Prompt admission |
| --- | --- |
| ready / idle | Send, subject to earlier queued text and existing reservations |
| ready / accepted | Queue |
| ready / working | Queue; a provider that accepts running-turn input (Codex) takes an ordinary prompt into the turn |
| ready / waiting_on_you | Queue a later prompt; answer the pending interaction separately |
| starting or reconnecting | Queue |
| parked | Queue and request reconnect |
| unreachable or exited | Refuse and retain caller text |
| detached | Refuse until explicit resume |

The send path's admission check (`requireIdle`) accepts only send-now permission.
Running-turn input is a claim from the same queue, not a separate path: it takes
the first queued prompt that is neither system-origin nor held for the next turn
(`nextTurn`), names the working turn, and leaves work unchanged. A definite
refusal because that turn had ended returns the same prompt to the queue.
A transition arriving at ready/idle requests queue drain; repeated idle evidence
does not. Releasing a send reservation can retry drain without changing work.
A reservation is not provider acceptance, and an acknowledgement cannot complete
a turn.

## Evidence after restart

Every reconstructed attachment starts at `starting`. The [threads store](threads-store.md)
allocates a durable, increasing attachment epoch before provider attachment.
Native IDs, settings, and timing anchors survive as facts to locate the thread;
they cannot establish readiness or work. Explicit resume starts a new epoch and
uses the canonical current native owner. Missing or conflicting ownership fails
before provider work, instead of selecting an old hosted record.

The adjacent `codex_lifecycle.go`, `claude_lifecycle.go`, and `grok_lifecycle.go`
adapters translate each provider's own settled evidence through `Current()`.
Codex reads qualified thread state, Claude reads the shim's settled runtime and
pending interaction evidence, and Grok reads its loaded native session. Grok's
`session/new` response uses top-level `sessionId`; `session/load` can put it in
`_meta.sessionId` or `_meta["x.ai/sessionDetail"].sessionId`, or omit it. A settled,
successful response uses the requested native identity when none is returned.
A different returned identity is refused, as is a response with neither identity.
The successful provider call supplies readiness; a stored identity alone does not.
Replay, stored work anchors, and a silent transport cannot declare present activity
or completion. Newer qualified live evidence wins over an older recovery response.

Every observation carries an epoch and ordered sequence. The transition rejects
old attachments, repeated or earlier sequences, and unrequested future epochs.
An old close or callback cannot change a replacement attachment. Provider
adapters also qualify native and turn identities; sequence checks do not replace
those identity checks.

## Ownership and checks

The lifecycle package depends only on the standard library and performs no I/O.
Adapters declare capabilities and supply connection/work evidence. Application
coordination owns retries, persistence, locks, delivery, and interaction replies.
Browser and
API surfaces consume the resulting state. Child activity remains a separate read;
there is no `delegating` lifecycle state. Memory-only catalog rows show their
connection until ready, then waiting or work. When ordering sidebar rows, Nano
uses the provider's current work state before an older transcript classification
of that worker. Durable transcript fleet classifications remain a separate read.

The accepted transition rows test the production function and its permission
rules. Boundary tests reject direct session status writes and provider dispatch in the
shared functions assigned to this stage. Restart fixtures for all three providers
cover settled evidence, stale anchors and callbacks, replay, close/resume epochs,
missing identity, waiting, and live work racing recovery.
