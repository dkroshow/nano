# Fixture conformance and named differences

The first gate covers the pure module, imported fixtures, dump command and
import boundary. History and live ingestion call the module. The old adapters have been
removed; immutable comparison receipts and their historical capture commit
remain available in the differential recipe.
The second gate adds their conversion and the bounded corpus differential:
every Claude and Grok file, a deterministic 20 percent Codex sample, and every
fixture. No corpus parity or production rebuild cost is claimed here.

## What the fixture suite checks

The imported research contains **393 cases and 1,195 files**. Original native
inputs, manifests and draft expectations retain their inventoried hashes.
Additional constructed regressions live beside the research, or as explicit
native rows in focused Go tests. They do not claim to be new live captures.

| Provider | Imported cases | Single-input item views | Separate scenario cases |
| --- | ---: | ---: | ---: |
| Codex | 215 | 213 | 2 |
| Claude | 122 | 119 | 3 |
| Grok | 56 | 52 | 4 |
| Total | 393 | 384 | 9 |

The suite has three assertion layers:

1. [Inventory and framing](../../internal/transcriptparse/fixtures_test.go)
   verifies original hashes, required artifacts, malformed rows and byte framing.
   [Model consistency](../../internal/transcriptparse/consistency_test.go) checks
   784 reader/input combinations across all cases: source accounting, reference
   targets, scope, local positions, aliases, clocks, usage and serializability.
2. [Codex and Claude views](../../internal/transcriptparse/research_views_test.go)
   and [Grok views](../../internal/transcriptparse/grok/research_views_test.go)
   compare item type, role, text, native phase, outcome and native turn ID.
   They read the original draft and apply explicit reviewed differences from
   [Codex](../../internal/transcriptparse/codex/testdata/item-views.json),
   [Claude](../../internal/transcriptparse/claude/testdata/item-views.json) or the
   Grok test's named table. Expected values are not generated from reader output.
3. The focused tests linked below assert the detailed native facts, replay,
   partial captures, control authority and checkpoint behavior. A six-field
   item view alone does not prove those facts. Unsupported or ambiguous evidence
   has a conservative expected result and a diagnostic, not a guessed success.

The accepted model uses Item and Turn, separate observation clocks, three
completeness dimensions and source-local ordinals. The draft's obsolete shapes
and confidence numbers are not compared as if they were the implemented API.
Exact native content and identities remain available through observations and
aliases; opaque fields are retained without claiming every field is validated.

[Converted fixture receipts](../../internal/app/testdata/transcript-compatibility.json)
cover all 417 native inputs through the actual file/SQLite or live adapter path.
160 views match the old adapters; 257 differ under the named rules on this page.
[The reconciliation test](../../internal/app/transcript_fixture_reconciliation_test.go)
checks source hashes, reader selection, exact converted-view hashes, every
native fixture's membership and a documented name for each changed case.
These reviewed receipts supplement the independent semantic assertions above;
they establish fixture reconciliation, not the still-pending corpus gate.

`LIVE-CODEX-RESUME-1`: resume items now pass through `ParseWire` and the shared
live conversion. [Resume assertions](../../internal/app/codex_resume_parse_test.go) preserve
command display text, plain output, failed outcome and native identities. The
RPC client supplies only a result body, so this boundary adds a caller-local
correlation label; the reader treats it as an uncorrelated snapshot, with no
fabricated completion notification. Selected native fields are preserved. The
existing five-turn tail, live-over-snapshot merge, view completion and display
clock policy remain at the consumer boundary. Absent native item clocks and
completion evidence remain absent in the module. Items without a native ID are
not assigned a manufactured native ID by this route.

`LIVE-GROK-2`: operational prompt, response-candidate and terminal consumers
now read `ParseWire` items and observations. [Operational assertions](../../internal/app/grok_parse_evidence_test.go)
and the existing Grok prompt/terminal and load-handshake tests check native
prompt scope, nullable clocks, terminal usage, synthetic continuations and
child isolation. Bounded native terminal detail still supplies the operational
usage object and error code. Queue/acceptance/load policy remains in the caller.
An unknown marked replay stays observation evidence; it cannot replace a live
response candidate. The existing terminal policy then selects that candidate.

## Paired captures and recipes

