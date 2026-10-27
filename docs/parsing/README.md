# Native transcript parsing

The parsing module turns provider bytes into ordered source evidence, items
and turns. It does not discover files, store history, call a provider or read
a clock. Consumers supply those effects and decide cross-source identity.

Start with the [model contract](model.md) and [dump command](command.md).
The native format references preserve the bounded September 2026 studies:

| Provider | Historical files | Live wire |
| --- | --- | --- |
| Codex | [Rollouts](codex-file.md) | [App-server](codex-wire.md) |
| Claude Code | [JSONL](claude-file.md) | [Stream JSON](claude-wire.md) |
| Grok Build | [Updates and sidecars](grok-file.md) | [ACP](grok-wire.md) |

The [conformance index](conformance.md) links the executable expectations,
named differences and capture limits. Package notes explain the implemented
rules for [Codex](../../internal/transcriptparse/codex/README.md),
[Claude](../../internal/transcriptparse/claude/README.md) and
[Grok](../../internal/transcriptparse/grok/README.md). History and all three live
adapters read through the module. The old parsers were deleted during the cut-over.

A tag such as `CODEX-CALL-1` names an intentional difference from the old
parser or September research draft. Tags use `PROVIDER-TOPIC-N`; the package
notes above define them, and the [conformance index](conformance.md) links all
100 names to tests and fixtures. The fixture inputs preserve protocol shapes with synthetic identities and
paths. Expectations state the semantic facts each reader must retain.

The tests check that every input is accounted for, linked facts point to real
evidence, and restored checkpoints preserve item order. Separate tests compare
visible content and detailed native behavior. Those checks cover the imported
fixtures; they do not establish parity across the local transcript corpus.
