# Inspect a native transcript

`transcript-dump` reads a local file or stdin and writes normalized transcript
JSON. It opens no Nano database and needs no running provider or Nano server.
Its only dependencies are the parsing packages and the Go standard library.

Build it from the repository root:

```sh
go build -o transcript-dump ./cmd/transcript-dump
```

For example, this fixture reports 10 response input tokens, 10 turn input
tokens and 100 cumulative thread input tokens. The selected turn summary has
10 input tokens (`input_tokens` in the native row) and 13 total tokens:

```sh
./transcript-dump internal/transcriptparse/codex/testdata/research/edge-usage-three-grains/input.jsonl --strict
```

The real output below is trimmed to selected fields. This usage-only fixture
has no content items; its observation points to the native file row.

```json
{
  "provider": "codex",
  "items": [],
  "turns": [
    {
      "nativeTurnId": "turn-example",
      "outcome": "unknown",
      "usageBasis": "native_turn_snapshot",
      "usage": {
        "input": 10,
        "output": 3,
        "total": 13
      }
    }
  ],
  "observations": [
    {
      "kind": "metadata",
      "source": {
        "kind": "file",
        "line": 1
      }
    }
  ],
  "diagnostics": null
}
```

An item is one assembled piece of content or action; a turn groups scoped
conversation evidence and usage. Diagnostics list uncertain or invalid evidence
and are `null` when none were emitted. For comparison, the final assistant item
from the paired fixture's `file.jsonl` has these fields:

```json
{
  "type": "agent_message",
  "role": "assistant",
  "text": "EXAMPLE_OK",
  "complete": true,
  "outcome": "unknown",
  "phase": "final_answer"
}
```

Here `complete` describes captured generation, `phase` is the native final-answer
label, and `outcome` does not invent an execution result for text.

The path may come before or after flags. Use `-` to read stdin. The command
never modifies its inputs.

Claude shim spool rows and `session/line` capture envelopes are detected as
Claude input. Their enclosed native records use the same parser as direct
Claude stream-json; transport clocks and replay evidence remain distinct.

For wire examples, use `--source wire` with these recorded inputs:

- Codex: protocol logger `{ts,dir,msg}` wrappers, such as the
  [paired app-server capture](../../internal/transcriptparse/codex/testdata/research/paired-first-turn/input.jsonl).
- Claude: shim spools under `~/.nanod/claude-shim/spools/`, or recorded
  `session/line` notifications. See [capture layers](claude-wire.md#three-independent-layers).
- Grok: recorded ACP frames, such as the
  [wrapped parent/child capture](../../internal/transcriptparse/grok/testdata/research/paired-repo-parent-child/manifest.md).
  Native `updates.jsonl` retains update notifications rather than the complete
  request/reply stream; see [frame types](grok-wire.md#frames-and-update-dialects).

```text
transcript-dump PATH|-
  --provider auto|codex|claude|grok
  --source file|wire
  --format json|jsonl
  --summary PATH
  --lineage PATH
  --raw
  --strict
  --max-row-bytes N
  --version
```

| Option | Meaning |
| --- | --- |
| `--provider` | Defaults to `auto`. Detection inspects structural discriminators in the first 256 KiB. It never uses message text to guess a provider. Missing or conflicting evidence requires an explicit provider. |
| `--source` | Defaults to `file`. File input uses the provider's incremental reader. Wire input is one complete native frame or supported capture wrapper per line. |
| `--format` | Defaults to `json`: final items, turns, independent observations, relations, diagnostics and source records. `jsonl` streams typed `versions`, `change`, `observation`, `diagnostic` and `record` objects. A change upserts an item/turn or adds a relation. |
| `--summary` | Read this additional Grok summary JSON document as session metadata. It does not set the model on historical turns. |
| `--lineage` | Read this additional Grok child metadata JSON document. Conflicting native parent observations produce a diagnostic. This option takes one file. |
| `--raw` | Include the bounded native item, part and observation detail retained by the parser. Safe structured metadata remains visible by default. Credentials omitted by a method decoder are not restored by this flag. This is not a byte-for-byte source export. |
| `--strict` | Return exit status 1 if any parser diagnostic was emitted. Structured output is still written. |
| `--max-row-bytes` | Maximum bytes per native record or whole JSON document; defaults to 32 MiB for this command. Oversized records are drained and diagnosed so the next record can still parse. |
| `--version` | Print the command version. Each dump also records the parser/schema/assembly versions that produced it. |

The command treats EOF as a closed capture. A valid final JSONL record without
a newline is parsed. A malformed final fragment is reported as partial; it
does not complete a turn. For Grok whole documents, `.json` paths and recognized
JSON document shapes select whole-document framing. API callers select that
framing explicitly with `Input.Format = "json"`.

Exit status 0 means the command completed; it can include warnings unless
`--strict` was set. Status 1 means strict diagnostics or a parsing/output error.
Status 2 means invalid arguments, ambiguous provider detection or an input-open
error. Diagnostics also appear on stderr as JSON, without echoing malformed
native content.

Items are sorted by their first source occurrence, and turns by their local
ordinal. Wire JSONL preserves the emitted change order. These local keys and
positions belong to the supplied capture. Native IDs remain separate. Several
observations can describe one item; summing every usage observation would be
incorrect. See the [model](model.md).

The command retains parser checkpoints and accumulated output in memory.
The record limit bounds one native row, not total transcript memory. Large-source
memory use has not been measured. The [conformance index](conformance.md) links
the fixture assertions and conservative behavior for unsupported or ambiguous
evidence. A successful dump does not establish corpus parity.

To add a regression, keep its native input and expected semantic facts in the
provider's `testdata` directory. Assert identities, observations, ordering,
completion/finality, clocks and usage independently. Prove the test fails
without the intended fix. Research snapshots use an earlier draft schema;
compare their meaning after applying the accepted model, not arbitrary local
IDs or obsolete confidence fields.

Unreadable native input or a missing sidecar returns exit code 2 and an explicit
`evidence_unavailable` error on stderr. The command reads native files or stdin;
it does not read Nano history stores or claim to restore missing source bytes.
