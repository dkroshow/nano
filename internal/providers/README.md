# Provider contract

An adapter implements five turn operations: start, resume, submit, interrupt,
and interaction response. It also exposes lifecycle evidence and declared
capabilities. Inputs contain values; results contain normalized parser or
lifecycle evidence. There is no session pointer, HTTP response, transcript join,
or database handle in the contract.

The provider packages expose concrete adapters over a value-only Operations
edge. The adapters validate settings at the provider's declared moment,
preserve launch posture evidence, reject cross-provider scope, and keep an
accepted acknowledgement without an exact binding explicitly uncertain. A
completed turn is valid only when the same evidence contains its named,
publishable assistant final item; this prevents a provider completion from
pointing at a missing transcript row during an intermittent provider/transcript
handoff. A later complete replay is safe because transcript identity remains
stable. Runtime operation factories bind the existing native clients to these
operations; no adapter owns a hosted session.

The declarations preserve the behavior in the merged source:

| Behavior | Codex | Claude | Grok |
|---|---|---|---|
| Model override | Next turn | Live control | Creation only |
| Effort override | Next turn | Creation only | Creation only |
| Service tier | Supported | Unsupported | Unsupported |
| Permission evidence at launch | Approval policy | Permission mode | Permission mode |
| Confirmation requirement | Qualified user evidence | Exact durable completion plus live permit | Qualified user evidence |
| Interrupt settlement without user row | Supported | Unsupported | Unsupported |
| Instruction paths | Native report | Documented expectations | Unavailable |
| Tool inventory | Runtime query | Initialization snapshot | Unavailable |
| Raw protocol inspection | Available | Unavailable | Available; journaled prompts remain protected |
| Metadata | Native events | Initialization/result | Session snapshot and lineage |

`CheckSettings` preserves absent versus explicitly cleared fields and rejects
changes at an unsupported moment. `CheckInteraction` rejects unsupported
interaction kinds. `PermissionValue` selects the declared launch field. These
checks express support; they do not authorize an
operation, settle a turn, or scrub stored text. Confirmation policy belongs to
turns, which must validate the exact proof before its transaction.

Lifecycle declarations match `codex_lifecycle.go`, `claude_lifecycle.go` and
`grok_lifecycle.go` at merged base 524a7eb. Settings support comes from
`handleTurnOptions`, `handleClaudeModel` and provider startup argument handling;
interaction support from `interactions.go`, `claude_control.go` and `grok.go`;
instruction/inventory support from their existing read surfaces; confirmation
and interruption requirements from `turn_send.go` and `interrupted_settlement.go`.
Grok metadata append/lineage behavior comes from `thread_catalog_service.go`.

Parsing, identity, restoration and
history append remain in their own modules. Settings, inventory and protocol
inspection read surfaces do not expand the five-verb turn interface.

Tests cover capability behavior, evidence gaps, dependency direction, the five
verbs and table accounting. The shared-verbs guard covers app coordination and
the active submit, interrupt, and interaction entry points. Provider names stay
only in construction registries and provider protocol/read/presentation edges.