| Case | Executable evidence and scope |
| --- | --- |
| Codex `paired-first-turn` | `TestCodexPairedFirstTurn` and `TestCodexCheckpointBetweenPairedRows` in [reader tests](../../internal/transcriptparse/codex/reader_test.go). Genuine file/wire capture; three content items and native terminal converge. Native clocks and context coverage remain distinct. |
| Claude `paired-split-file` / `paired-split-wire` | `TestPairedSplitSnapshotsDoNotDuplicateStream` in [reader tests](../../internal/transcriptparse/claude/reader_test.go). Constructed split/snapshot pair. |
| Claude `captured/paired-assistant-excerpt` | `TestGenuinePairedAssistantExcerpt` in [capture tests](../../internal/transcriptparse/claude/captured_pair_test.go). Genuine two-file-row/fourteen-shim-record excerpt of one API message; no full-conversation claim. |
| Grok `paired-repo-parent-child` | `TestGrokGenuineWrappedCaptureRetainsScopedEvidence` in [control tests](../../internal/transcriptparse/grok/control_test.go). Genuine wrapped capture keeps parent/child identities, direction and clocks apart. |
| Grok `paired-segments-tools-late-prompt` | `TestPairedLatePromptAndCallBinding` in [reader tests](../../internal/transcriptparse/grok/reader_test.go). Constructed late binding with preserved earlier observations. |
| Codex `edge-oversize-body-budget` | [Size recipe test](../../internal/transcriptparse/codex/size_recipe_test.go) materializes the specified 33,554,433-character native body in memory. It checks bounded skipped-row checkpoint size, exact byte advancement and parsing the next valid row after restoration. |
| Claude `rewrite-and-size-budget` | `TestClaudeResearchSourceReplacementAndBudget` in [research semantics](../../internal/transcriptparse/claude/research_semantics_test.go) executes the mutation recipe. Source replacement needs a new incarnation; native row UUID survives and local coordinates change. |
| Grok `oversized-record` | `TestFileReaderTailMalformedAndOversizeRecover` in [reader tests](../../internal/transcriptparse/grok/reader_test.go) checks oversize draining, malformed tails and recovery. |
| Grok `summary-rewrite-lineage-conflict` | `TestGrokSnapshotLineageConflictsStayExplicit` in [document tests](../../internal/transcriptparse/grok/document_test.go) preserves conflicting native parent facts. |

## Named Codex differences

The [Codex package notes](../../internal/transcriptparse/codex/README.md) explain
each rule. The exact changed research item views and their reasons are in
`codex/testdata/item-views.json`. The following index covers all named rules,
including deeper semantics that do not change those six item fields.

| Names | Focused evidence |
| --- | --- |
| CODEX-ACTION-1, CODEX-GENERATION-1, CODEX-TOOL-1 | [File action tests](../../internal/transcriptparse/codex/file_action_test.go), [reader call/result tests](../../internal/transcriptparse/codex/reader_test.go) |
| CODEX-ACTION-2, CODEX-TWIN-2 | [Ambiguous action and twin tests](../../internal/transcriptparse/codex/ambiguous_action_test.go) |
| CODEX-CALL-1 | [Ambiguous native call fixtures](../../internal/transcriptparse/ambiguous_calls_test.go) |
| CODEX-TURN-1 | [Derived prompt activation across saved checkpoints](../../internal/transcriptparse/codex/scope_test.go) |
| CODEX-TWIN-1, CODEX-SCOPE-1, CODEX-END-1, CODEX-TIME-1, CODEX-USAGE-1, CODEX-USAGE-2, CODEX-PART-1 | [Reader tests](../../internal/transcriptparse/codex/reader_test.go) |
| CODEX-SCOPE-2 | [Connection scope tests](../../internal/transcriptparse/codex/scope_test.go) |
| CODEX-REASONING-1, CODEX-PART-2, CODEX-TEXT-1 | [File reasoning](../../internal/transcriptparse/codex/reasoning_file_test.go), [indexed reasoning](../../internal/transcriptparse/codex/reasoning_order_test.go), [whitespace](../../internal/transcriptparse/codex/text_test.go) |
| CODEX-CONTENT-2, CODEX-COMPACTION-1 | [Native content and compaction tests](../../internal/transcriptparse/codex/file_content_test.go) |
| CODEX-COMPACTION-2, CODEX-NOTICE-1 | [Research views](../../internal/transcriptparse/research_views_test.go), with named cases in the Codex view table |
| CODEX-ITEM-1 | [Captured and schema item variants](../../internal/transcriptparse/codex/items_test.go) |
| CODEX-CONTROL-1, CODEX-CONTROL-2, CODEX-CONTROL-3 | [Requests](../../internal/transcriptparse/codex/control_test.go), [typed replies](../../internal/transcriptparse/codex/replies_test.go), [external resolution](../../internal/transcriptparse/codex/resolution_test.go) |
| CODEX-METADATA-1, CODEX-METADATA-2, CODEX-METADATA-3, CODEX-GUARDIAN-1 | [Sidecars](../../internal/transcriptparse/codex/metadata_test.go), [wire notifications](../../internal/transcriptparse/codex/notification_test.go), [file metadata and guardian](../../internal/transcriptparse/codex/file_metadata_test.go) |
| CODEX-PROGRESS-1, CODEX-PROGRESS-2 | [Addressed updates](../../internal/transcriptparse/codex/progress_test.go), [turn and review facets](../../internal/transcriptparse/codex/facets_test.go) |
| CODEX-PROCESS-1, CODEX-REALTIME-1 | [Process bytes and handles](../../internal/transcriptparse/codex/process_test.go), [realtime uncertainty](../../internal/transcriptparse/codex/realtime_test.go) |
| CODEX-SNAPSHOT-1, CODEX-USAGE-3 | [Snapshot coverage and overlap](../../internal/transcriptparse/codex/snapshot_test.go), [usage repetition and decreases](../../internal/transcriptparse/codex/usage_test.go) |
| CODEX-META-3, CLAUDE-META-3, CATALOG-TIME-1 | [Catalog prefix comparison](../../internal/history/transcript_metadata_test.go) checks the existing metadata fields and native clocks through the module. |

