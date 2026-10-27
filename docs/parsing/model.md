# Transcript parsing model

Provider bytes enter the readers; items and turns come out. Ordered observations
retain the evidence beneath those assembled values. Missing native identity,
time or completion proof stays unknown; see the [worked examples](#10-examples-and-limits).

This is the accepted parsing contract, derived from the three provider format
studies and the parsing-layer design in September 2026. The Go definitions
live in [model](../../internal/transcriptparse/model/). The model describes
source evidence; consumers supply storage, submission bindings and display order.

The executable coverage and named differences are listed in the [Codex](../../internal/transcriptparse/codex/README.md),
[Claude](../../internal/transcriptparse/claude/README.md) and
[Grok](../../internal/transcriptparse/grok/README.md) package notes. This document
does not claim coverage of every possible native extension. The
[command](command.md) exposes the current implementation for inspection.

## 1. The units

A **source** is one caller-labelled byte stream and its incarnation. A file
replacement or a new connection has a different incarnation. The parser never
opens a path, discovers a session, reads a clock, or creates a random identity.

An **observation** records one interpretation of source evidence. It can be a
delta, snapshot, start, stop, terminal, usage sample, metadata fact, control
exchange, replay, retraction, or unknown. One record may yield several
observations, ordered by native block order. A single observation can support
several facts without copying its raw bytes.

`Item` and `Turn` provide assembled content and turn summaries alongside
ordered observations, scope, clocks, finality, usage and relationships.
History converts these at its boundary; the parsing module never imports history. Observations and parts support these
two units rather than introducing a separate graph or alias registry.

An **item** is a logical unit of content or action within one source view. Its
ordered `Observations` preserve the observations beneath it; revisions never
erase them. Items expose the assembled value for consumers that need the
current result. Observations without an item remain in the batch: an init,
usage snapshot, or replay boundary must not create an empty conversation turn.

A **turn** is a grouping supported by prompt/boundary evidence. It has its own
native identity when the provider supplies one, a parser-local key, ordered
item references, completion evidence and optional final-item references.
Claude API messages are not human turns. Grok telemetry turn numbers and
Codex root-turn IDs retain their own namespaces.

A **batch** contains version stamps, ordered changes, observations, diagnostics
and record dispositions. The next continuation state is a separate return value. Changes are typed item upserts, turn
upserts and relation additions. A full snapshot contains the current items,
turns and facts together with their observations; the command renders this.
Submission bindings live in the prompt delivery records; saved order is the
catalog's index of the provider file, and live text joins it by exact native
ids.

## 2. Scope and identity

Observations, relations, diagnostics, usage and facts with independently scoped
meaning carry scope directly. Item-local aliases and parts inherit their item
scope; they are small lists, not entries in a general registry. Scope identifies
connection, thread, turn, or item, with provider, caller source identity,
and the known native identifiers for that level and its parents. A native ID
may be null. That means unknown; it is never filled from a path, ordinal or
similar-looking text. A file with no known thread ID has a source-local thread
scope. Account/process facts use connection scope with a typed subject.

Connection identity includes an incarnation and requester/direction where
needed. RPC number `1` and string `"1"` differ. A child session carried on a
parent connection has the child's thread scope; it must not contribute parent
content, terminal evidence or usage. `Scope.NativeParentCallID` retains a
parent call identity for nested traffic without inventing a child session or
turn ID. This field is required by the Claude `parent-tool-scope` fixture:
parent and child can reuse a message ID and block index. Worker-local sidechain
content remains the worker's primary conversation, with its native sidechain flag retained.

`Alias` records namespace, scalar value, authority (`native` or `derived`) and
supporting observation refs. It inherits its item's scope. Native item, API message, block
index, tool call, event, prompt, row UUID, task and request IDs have separate
namespaces. A composite of native components is still a derived key; the
components remain available. Transport sequence is not native event identity.

Parser-local item keys are deterministic and immutable once emitted. First
evidence supplies a source-local key; later stronger evidence adds aliases,
not a replacement key. Exact scoped native identities support within-source
assembly. Neither text equality nor adjacent line equality proves identity.
Equal content with distinct native event IDs is preserved. An exact repeated
event can add a replay observation without appending its text again.

`Observation.NativeEventDigest`, when present, fingerprints immutable native
event content so history can detect conflicting identity reuse after parser
eviction. Claude hashes canonical message fields, block identity, classification
flags and question/answer overlays. Changing prompt metadata and secondary file
snapshots remain separate envelope evidence. Changed message usage still
conflicts. Result/system rows hash canonical JSON without `timestamp`. This
differs from the original source-byte digest. Missing digests supply no equality
proof.

Across file/wire sources, the parser provides evidence and explicit known
correspondence, not a universal ID promise. Consumers can apply this order:
persisted submission binding, scoped native item/block, scoped call plus facet,
event identity, then source-local identity. Conflicting strong identities do
not join. Within one source, an established local item/turn key can retain
earlier unknown-scope observations after an explicit native binding. The
original observations remain unchanged; a binding observation explains the
new assembled scope. A known native identity conflict still prevents a join. A source incarnation change requires caller-selected reset/replay;
an old checkpoint cannot silently continue on a rewritten file.

Claude fills `Item.NativeItemID` with `API message ID:block index` when the
API message ID is known. Its `claude.messageBlock` alias records `native`
authority for a provider-supplied index and `derived` (provisional) authority
for a counted index. Component aliases retain their own basis. Consumers use
`Item.ExactNativeItemID()` before strong matching and compare the exact
components at the call step. Different exact API message IDs prevent a reused
call from joining two blocks. A provisional composite cannot reject a source
or replace a stored block identity. A row UUID or call ID never substitutes
for a missing API message ID. Both readers use the same composite and basis.

### History conversion (consumer, outside the module)

History's identity receipts are indexed within a projection generation. They
map native and parser-local keys to canonical rows and retain the first event
observation and digest. A later exact replay adds its local key as an alias.
Conflicting native evidence prevents a join, even when a weaker call or local
key matches. Scope includes source incarnation and child context; equal text
provides no identity evidence. These receipts live in SQLite, outside parser
continuation state. Background file ingestion uses the module and these
receipts. All three live adapters also use the module. The cut-over evidence is
indexed in [conformance](conformance.md) and differential.

Claude's retained live cards use the same identity resolver through an in-memory
receipt index. This index holds native identity and digest evidence, not content
or continuation. Local bindings belong to the current adapter; receipts for a
removed card leave with it. A known tool result stays inside its invocation card
for the existing browser DTO, while the module retains both facets. Owner and
lifecycle assignment remain separate from this conversion.

History stores where each observation came from and the facts derived from it.
The address includes its file, line, byte range and hash. Requested native text
is read from that address after checking that the file has not changed. Missing
or changed evidence returns `evidence_unavailable`; the retained transcript view
stays readable. Each projection stores the ordered facts once, without copying
the native text or detail. Large checkpoints and facts use lossless compression
with a 512 MiB decoded-size bound. Source/scope definitions and exact scalar keys are
interned within the projection through versioned, reversible keys. The unique
index compares the exact encoded value; it does not substitute a digest for
identity. Canonical local hash strings and UUIDs store their actual bytes. Scopes
share their base context and reference local turn/item keys. Other values keep
exact JSON, including native escapes and number versus string distinctions.
Decoding restores the original values before identity resolution. A scope can
reference one JSON base and two scalar keys, never another scope graph. Item and
turn membership use integer observation references; changing views do not replace
that evidence.
Reusing an observation key with different content returns a conflict.
Observation clocks inherit matching scope and evidence keys in private timestamp
tuples. Flags preserve absent raw time, explicit null, and a distinct native
representation. Derived observation facts use a versioned binary container,
compressed only when smaller. Compressed decode verifies the declared length
and checksum. Both forms validate JSON within the same 512 MiB allocation bound
as checkpoints. Source and
observation digests retain all 32 bytes; neither is truncated.
Typed relationships retain their observation references in order. Removing a
derived item leaves independent source evidence intact; collecting its whole
projection also collects the evidence and relationships.

History can consume normalized batches inside `ApplyTranscript`'s existing
source-generation and projection guards. A rebuild leaves the previous view
available until the new rows commit. Item revisions retain their first row
position and owner, and replace body and search content in the same transaction.
Turn aliases preserve delayed native identity without copying source ordinals
into view ordinals. Private update records retain scope, clocks, completion, finality and native
usage samples separately from the compatibility DTO. Their keys and ordered
references use the same projection dictionary. Item text, detail and part
payloads are absent from these records; parts retain their exact source address.
A replay reads the existing view instead of rebuilding text from private state.
Large turn records use the same lossless bounded codec as checkpoints. Native
usage counters, sample grain and unknown versus zero survive decoding. The
compatibility DTO's existing finality labels are assigned only at that boundary;
the model has none.
Relationship records use integer references to the original local or native
values, preserving their typed namespace and order. Separate indexed columns
hold resolved history endpoints. Parser diagnostics are
projection-owned records.

`ProjectParsedFile` prepares catalog metadata and normalized transcript batches
in one source read, outside SQLite. It stores batches in a temporary compressed
spool that the caller closes after applying them. Grok can supply a separate
summary reader for catalog facts; those observations retain their own source.
Metadata reconciliation happens before the caller reloads the source guards and
applies the transcript spool. The store never reopens the native file. Evidence requests later check source
generation, device/inode, exact byte range and digest before returning native
bytes. Missing, rewritten or truncated evidence returns `evidence_unavailable`;
the retained transcript view stays readable. Legacy rows without observation
addresses use their existing guarded line reader until rebuilt. Ephemeral wire
observations do not acquire a fabricated file address.

Transcript windows read the retained item and turn columns, without evidence
file access or reparsing. Each turn's semantic-item check uses its stored
sequence bounds and turn key, so the existing page index restricts the lookup
even when source-address indexes are present.

A file checkpoint keeps provider continuation and the current typed catalog
reduction. History stores large continuation bytes in a lossless `zlib-v1`
container and restores the exact JSON before calling a reader. Small and legacy
inline checkpoints remain supported. The container declares its decoded length;
decoding stops at that length plus one byte and rejects corrupt data, unknown
codecs and lengths above 512 MiB. Storage rejects states above that bound instead
of truncating them. This allocation guard is separate from the module's advisory
4 MiB continuation limit. Checkpoint-header reads omit the body entirely.
`read_offset` includes an unfinished JSONL tail; `complete_offset`
ends at the last complete record. Appends resume at the read offset and restore
the pending bytes. Ordinary EOF does not close a growing JSONL source. Whole
JSON documents close at EOF and rebuild on change. Incompatible continuation
fails before reading appended bytes. Recovery can load checkpoint headers
without reading their JSON bodies. The background scanner prepares changed
sources, reconciles their metadata, reloads the guards, then applies the spools.
Server construction does not wait for that scan. The remaining transcript
backfill waits for the initial scan to avoid parsing the same source twice.

The existing Dev view also needs links to non-content and malformed native
records. History projects those links as `provider_event` rows from `Records`,
in source order, with their native line hash and observation membership. They
have no model Item snapshot, native item identity, or completion claim. Their
sequence positions let older page cursors exclude a later terminal record.
The standalone module output remains unchanged by this compatibility view.

## 3. Order, assembly and snapshots

Source order is `(incarnation, byte/frame position, emission index)`; native
ordinal and block/part indices are separate fields. Observation refs on an
item follow observation order, including replay receipts. Items retain their
first-established within-source position. Provider timestamps never sort them.
`Item.SourceSeq` is the one-based order of first item appearances in one source.
`Turn.StartSourceSeq` and `EndSourceSeq` bound that sequence. `Turn.Ordinal` and
`Item.TurnOrdinal` count turns within that source. Consumers assign their own
projection and delivery ordinals at the conversion boundary.

Item kinds are `user_message`, `agent_message`, `reasoning`, `tool_call`,
`tool_result`, `command`, `file_change`, `plan`, `compaction`, `interaction`,
`notice` and `provider_event`. Role is independently user, assistant, tool or
system, and omitted where inapplicable. Native role and native phase remain separate from interpretation.
The existing view categories are derived once from the semantic model; hiding
an item in a view never deletes its observations.

Provider granularity:

- **Codex:** native items are logical units with addressable content parts.
  Deltas target native item and part indices. Response/event/completion
  representations join only with scoped identity or an explicit serialization
  correspondence rule backed by a named fixture. Otherwise preserve both and
  diagnose ambiguity. Resume coverage (`full`, `summary`, `not_loaded`) is a
  fact; an incomplete snapshot cannot erase unobserved content.
- **Claude:** one item per typed API-message block, with an API-message group.
  Prefer native `apiBlockIndex` or stream `index`; retain the native component.
  When absent, derive an ordinal only from that message's observed block
  sequence and label it derived. Interleaved message IDs maintain independent
  state. Repeated row UUIDs do not advance ordinals. If split versus replacement
  cannot be established, preserve the observation and report ambiguity.
  Multi-block user messages similarly preserve ordered blocks. Final references
  may name several eligible text blocks from one completed API message.
- **Grok:** adjacent same-kind message/thought chunks within one scoped prompt
  form a segment. Semantic content of another kind ends that segment; unrelated
  child traffic and non-content control facts do not. Missing prompt identity
  can be bound later only with explicit evidence. Tool revisions share scoped
  call identity and retain each event. Terminal output is a separate result
  facet linked to the call, not another invocation.

Invocation and result are separate facets on all providers. A richer snapshot
updates the applicable fields and records snapshot coverage; it does not add
the complete snapshot text to prior deltas. An out-of-order delta without a
known baseline remains unresolved evidence. The parser never guesses a prefix.

## 4. Typed content and origin

Items contain ordered content parts. Supported types include text, reasoning,
opaque reasoning, image/media reference, tool arguments/result, diff, plan,
interaction and unknown. Specialized payloads hold command arguments, exit
code, file changes, plan entries, question schemas/options and structured
answers. Unpromoted native extensions are bounded and retain their type.

Each part's fact scope is its owning item's scope; independently scoped facts
are separate observations rather than hidden in an unscoped payload. Parts
carry availability (`inline`, `reference`, `omitted`, `redacted`, `truncated`),
original byte length when known, omission reason and source reference. Output
truncation does not imply execution failure. Encrypted reasoning stays opaque.
Credentials from control/auth responses never enter generic extensions.

Semantic role, native envelope role and actor evidence are independent.
Origins include human, programmatic, injected context, task notification,
parent coordination, worker-local and unknown; attribution carries evidence.
Unknown is an allowed result. A `user` envelope alone does not prove a human
actor. Human-origin file evidence remains human even when an `origin` object
exists. Hidden control, compaction summaries and synthetic continuations do
not silently become human prompt boundaries.

## 5. Time: three distinct sources

`TimeObservation` has scope, value, raw representation, source class, source
field, precision, meaning and supporting observation. Meaning is event,
write, start, end, terminal, activity, conversation or thread_activity. Value is nullable; a
valid instant is serialized UTC RFC3339Nano, without inventing precision.

| Source class | Meaning | Examples |
| --- | --- | --- |
| native | Provider-authored timestamp | event occurrence, file write stamp, command start/end, turn start/end |
| capture | Time recorded by the capture transport | shim spool `ts`, recorded wire capture timestamp |
| observed | Time supplied by this parser's caller | scanner observation, current wire receipt, replay receipt |

Multiple native times coexist, including Grok file seconds and agent event
milliseconds. Capture and current observed times never fill missing native
time. Replaying a record retains original time and adds the new observation
time separately. A missing timestamp is not zero. Valid zero/negative epoch
values are governed by the field's format contract, not generic positivity.

### History conversion (consumer, outside the module)

History's existing turn/usage display starts at the first scoped native activity
in source order, and ends at the selected completion fact's native clock.
This conversion does not fill absent start/end scalars in the source model or
rename a native write clock as a provider-reported start. A reported duration
keeps its own unit and evidence rather than being recomputed from those clocks.

Activity meanings preserve the catalog's existing native time rules without
provider decoding in history. `activity` marks source bookkeeping;
`conversation` marks a native conversation row. Both can establish the start
of a source's activity, but bookkeeping does not advance the last conversation
time. `thread_activity` is a session document's reported activity clock; it
does not assert that a particular message exists. A reader can retain both a
write clock and its activity meaning. Neither implies turn completion.

CATALOG-TIME-1 names these added time facts. The full `ThreadProjection` oracle
in `TestParsedJSONLMetadataMatchesExistingCatalogFields` compares every prefix
of the new catalog fixtures and existing catalog/Guardian/plugin fixtures.
The Grok counterpart compares summary clocks, including offset and whitespace
handling. Module values remain UTC; history can retain the old API's offset
representation only when the raw scalar proves the same instant.

Durations are separate measurements with native field, unit, scope and basis.
API-call duration, tool duration and whole-turn duration are not interchangeable.
Elapsed duration neither creates a terminal nor overrides adverse evidence.

## 6. Completeness, outcome and finality

These are three independent dimensions, each with evidence references:

- Record/capture completeness: complete, partial, truncated or unknown.
- Execution outcome: running, completed, failed, interrupted, truncated,
  declined, cancelled or unknown, applicable to a turn/tool/interaction.
- Finality: unknown, candidate, final or retracted, with native/derived basis
  and supporting observations. There is no confidence score or label.
  Native phase remains untouched.

Content generation is an open/complete flag on the item, with part availability
for incomplete or withheld content. It is not a fourth completeness axis.

Completion is an evidence set, not a destructive last-value assignment. A
scoped native terminal can settle its exact turn. An unrelated or late terminal
cannot settle the current turn. A block stop completes generated content, not
a tool execution. EOF and connection loss supply no successful terminal.
An interrupt request/ack is intent/control evidence until native outcome exists.

Claude result interruption takes precedence over generic error; explicit error
is failure. API message end_turn/stop_sequence offers provisional evidence,
which later tool activity can revise. max_tokens truncation and API-error stubs
cannot be erased by a duration row. Retraction excludes prior blocks from final
candidates without deleting them.

Codex native final phase can designate an answer before a terminal; it does
not imply successful turn completion. Grok successful prompt completion may
support a derived last-eligible-assistant designation. It never creates a
native final phase on every chunk. Missing/unknown terminal reasons remain
unknown unless a provider-specific documented rule establishes their meaning.

## 7. Usage at its native grain

`UsageObservation` contains scope, grain, sample identity, revision identity,
counter semantics, counters, model buckets, incomplete flag, reset
epoch when evidenced and observation refs. Grains include response/API message,
turn/prompt and thread/session. Account billing and quota data are separate
connection-scoped metric facts; percentages are not token usage.

Missing counters are null, distinct from zero. Input/output/cache-read/
cache-write/reasoning/total fields retain native meanings; no generic total is
invented. Model and effort are time-scoped observations, not a latest snapshot
stamped onto historical turns. Conflicting usage remains inspectable.

Each consumer-facing turn usage summary names an aggregation basis and its
constituent observations. `Turn.UsageCoverage` counts reporting samples for
each selected counter. Prefer an authoritative exact-turn snapshot when
available; otherwise use distinct response contributions or an explicitly
evidenced cumulative delta. Never sum response, turn and thread grains together.
Never add live and file observations as independent contributions.

- Claude: revise a contribution by API-message identity; aggregate distinct
  messages with per-counter coverage. Keep result aggregates separately.
- Codex: response/root-turn/thread snapshots remain distinct. Exact repeated
  snapshots cannot inflate totals. A counter reset starts an evidenced epoch;
  unexplained decreases yield diagnostics instead of negative usage.
- Grok: accepted prompt usage is a scoped observation; preserve `modelUsage`
  and `usageIsIncomplete`. Replay, duplicate terminals and child totals do not
  add another parent contribution.

## 8. Typed relationships

`Relation` has kind, item/observation endpoints, scope and supporting
observations. Unresolved native call identity remains in the item's callId;
an orphan result is retained and diagnosed, not dropped. Do not build a
general graph of aliases, threads, requests and resources.

| Kind | Meaning |
| --- | --- |
| result_of | Result facet belongs to a scoped invocation |
| progress_of | Revision/progress concerns the invocation/task |
| replay_of | Receipt repeats identified earlier evidence |
| serialization_twin | Codex response/event observations serialize the same item |

`serialization_twin` is required by the Codex `paired-first-turn` and
`edge-nonadjacent-twins-and-real-repeat` fixtures. It connects observations,
preserving both source coordinates. Matching uses an unmatched opposite
serialization within one turn; ambiguous candidates remain separate with
`ambiguous_serialization_twin`. Poll/process handles, snapshot coverage,
retractions, request resolution, parent/guardian/continuation identities,
author/recipient and compaction context references remain bounded native
detail. Promote a typed field only when a fixture requires it. No relation
kind exists without an executable fixture that needs it.

Relations do not by themselves approve an action, complete a turn or prove a
human author. Consumer submission bindings live outside the parser.

## 9. Reader and checkpoint contract

Catalog metadata is a typed value beneath an observation. Its field set is
native thread/parent identity, source kind, title, preview, provider version,
cwd, model, effort, priority/service tier, permission, historical branch and
origin. Each value is known, unknown or unsupported; an absent field makes no
new claim. Scope, source and native clocks belong to the containing observation.
Readers emit changed facts. Filename fallback, discovery paths, Nano identities
and database reduction remain with the consumer. This adds no Thread unit.
Readers retain the last field value and its native attribution only to detect
changes and apply native first-title/first-preview rules. A newer native
observation time is a changed attribution even when its text stays the same;
this preserves the existing catalog's `MetadataField.ObservedAt`. Repeated
facts with the same attribution emit no catalog fields. This cache is
continuation state and is never emitted as a thread snapshot.

### History conversion (consumer, outside the module)

History selects catalog observations for the discovered source using normalized
native scope. Embedded worker facts remain separate from a parent catalog.
Source discovery checks an agent filename against native agent identity; this
check stays outside the module. A child row without an agent ID keeps unknown
child identity and known parent identity, instead of claiming its parent's ID.
Already-proven native parent context from a separate sidecar is retained when
a summary or update stream does not repeat it. Operational journal actor IDs
do not become native conversation identities.
`TestParsedClaudeCatalogSelectsSourceWithoutNativeDecoding` covers root/worker
selection, mismatched agent filenames and conflicting root identities.

History's existing catalog projection supplies the exact differential oracle;
any difference requires a named fixture and written reason at cut-over.

### Reader entry points

Provider packages expose `ParseFile(state, input)` and `ParseWire(state, frame)`
with explicit source context and limits, returning `(State, model.Batch, error)`.
`Input.Format` is empty or `jsonl` for records, and `json` for supported whole
documents. The caller supplies source label, incarnation, optional connection,
known scope, size limit and optional capture/observation clocks. Input
state is caller-owned and unchanged. State is deterministic, serializable and
versioned; continuation with incompatible versions/source incarnation fails
explicitly. Shared private helpers frame bytes and assemble within-source facts.
All production imports remain within this tree and Go's standard library.

File ingestion accepts chunks and returns a complete-line checkpoint. An
unterminated tail stays pending, including valid JSON without a newline, until
the caller explicitly declares a closed source. A closed-source parse may
consume a valid final record; malformed tails stay diagnosed as incomplete
capture. Neither condition implies execution completion. Oversized lines are
skipped through their delimiter with bounded retention and exact byte counts;
the next valid line still parses. JSON documents/sidecars use explicit source
formats instead of forcing every file through JSONL.

Every record gets a known semantic, known non-content, unknown or malformed
disposition. Unknowns retain an inspectable bounded source handle and stable
diagnostic. Diagnostics have code, severity, scope, coordinates and bounded
detail; aggregate counts remain complete even if detailed output is capped.
No full private record is printed in a diagnostic summary.

Version stamps separately name schema, provider reader, assembly, taxonomy,
usage contract and checkpoint compatibility. History receives metadata, items,
turns, usage, diagnostics and checkpoint in one parse result, then commits them
together. Reader versions trigger rebuilds even without file changes; a failed
replacement preserves the active prior projection. These storage actions are
outside the module.

An explicitly closed source retains only its close marker, position and version
information in the checkpoint. Its emitted rows remain with the consumer.
Closing a file does not supply completion evidence for an unfinished turn.

Growing sources retain continuation work, not completed output. The Claude
reader retains the latest turn in each scope for trailing results, usage and
duration. A new prompt retires earlier settled turns and their item, message,
call and replay-receipt state. Unfinished turns and pending controls remain.
Open continuation above 4 MiB produces `continuation_limit_exceeded`; the
reader keeps the unfinished evidence. Every reader measures that size by
encoding the whole continuation, so it measures on a cadence set by the input:
once the source has added an eighth of the last measured size, and never more
often than every 64 KiB. Measuring per frame made each event cost as much as
the retained state, and one 2.5-hour open turn stalled the session loop. The exact private source that previously
produced a 253 MB checkpoint has an opt-in bound and semantic-append test in
`corpus_checkpoint_test.go`. See the comparison guide.

A repeated UUID inside retained continuation produces replay evidence. An
unmarked UUID seen after eviction produces a newly observed item with its
native aliases and a new parser-local key. The parser receives no membership
set of past identities. History resolves the item through its persisted,
scoped identity index: an exact match gains the observation, a replay relation
and the new local-key alias, while retaining its existing row and text.
Eviction must not change that resolved history view. This identity reduction
belongs to the cut-over, and is tested there for both file and wire readers.

Codex and Grok also retire earlier settled turns, keeping current and unfinished
turns plus pending request context. Grok's explicit `isReplay` with a native
event outside retained state emits a standalone observation and a
`native.eventId` endpoint. The consumer indexes this namespace from normalized
`Observation.NativeEventID`, including events belonging to a turn. A replay-only
capture therefore supplies evidence even when its original target is absent;
resolution must retain that evidence and diagnose the missing target.

A late replay can identify an evicted target with
`Relation.ToNativeObservation`: native scope, alias namespace and exact scalar
JSON. Parser-local item and turn keys are absent from that reference. History
resolves it against persisted observation aliases. One target adds the resolved
observation ID to the relation. An absent or ambiguous target produces a
diagnostic and leaves the incoming observation standalone. Resolution does not
recreate an item or turn in continuation state. Unknown native scope never means
“choose the latest target”; number `1` and string `"1"` are different aliases.

## 10. Examples and limits

An ACP assistant stream with E1:`ha`, E2:`ha`, replay E1 produces text `haha`.
It retains three observations and a replay_of relation. The second real `ha`
is not dropped. A later terminal for another prompt remains separate evidence.

A Claude tool block stop yields complete generated arguments and unknown
execution outcome. An error tool_result yields a separate complete result,
failed outcome and result_of relation. Neither implies that the parent turn
failed; a later successful answer can still complete the conversation.

A Codex turn reports response usage 10, exact-turn usage 10 and cumulative
thread usage 100. All three remain inspectable; the turn summary is 10 with
the exact-turn basis, never 120. Missing wire native time stays null even when
capture and caller receipt timestamps exist.

The 393 research cases are shape evidence and proposed assertions. Some omit
unrelated outputs; some are recipes. Executable expectations must identify
their assertion scope and named departures from the research draft. Cross-cell
convergence means matching proven semantic facts, not erasing differences in
native identity, time or capture coverage. The first gate requires all fixtures
passing with every difference named. The cut-over gate compares every Grok and
Claude file, a deterministic 20 percent Codex sample and every fixture, with
every difference named. A full-corpus pass may follow as an audit.

The [conformance index](conformance.md) lists the executable assertion layers,
named differences and conservative behavior where native proof is absent.
