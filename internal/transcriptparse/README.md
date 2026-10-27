# Transcript parsing

This package tree provides a pure parsing layer: an accepted model, incremental
framing, and file/wire readers for Codex, Claude and Grok. The standalone
`cmd/transcript-dump` consumes these readers. Provider notes list tested behavior
and remaining gaps: [Codex](codex/README.md), [Claude](claude/README.md),
[Grok](grok/README.md). The [consumer docs](../../docs/parsing/README.md)
describe the model, command and six native formats. History/live cut-over and
full semantic conformance remain open.

Production code can import only Go's standard library and inward packages:

```text
codex, claude, grok -> model, internal/assemble, internal/jsonl
internal/assemble -> model
model, internal/jsonl -> standard library
```

The root package has no runtime entry point. Its tests inspect imports across
every Go source file, including platform-specific files, and reject external
dependencies. Production checks also reject filesystem, network, random and
clock access. Test code may read fixtures and run Go tooling; it still cannot
import other Nano packages. Legacy comparison belongs outside this tree.

Run `go test ./internal/transcriptparse/... ./cmd/transcript-dump` to check the
module and command. This includes model examples and selected native assertions
for all three providers.
It is not yet full provider conformance or corpus parity.

## Research fixtures

Each provider's `testdata/research/` contains unchanged research inputs,
manifests and draft expected outputs. The adjacent `research-inventory.json`
records the imported case names and every file's SHA-256. There are 215 Codex,
122 Claude and 56 Grok cases (393 total). Hashes describe the already-scrubbed
fixture bytes; they do not identify original private provider data.

`TestResearchFixtureInventoryAndFraming` checks source integrity, expected
output structure and framing across record/chunk boundaries. It does **not**
compare provider semantics against the draft outputs. The drafts use different
schemas, sometimes assert only a subset of outputs, and include recipes that
stand for generated input. The loader retains those distinctions. Explicit
`assertionsOnly` expectations can name final items outside their excerpt;
complete expectations cannot. A recipe is never treated as a captured event.

`TestResearchModelConsistency` exercises every case through the readers: 784
input/reader combinations across 393 cases, including the generated oversize
recipe and Grok JSON metadata documents. JSONL is also sent through the other
reader to check unsupported-input accounting. It checks observation uniqueness,
references and owner emissions, clock scope, stable item positions, serialized
results/checkpoints and finality consistency. It does not assert that every
native meaning matches the research draft. The test reproduced a duplicate
Claude observation ID and a Codex null-usage observation pointing to an
unemitted turn; both are now covered.

Executable model expectations should be added separately, with any departures
from the drafts explained. Preserve these research originals so reviewers can
trace what changed. Passing source-integrity checks must never be reported as
passing all semantic fixtures.

## Incremental framing

`internal/jsonl.Feed` accepts prior state, bytes, a maximum row size and an
explicit closed-source flag. It returns owned state and records; it never
modifies caller input. State round-trips through JSON, including incomplete
UTF-8 bytes. Limits and state versions cannot change during continuation.

`Offset` counts all accepted input bytes. `Checkpoint` marks the end of the
last emitted record. To resume from serialized state, append bytes starting
at `Offset`: pending bytes are already in that state. A caller choosing to
re-read from `Checkpoint` must instead restore the semantic checkpoint before
the pending row and discard that row's framing state.

Records retain one-based physical line numbers and half-open byte spans. Their
`Bytes` omit LF and a paired CR; `End` includes those delimiters. Blank and
malformed rows remain records for provider decoders to classify. Invalid UTF-8
is preserved at this layer. Parsing JSON here would hide malformed evidence.

A growing source never consumes its unterminated tail. Explicit close emits
that tail with `Terminated: false`; the decoder can distinguish malformed
truncation from valid final JSON. Closing a source says nothing about a tool
or turn's execution outcome. Appending after close requires a new state.

Row limits exclude LF and its optional CR. An oversized row retains only a
bounded prefix, reports `row_too_large` and skips through its delimiter. The
following row still parses. Partial oversized rows remain pending with bounded
state. No digest of the truncated prefix is claimed as a full-record digest.