## Named Claude differences

The [Claude package notes](../../internal/transcriptparse/claude/README.md)
explain each rule. Exact research view changes are in
`claude/testdata/item-views.json`.

| Names | Focused evidence |
| --- | --- |
| CLAUDE-PAIR-1 | [Genuine paired excerpt](../../internal/transcriptparse/claude/captured_pair_test.go) |
| CLAUDE-SCOPE-1, CLAUDE-SCOPE-2, CLAUDE-SCOPE-3, CLAUDE-SCOPE-4 | [Native, worker and connection scope tests](../../internal/transcriptparse/claude/scope_test.go) |
| CLAUDE-BLOCK-1, CLAUDE-TOOL-1, CLAUDE-END-1, CLAUDE-TIME-1, CLAUDE-USAGE-1 | [File/wire reader tests](../../internal/transcriptparse/claude/reader_test.go) |
| CLAUDE-PART-1, CLAUDE-TOOL-2 | [Mixed and malformed parts](../../internal/transcriptparse/claude/parts_test.go), [commands and proposed changes](../../internal/transcriptparse/claude/tools_test.go) |
| CLAUDE-END-2, CLAUDE-ORIGIN-1 | [Interruption, error, refusal and terminal precedence](../../internal/transcriptparse/claude/completion_test.go) |
| CLAUDE-USAGE-2, CLAUDE-USAGE-3 | [Strict counters and response contributions](../../internal/transcriptparse/claude/usage_test.go) |
| CLAUDE-REPLAY-1 | [Native UUID replay and conflicts](../../internal/transcriptparse/claude/replay_test.go) |
| CLAUDE-CAPTURE-1, CLAUDE-OBSERVATION-1 | [Shim sequences, process exits and observation identity](../../internal/transcriptparse/claude/capture_test.go) |
| CLAUDE-CONTROL-1, CLAUDE-CONTROL-2 | [Control identity, replies and metadata](../../internal/transcriptparse/claude/control_test.go) |
| CLAUDE-CONTROL-3, CLAUDE-CONTROL-4 | [Native decisions and unverified pending controls](../../internal/transcriptparse/claude/control_decision_test.go) |
| CLAUDE-QUESTION-1, CLAUDE-QUESTION-2, CLAUDE-QUESTION-3 | [Composite, late and ambiguous questions](../../internal/transcriptparse/claude/question_test.go) |
| CLAUDE-CALL-1 | [Ambiguous native call fixtures](../../internal/transcriptparse/ambiguous_calls_test.go) |
| CLAUDE-METADATA-1, CLAUDE-JOURNAL-1, CLAUDE-COMPACTION-1 | [Attachments, metadata, journals and compaction](../../internal/transcriptparse/claude/metadata_test.go) |
| CLAUDE-ENVELOPE-1 | [Envelope aliases, stop reasons and message model](../../internal/transcriptparse/claude/envelope_test.go) |
| CLAUDE-OBSERVATION-2, CLAUDE-REWRITE-1, CLAUDE-UNKNOWN-1 | [Orphan evidence, rewrite and unverified results](../../internal/transcriptparse/claude/research_semantics_test.go), [unknown block views](../../internal/transcriptparse/research_views_test.go) |
| CLAUDE-CONTEXT-1 | [Research views](../../internal/transcriptparse/research_views_test.go) include surrounding native content omitted by focused draft views |

## Named Grok differences

The [Grok package notes](../../internal/transcriptparse/grok/README.md) explain
each rule. `grokResearchViewDifferences` names the exact changed research views.

| Names | Focused evidence |
| --- | --- |
| GROK-OBS-1, GROK-OBS-2, GROK-ID-1, GROK-TIME-1, GROK-SCOPE-1, GROK-SCOPE-2, GROK-TOOL-1, GROK-END-1, GROK-END-2, GROK-USAGE-1 | [Reader, lifecycle, replay and checkpoint tests](../../internal/transcriptparse/grok/reader_test.go) |
| GROK-BINDING-2 | [Independent ordered late-binding changes](../../internal/transcriptparse/grok/binding_test.go) |
| GROK-CONTROL-1, GROK-RPC-1 | [Capture direction, typed IDs and RPC namespaces](../../internal/transcriptparse/grok/control_test.go) |
| GROK-RPC-2 | [Request replay, conflicts, reuse and malformed IDs](../../internal/transcriptparse/grok/rpc_test.go) |
| GROK-QUEUE-1, GROK-LOAD-1 | [Queue authority](../../internal/transcriptparse/grok/queue_test.go), [load coverage and replay](../../internal/transcriptparse/grok/load_test.go) |
| GROK-CONTROL-2 | [Prompt reply terminal authority](../../internal/transcriptparse/grok/terminal_control_test.go) |
| GROK-INTERACTION-1, GROK-PROVENANCE-1, GROK-TIME-3 | [Native decisions, part paths and control clocks](../../internal/transcriptparse/grok/interaction_test.go), [wrapped terminal paths](../../internal/transcriptparse/grok/terminal_control_test.go) |
| GROK-CONTENT-1, GROK-TASK-1, GROK-TIME-2 | [Media, ordered plans, truncation and clock regression](../../internal/transcriptparse/grok/content_test.go) |
| GROK-META-1, GROK-COMPACTION-1 | [Summary, lineage, compaction and unverified schema](../../internal/transcriptparse/grok/document_test.go) |
| GROK-META-2, GROK-DIRECTION-1 | [Envelope metadata and client-sent updates](../../internal/transcriptparse/grok/update_metadata_test.go) |
| GROK-AUX-1 | [Auxiliary bodies, calls, telemetry and ambiguity](../../internal/transcriptparse/grok/auxiliary_test.go) |
| GROK-OBSERVATION-1, GROK-COORDINATION-1, GROK-SEGMENT-1 | [Exact research views](../../internal/transcriptparse/grok/research_views_test.go), supplemented by the control, metadata and reader tests above |
| GROK-REPLAY-2 | [Late replay assertions](../../internal/transcriptparse/grok/replay_test.go) keep post-eviction replay as a native observation reference; [live conversion](../../internal/app/live_parse_test.go) resolves a retained original and leaves a missing original as evidence. |

## Deliberate limits

History conversion is now `history-file-v11`, with observation storage by
source reference. Version numbers in the rules below identify when each rule
landed.

Missing native proof stays missing. Realtime deltas without part identity,
resume snapshots without ordering proof, unknown pending control schemas and
reused call IDs have explicit diagnostic fixtures above. A metadata or child
terminal cannot settle a parent conversation. Transport acknowledgement cannot
approve a tool. A row without a stable native/capture identity cannot establish
replay through equal text alone. Source replacement requires a new incarnation.

Native fields without a promoted semantic meaning remain bounded detail.
This includes unfamiliar tool/media extensions, sidecar schema versions and
provider counters whose reset epoch is not evidenced. The parser does not
invent their execution, media encoding, cross-source identity or usage totals.
Unknown or invalid promoted fields retain diagnostics; valid neighboring
evidence survives.

MODEL-EVIDENCE-1 names the additional native detail and provenance exposed by
the converted file view. The older parsers often retained a selected display
map; the module retains native payload detail, typed parts and observation
references. Emission indices follow the newly represented parts, including
opaque reasoning. The source hash still identifies the native record.
`TestParsedTranscriptApplyWritesViewsAndEvidenceAtomically` and
`TestTranscriptObservationMembershipKeepsSourceOrder` check the stored evidence
and source order. The provider part fixtures above check the native contents.

HISTORY-TURN-VIEW-1 names changes to projection positions when parts, replay
or source-only rows change. History assigns dense row positions and resolves a
turn's final reference to its retained item. Native turn IDs and API-message
anchors align the corpus comparisons before completion, context and usage are
compared. An absent configuration and JSON `null` both mean unknown; their
serialized hashes differ. Native timestamps and durations follow their typed
facts, as checked by `TestConvertedFilePreservesTurnUsageDisplayFields`.
The provider scope, completion and usage rules above explain changed groups;
an ordinal match alone is not evidence that two turns are the same.

READER-LIMIT-1 names explicit malformed/oversized record diagnostics and
oversized open continuation. A bounded record placeholder retains its source
reference; whole-document framing hashes the document instead of a physical
line. These hash differences remain separate from semantic message hashes.
`TestOversizedLineIsBoundedAndNextLineSurvives`,
`TestCodexAndGrokOversizedOpenContinuationIsDiagnosed` and the provider framing
fixtures check these limits. Open continuation is diagnosed without silently
truncating its state. Unknown provider shapes use the provider-specific
diagnostic names in the tables above.

The row limit bounds incomplete/oversized record retention. All three readers
evict earlier settled turns from growing-source continuation and diagnose open
state above 4 MiB. Claude's exact-source bound test, converted-store comparison
and separate rebuild-cost proof are described in the
comparison guide. The dump command retains final output.
First-gate fixtures do not replace these measurements.

## Build and import boundary

[The boundary test](../../internal/transcriptparse/boundary_test.go) parses all
Go files in the module and command, including test files and inactive platform
files. Dependencies must be standard library or this module. Production reader
imports also exclude filesystem, networking, random IDs and clock access.
Layer checks prevent sibling-provider dependencies and outward model imports;
the command cannot import the private assembly packages. Negative guard tests
exercise forbidden dependencies, effects, aliased clocks and dot imports.

Every package has `doc.go`. The standalone command can be built in a directory
containing only `internal/transcriptparse`, `cmd/transcript-dump` and a minimal
`go.mod` with the current module name and Go 1.21 declaration. No Nano database,
server, web bundle or third-party dependency is needed. [Command tests](../../cmd/transcript-dump/main_test.go)
cover framing, typed JSONL changes, raw detail, strict diagnostics, detection,
stdin limits, source clocks and companion lineage metadata.

## Reader recovery errors

READER-ERROR-1: all three file readers preserve `source_closed` and
`offset_overflow` from framing as scoped error diagnostics. Callers can distinguish
a closed capture from an exhausted source coordinate.
[TestFileReadersPreserveFramingErrorCodes](../../internal/transcriptparse/reader_errors_test.go)
exercises both recovery cases for every provider. Checkpoint formats are unchanged.

GROK-PROMPT-INDEX-1: two different native prompt indices prevent consecutive
user chunks from becoming one prompt. The earlier unknown turn stays separate,
and replay preserves its identity.
[TestNativePromptIndicesSeparateConsecutivePrompts](../../internal/transcriptparse/grok/prompt_index_test.go)
exercises both readers, equal-index chunks, late prompt binding and checkpointed replay.

CLAUDE-ITEM-ID-1: `NativeItemID` is API message ID plus block index, with
`native` authority when the provider supplied the block index, `derived`
(provisional) authority when the reader counted it, and separate component aliases.
[Composite identity](../../internal/transcriptparse/claude/identity_test.go)
and the [genuine paired capture](../../internal/transcriptparse/claude/captured_pair_test.go)
prove file/wire agreement, checkpoint restore and missing-ID behavior.

CLAUDE-EVENT-DIGEST-1: UUID observations carry the native event's canonical
content digest. History can therefore check persisted replay identity without
decoding a Claude envelope or keeping retired events in reader checkpoints.
[The digest regression](../../internal/transcriptparse/claude/event_digest_test.go)
covers both readers across eviction and restore: a changed timestamp preserves
the digest, changed content changes it, and item evidence retains it.

CLAUDE-REPLAY-2: a replay can change its prompt ID and secondary file snapshot
while retaining the UUID and message. The same regression file covers this
shape from frozen receipt `00245-0821da629feb`: one result facet, two message
observations, changing envelope evidence retained, and the new prompt alias.
Message usage and structured answers remain part of the conflict check.

CLOSED-CHECKPOINT-1: explicitly closed file sources and whole Grok documents
retain only source position and version information in their checkpoints.
[Closed-source tests](../../internal/transcriptparse/closed_checkpoint_test.go)
require checkpoints under 4 KiB, preserve emitted content, and reject later
bytes without inventing provider completion. This does not establish the
separate bound for growing files with open assembly.

CODEX-TIER-1: explicit `thread_settings_applied` service tier takes precedence
on its turn over a later `turn_context` fallback. An unbound settings fact
waits for the next context and survives checkpoint restoration; it does not
leak into the following turn. A later explicit settings event can revise its
open turn. The [constructed fixture](../../internal/transcriptparse/codex/testdata/effective-service-tier.jsonl)
and [reader assertions](../../internal/transcriptparse/codex/settings_test.go)
cover all three cases. Codex reader-v30/checkpoint20.

CODEX-DURATION-1: a task terminal's `duration_ms` or `durationMs` retains a
native millisecond duration and its observation reference. It is not calculated
from file-write timestamps. Zero remains an explicit duration; null, negative,
fractional and string values remain unknown with `invalid_duration`.
[Duration assertions](../../internal/transcriptparse/codex/duration_test.go)
cover both field names. Codex reader-v31/checkpoint21 restores this field from
the earlier history parser.

HISTORY-DISPLAY-TIME-1: history's turn/usage display uses the first scoped
native activity in source order for its start. Its end uses the selected
completion fact's native clock. Native completion outranks a later derived
stop; a duration record alone cannot move the end. Capture/receipt clocks do
not fill an absent native clock. The source model keeps each clock's original
meaning and leaves its absent start/end scalars unchanged.
[Actual conversion fixtures](../../internal/history/transcript_converted_usage_test.go)
check Claude/Codex start, end, native duration, model, effort and counters;
[clock assertions](../../internal/history/transcript_display_time_test.go)
cover regressing clocks, child scope, reopening and an untimed native terminal.
History conversion v6 rebuilds projections with these display fields.

HISTORY-CONTEXT-2: a late execution result can recreate an evicted native turn
without its earlier context. Conversion keeps that same turn's stored config,
model, effort and service tier when the new snapshot omits them. Another
turn's context never supplies these fields. The constructed
`TestConvertedCodexLateExecutionKeepsTurnContext` in
[conversion assertions](../../internal/history/transcript_converted_usage_test.go)
reproduces the loss found in corpus receipts `05335-025cfc02348e` and
`05336-2c03cedb1911`. It checks the interrupted first turn and the distinct
second turn through SQLite and the transcript/usage columns. History
conversion v7 restores the earlier parser's retained context.

HISTORY-CALL-FACET-1: call identity also requires an equal normalized tool
name. A named invocation and an unnamed execution observation remain distinct
after reader eviction, even when both are command facets with the same call ID.
Exact item/event identities still resolve snapshots independently of this weaker
fallback. The same conversion fixture above now requires both the unknown
generation and the completed execution. The [live identity assertions](../../internal/history/transcript_identity_index_test.go)
require each compatible late update to find its own retained facet.
History conversion v8 fixes the merged invocation observed at line 425 of
receipt `05336-2c03cedb1911` after the execution at line 554.

HISTORY-GROK-USAGE-CONTEXT-1: the existing SQL and API usage display keeps
Grok's current session model and effort on historical turns. History applies
the separately parsed catalog snapshot after the transcript batches, in the
same transaction. The stored model turn and its observations retain unknown
per-turn model and effort: the current summary cannot prove historical settings.
`TestGrokSessionModelEffortRoundTripsThroughView` now checks the actual module,
SQLite and API path, including that evidence boundary. This fix landed as Grok
conversion v9 while Codex and Claude were at v8; all three are now v11. It restores the display fields missing
from the first converted corpus run without changing token counters.

CODEX-SIDECHAIN-1: native `response_item/agent_message` retains the earlier
parser's sidechain flag and collaboration tag. Ordinary `event_msg/agent_message`
remains ordinary assistant output. This restores the classification used by
history filtering without changing the message's text or native scope.
The existing `capture-agent-message` and `file-response-item-agent-message`
fixtures plus the ordinary event fixture have explicit assertions in
[file content tests](../../internal/transcriptparse/codex/file_content_test.go).
Codex reader-v32/checkpoint22 fixes the omission found in the bounded corpus.

CODEX-SETTINGS-2: a pending explicit service tier applies when the next task
starts, including tasks without `turn_context`. It cannot remain pending until
a later task supplies context. `TestCodexSettingsApplyToNextTaskWithoutContext`
in [settings tests](../../internal/transcriptparse/codex/settings_test.go)
reproduces the missing and misplaced tiers found in corpus receipt
`05509-fb18aa9a398b`. The existing settings fixture still checks explicit-notice
precedence over context and JSON restoration. Codex reader-v33/checkpoint23.

LIVE-CODEX-2: execution response collection reads completed wire items and
resume snapshots through `ParseWire`, preserving native text and whitespace.
It retains the existing final-answer selection and native turn-terminal rule;
item completion alone cannot complete an operation. The wire and resume cases
in `TestCodexOperationalResponseUsesNativeTextInWireAndResume` exercise the
stored response, while `TestAgentExecutionObserverUsesCodexTurnTerminalEvidence`
checks the terminal boundary. This removes the remaining production uses of
the old Codex text helper.

HISTORY-EVIDENCE-1: file ingestion retains the existing Dev view's source-only
record links as history rows, separate from model Items. Record positions also
keep a terminal event out of an older page snapshot. The constructed fixtures
in [the catalog service tests](../../internal/threadindex/thread_catalog_service_test.go)
(`TestTranscriptDevEvidencePreservesExactNativeTextAndRejectsReplacement` and
`TestThreadTranscriptFixedSnapshotBodyAndETagStableAcrossAppend`) exercise
native whitespace, invalid UTF-8, exact numeric text, replacement rejection,
and a stable page body and ETag after completion. No provider envelope is
decoded to create these compatibility rows.
The converted file view emits a fallback link only for a source record with
no represented item or item observation. Stop annotations, replay placeholders
and metadata captions from the older parser therefore no longer add another
row beside represented evidence. Standalone metadata keeps the generic record
caption; its fallback row has no invented native clock or turn binding. Native
clocks, terminal outcomes and metadata stay in their typed observations/turns.

HISTORY-IDENTITY-2: conversion preserves independent model items that share a
call ID. An interaction's call ID addresses a tool and does not identify the
request. Distinct local items within one retained source turn also remain
distinct; native receipts can still resolve provisional items after eviction
changes that source turn key. The existing Claude
`control-replay-answered-and-pending` and Codex `ambiguous-call-id` fixtures
now run through the actual SQLite conversion in [live_parse_test.go](../../internal/app/live_parse_test.go);
the Claude fixture also runs through the live adapter. Each retains both
requests/invocations and their independent outcomes. History conversion v5
rebuilds earlier converted projections; the module types and readers are unchanged.

LIVE-CODEX-1: the live Codex adapter now calls `ParseWire`. Its existing browser
DTO still uses receipt time as a display fallback and labels finished cards
`completed`; the model keeps unknown native time and failed outcome separately.
Native card detail retains execution status and output. `CODEX-TOOL-1` now also
applies to this route: plain command output is not opportunistically base64
decoded. The constructed [live assertions](../../internal/app/live_parse_test.go) cover
literal `TWFu`, failed completion, null native time, and resumed text prefixes.
Resume prefixes remain at the view boundary and do not become parser evidence.
Provider-supplied item IDs are available during deltas; a generated view ID
for an unidentified item no longer appears as a native ID.
The older live reasoning fixture now supplies the required `summaryIndex: 0`;
missing indices remain rejected as documented by `CODEX-PART-2`.
The live route now exposes the native tool requests/replies, addressed progress,
warning notices, process exits, compaction, turn plan/diff and realtime notices
already asserted by CODEX-CONTROL-1/2, CODEX-PROGRESS-1/2, CODEX-METADATA-2,
CODEX-PROCESS-1 and CODEX-REALTIME-1. Their 21 wire fixture probes previously
produced no live items. Each new item retains the module's scope and evidence.

CLAUDE-BLOCK-2: a tool-only snapshot following streamed text uses its unique
call ID within that API message to find the streamed block. Counting snapshots
alone could overwrite the text block. Ambiguous call IDs or conflicting block
types stay observations with `snapshot_block_identity_conflict`. The constructed
[stream-tool-snapshot fixture](../../internal/transcriptparse/claude/testdata/stream-tool-snapshot.jsonl)
and [assertions](../../internal/transcriptparse/claude/stream_snapshot_test.go)
cover both outcomes. Claude reader-v30/checkpoint20.

LIVE-CLAUDE-1: the live Claude adapter calls `ParseWire`. Stream and snapshot
tool cards retain the API message/block composite identity; call ID stays
separate. A known `result_of` supplies the existing invocation card's output
and error flag while the model retains distinct invocation/result facets.
Full assistant text snapshots are now visible, including text omitted by the
old stream-only adapter. Nested child traffic stays outside the parent cards.
Initial block text and every native user block survive. An orphan tool result
remains visible with its unresolved call ID. Generated user-view IDs no longer
appear in the native-ID field.
[Live assertions](../../internal/app/live_parse_test.go) exercise these changes. The existing
live fixture now supplies native delta `type` fields. User event UUID fallback
and receipt-time display remain compatibility view choices.

LIVE-REPLAY-1: the retained live view uses history's existing identity resolver
with native receipts outside parser continuation. An evicted Claude result
rejoins its first invocation card; conflicting UUID content leaves that card
unchanged. Replay aliases cannot claim a later turn's call, and an earlier
replay does not freeze a subsequent snapshot revision. The constructed tests
in [live_parse_test.go](../../internal/app/live_parse_test.go) and
[the receipt index tests](../../internal/history/transcript_identity_index_test.go)
cover these cases. Receipts leave memory when their view rows leave the tail.
The opt-in frozen-source [live projection test](../../internal/app/live_parse_frozen_test.go)
runs through `applyWireLive`, including a later Nano turn. With and without
reader eviction, an exactly identified replay keeps its card, turn, presentation
owner, and position/content hash, with two observation references and `replay_of`.
First occurrences retain today's current-turn assignment. Stage 5 removes the
temporary conversion flag with `live_projection.go` and applies this identity
and slot rule in its owner. Done-to-completed compatibility stays in place;
browser validation is required after the PM deploys the second gate.

LIVE-GROK-1: the live Grok adapter calls `ParseWire` and resolves retained native
receipts outside parser continuation. The existing
[late-replay fixture](../../internal/transcriptparse/grok/testdata/late-replay-after-eviction.jsonl)
now also runs through the live adapter: two distinct `A` chunks stay `AA`, and
the evicted chunk's replay adds neither text nor a card. A marked replay with
no retained original stays evidence only. The timestamp test first records the
original, then verifies replay keeps its native time and text. The older chunk
test now uses distinct event IDs for distinct chunks. Model item IDs replace
the old prompt/kind-derived view IDs; user event IDs remain a browser echo
compatibility field. Known tool results still fill their invocation cards, and
terminal prompts settle their cards. An ordinary agent chunk no longer gets
`final_answer` phase automatically; the converted view requires the module's
finality evidence. Content completion, outcome and finality
remain separate in the model. [Live assertions](../../internal/app/live_parse_test.go) and
[Grok integration tests](../../internal/app/grok_test.go) cover these changes.
Unknown updates and malformed records remain evidence/diagnostics instead of
invented conversation cards. Discovery also reads child sidecars through
`ParseFile`; existing lineage and conflict tests retain the catalog contract.

HISTORY-IDENTITY-2: distinct native block IDs remain independent when their
call ID is reused. The weaker call key cannot merge them or reject their
whole source. The existing [ambiguous call fixture](../../internal/transcriptparse/claude/testdata/ambiguous-call-id/input.jsonl)
now passes through both file storage and the live adapter in
`TestClaudeDistinctNativeBlocksSurviveSharedCallInFileAndWire`. Both blocks
remain visible; the result has no arbitrarily chosen invocation. Conflicting
claims about one exact native block still fail closed. Exact component aliases
also distinguish different API messages when the block indices were counted.
A matching exact message/call pair outlives a provisional source turn; native
turn and parent scopes must still agree. Several compatible call candidates
fall through to exact UUID or source-local identity with an
`ambiguous_call_identity` diagnostic, preserving the standalone item. Parser
diagnostics remain advisory and never decide identity. This rule landed at
history conversion v4 with no schema change.

LIVE-TIME-1: while native creation time is unknown, later deltas and snapshots
keep the live card's first receipt time, including resume seeds. The module
continues to record unknown native time and separate observed clocks.
`TestLiveModulePreservesFirstReceiptTimeAcrossUpdates` in
[live_parse_test.go](../../internal/app/live_parse_test.go) exercises all three providers.
A later native timestamp takes precedence over that display fallback. This
explains the remaining creation-time difference in Claude's genuine paired
wire excerpt: its later assistant snapshots supply native timestamps, while
the old view keeps the earlier stream receipt times.


CLAUDE-ITEM-ID-2: counted block composites stay provisional after eviction.
The six-row `replay-block-index-reset.jsonl` fixture has two calls in one API
message, then a replay of the second block after a completed turn is evicted.
Both readers retain the emitted composite and its `derived` alias basis.
History admits only exact composites at the strong identity step; a provisional
composite cannot reject a source or replace its previously stored block ID.
`provisional_block_index` identifies counting without a retained message after
a prior turn; it says earlier blocks *may* have been evicted, without retaining
an unbounded set of old message IDs. Identity tests cover call/UUID resolution,
and frozen source `00093-5cdfcac18395` checks one converted row per command.

FILE-OUTCOME-1: the converted file view's status follows execution outcome.
A complete prompt, text block, proposed command, or opaque returned payload does
not prove successful execution. Those rows keep `unknown` outcome while their
content completion remains separate in parsed evidence. Native result failures
still produce `failed`. This accounts for the status-only fixture differences;
`edge-command-arguments-and-object-result` additionally preserves its explicit
exit code 2 as failure (CODEX-TOOL-1), and `file-event-msg-error` remains a notice
without assigning that notice a turn execution outcome.

HISTORY-DOCUMENT-1: the 18 Grok whole-JSON fixture probes now use document
framing. The old transcript adapter's `input.json` probes went through its
JSONL fallback and produced one malformed-record row per physical line. Its
production `summary.json` path instead produced an empty transcript. The module
reads each JSON object once, retains typed metadata or compaction evidence, and
the converted probe shows one source-evidence or compaction row. Auxiliary probes
use the harness-only consumer identity documented in the comparison guide.
Their view counts do not describe ordinary catalog routing. GROK-META-1 and
GROK-COMPACTION-1 provide the semantic assertions; indexed catalog values are
compared separately in full.

HISTORY-CONTEXT-1: native injected context, interruption markers and compact
summary text remain in the converted item instead of the old fixed captions
such as “Injected provider context” or “Turn interrupted”. Non-text compaction
boundaries and image context have no invented text body; their metadata and
parts remain in parsed evidence. CLAUDE-CONTEXT-1, CLAUDE-END-2 and CLAUDE-PART-1
supply the corresponding semantic assertions. Terminal source-record captions
remain covered by HISTORY-EVIDENCE-1, with turn completion retained separately.

HISTORY-TOOL-TEXT-1: a generic tool invocation retains its native argument
object instead of reducing it to one selected field. In
`task-launch-and-notification`, the Agent call keeps both `prompt` and
`run_in_background`; its result and later notification remain separate evidence.
Specialized command views still expose command text. Content completion and
execution outcome remain separate (FILE-OUTCOME-1).

Historical wire comparison receipts use `source-clock-presentation`,
`normalized-facet-ownership`, `auxiliary-order-normalization`, and
`native-turn-grouping` to name their distinct clock, facet, ordering, and
turn-group semantics. These receipts describe parser compatibility; browser
transcripts come from saved files.

## Storage tests after parser removal

The history tests build their input views through `ProjectParsedFile`, the
normalized spool and SQLite. They no longer call a duplicate native parser.
Storage tests still check append paging, pinned views, completed-turn bounds,
body retention, search and usage joins. Constructed usage examples supply API
response IDs and response counters where the test needs a turn total;
unidentified samples and thread totals are tested separately by the module.

`CODEX-TURN-1`: a genuine prompt without a native turn ID activates its new
source-local turn. Later answers and terminal evidence remain in that turn,
including after checkpoint restore. No native turn ID is manufactured.

The migrated expectations use the existing named native semantics: injected
context remains a notice; tool answers remain tool results; capture completion
is separate from turn outcome; source metadata remains inspectable; a new
prompt does not invent completion of an unfinished Claude turn. Assertions
for those cases are in the history transcript and usage tests as well as the
module fixtures linked above.

`CLAUDE-SCOPE-4`: `isSidechain:true` excludes the parent but does not identify
a child. Without a native child ID or parent-call scope, the reader retains
the row as a standalone native observation with `unbound_sidechain_scope`.
It changes no parent item, turn, completion, finality or usage. Nullable
native identity stays unknown. The file/wire scope test checks both parent
isolation and retention of the unbound evidence after checkpoint restore.
The source-hashed audit of all 5,329 frozen Claude files (549,390 rows) found
no row that triggers this guard; a real occurrence returns to the overseer
as a question.
