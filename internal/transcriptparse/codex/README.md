# Codex reader extraction

`content.go` extracts command, content, settings and origin helpers from the
current history parser at `115207e`. File dispatch follows its response/event
switches; wire dispatch follows the current live item adapter. `ParseFile`
and `ParseWire` share item, turn, usage and evidence assembly. The original
Nano parsers remain in service.

The genuine `paired-first-turn` capture now yields the same three content
items and native turn terminal from both inputs. Context observations remain
separate. Its response/event pairs and
`edge-nonadjacent-twins-and-real-repeat` require `serialization_twin`, the
conditional fourth relation allowed by the overseer's model verdict. These
are two native serializations, not a replay of an identical event. Matching
requires one unmatched opposite serialization with equal text in the same
turn. Ambiguous candidates remain separate with an explicit diagnostic.

Named implemented differences:

| Name | Behavior and executable evidence |
| --- | --- |
| CODEX-METADATA-3 | Five native world-state, coordination-metadata and thread-setting/name/goal fixtures retain complete observations without invented content or turns. |
| CODEX-GUARDIAN-1 | The file guardian assessment retains an interaction, native review alias, addressed action and running review status; it does not borrow the target item ID. Native coordination text is retained when present. |
| CODEX-PART-2 | `edge-wire-reasoning-indexed-parts` displays summary indices 0/1 before raw-content index 0 while preserving source arrival order on observations. Missing, null, negative, fractional, string and overflowing indices remain diagnosed observations without invented part zero or a turn. |
| CODEX-ACTION-1 | Five imported command/patch lifecycle cases retain native arguments, changes, outputs and execution outcomes. Scoped call IDs connect begin/end observations on one execution item and progress_of links a known generated invocation. A response output still links the generated invocation. Parent completion remains independent. |
| CODEX-GENERATION-1 | Completed function/custom/local-shell response-call status completes argument generation only. A known command result can report failure through its native object exit code. Nested local-shell argv remains readable; request_user_input retains an interaction invocation and separate result. |
| CODEX-CONTENT-2 | `TestCodexFileOriginKeepsNativeContent` retains synthetic notification/abort text, keeps image-only native user prompts and diagnoses unknown media without dropping its payload. Origin classification no longer erases content. |
| CODEX-COMPACTION-1 | `TestCodexFileCompactionAndOpaqueImageVariants` retains native compaction summaries and replacement history as detail without replaying embedded conversation rows. Schema compaction variants and opaque image-generation results keep typed items without invented text, encoding or execution. |
| CODEX-REASONING-1 | Both rollout reasoning fixtures retain readable summaries. File and wire assembly concatenate ordered summary/raw parts without trimming native text or exposing ciphertext. `TestCodexFileReasoningSummaryFixtures` and the mixed-parts regression cover the loss. |
| CODEX-PROCESS-1 | Connection-scoped process streams assemble declared base64 bytes across UTF-8 boundaries, keep stdout/stderr separate and preserve prior streams when exit buffers are empty. Exit status and output truncation are independent. Malformed base64/buffers and binary terminal output retain explicit evidence. ProcessHandle and command/exec processId are separate native aliases, never item IDs. Invocation correlation remains unresolved and diagnosed. |
| CODEX-REALTIME-1 | All eight imported realtime notifications retain native thread scope without caller-turn inheritance. Flat transcript/audio deltas remain diagnosed observations because part identity or baseline evidence is absent. A done body retains full text independently, with no guessed delta join or turn terminal. Unknown roles and arbitrary itemAdded payloads retain explicit diagnostics; audio gains no inferred encoding. |
| CODEX-PROGRESS-2 | Turn plan/diff snapshots update one separate facet per native turn and preserve their observations without fabricated item IDs. Auto-approval review updates use codex.reviewId, retain native millisecond clocks and target detail, and never change the reviewed command's execution outcome. Imported completed reviews with inProgress status keep that running outcome. |
| CODEX-METADATA-2 | Thirty imported wire metadata notifications retain their native payload at its explicit connection/thread/turn scope. Account, goal and hook terminal words never complete conversation turns or become token samples. Seven error/warning/compaction fixtures retain visible bodies with unknown action outcome. Unknown notifications, including unknown thread/* names, retain the bounded envelope with unknown_method. Research transport-only system placeholders become observations. |
| CODEX-SNAPSHOT-1 | The two edge-wire-resume fixtures retain snapshot coverage, native turn seconds and duration, without fabricated item clocks. Nested turns/items have distinct source paths. An uncorrelated but structurally identified received thread snapshot is decoded with unresolved_snapshot_response. Existing content and status are not overwritten without ordering proof; differing item evidence produces snapshot_delta_overlap_unproven. Empty snapshots/errors create no turns, and loaded turns supply no active default. |
| CODEX-CONTROL-3 | `serverRequest/resolved` closes the referenced RPC lifetime without inventing a decision. A captured answer keeps its outcome; a later answer can supply the missing decision. Repeated resolutions retain replay evidence, ID reuse after resolution creates a new request, and an orphan resolution stays an observation with unresolved_rpc_resolution. |
| CODEX-CONTROL-2 | All eight imported edge-response pairs now link decisions to requests. Dynamic replies remain separate result facets; generation never proves execution. Typed IDs and client/server namespaces cannot cross; ambiguous or conflicting replies are diagnosed. Exact reply replay creates no result, while an ID reused after a response creates a new request. Authentication/attestation and generic client reply bodies exclude opaque fields. Null success remains unknown. |
| CODEX-CONTROL-1 | All ten imported request-* specimens have executable assertions. Approvals/questions become pending interactions; item/tool/call retains a generated invocation and native arguments. Authentication/attestation stay connection metadata with opaque material excluded. Typed RPC aliases preserve number versus string; exact pending repeats retain replay evidence and conflicting reuse is diagnosed. Explicit native turn identity may own a source-local turn; otherwise no turn is invented. Reply evidence is covered separately below. |
| CODEX-METADATA-1 | `sidecar-history-jsonl` and `sidecar-session-index-jsonl` retain auxiliary facts with explicit native thread and native second/string clocks. They create no items, turns or remembered session defaults. Unknown file rows retain their whole bounded envelope, including fields outside payload; no private text enters diagnostics. |
| CODEX-PROGRESS-1 | `TestCodexItemUpdatesPreserveTheirAddressedFacet` checks plan deltas, file output/patch revisions, MCP progress and terminal input through JSON checkpoints. Parts retain distinct meanings, progress_of names addressed updates, and completion stays unchanged. A confirming plan snapshot replaces stale delta parts. The five imported orphan-update fixtures retain incomplete items plus item_update_without_start diagnostics; this differs from the research draft's observation-only deltas or empty system items. |
| CODEX-ITEM-1 | `TestCodexCapturedCompletedItemVariants` covers all seven captured file item_completed variants: Pascal-case text content, summary_text/raw_content, command argv, map-shaped changes and web.search Extension results. `TestCodexWireItemTaxonomyRetainsNativeFacets` covers all 18 wire item variants. Tool names, image references, opaque image results and review text survive. Unknown completed items retain their payload with an unknown_item_type or unknown_extension diagnostic. Item completion does not complete the turn. |
| CODEX-SCOPE-2 | `TestCodexFileSessionMemoryStaysOnItsConnection` keeps file session defaults within a connection, honors explicit native metadata and caller scope, and does not erase memory for an empty metadata row. `TestCodexWireThreadMetadataDoesNotBindMultiplexedTraffic` scopes thread/started to its native thread and parent without claiming later unscoped frames. |
| CODEX-SCOPE-1 | `edge-late-terminal-native-turn` completes turn-a without completing turn-b. Native item keys include the native turn. Legacy real user prompts create distinct local turns. |
| CODEX-TWIN-1 | `paired-first-turn` and `edge-nonadjacent-twins-and-real-repeat` retain both observations on a logical item. A later native turn with identical text remains distinct. |
| CODEX-END-1 | Native `final_answer` phase survives event-only and response forms. A phase does not complete its turn. Completed command snapshots preserve failed execution status. |
| CODEX-TOOL-1 | A generated command invocation and its failed result remain separate, linked by scoped `result_of`. Command output that resembles base64 remains plain text. |
| CODEX-PART-1 | Summary parts keep distinct indices and deltas update their addressed part. Checkpoints preserve the same canonical JSON details. |
| CODEX-TIME-1 | Missing native wire time stays null. Native item milliseconds, file write time, capture time and caller receipt time remain separate. |
| CODEX-USAGE-1 | `edge-usage-three-grains` retains response 10, turn 10 and thread 100, selecting 10. Nullable strict counters include cache write. Repeated last/total snapshots share a derived sample key. |
| CODEX-USAGE-3 | `capture-repeated-usage` and `edge-usage-repeat-and-reset` retain revisions, native counter fields and outer context capacity. Repeated last/total tuples produce repeated_usage_snapshot and contribute once. Scoped cumulative decreases produce usage_counter_decrease, without subtracting totals or inventing a reset epoch. Native thread/connection scopes isolate these comparisons; both counter naming styles preserve capacity. |
| CODEX-USAGE-2 | `file-event-msg-token-count` with `info: null` retains rate limits as thread metadata. Null wire token usage also creates no turn or token sample. This replaces the research draft's unresolved placeholder turn and prevents references to an unemitted turn. |

Reader tests also restore every byte split of the three-grain fixture and
restore the genuine paired file between every row. Each restore equals a
one-shot parse without mutating its prior checkpoint. Focused tests fail
against inert readers; edge regressions were observed red before their fixes.

The [conformance index](../../../docs/parsing/conformance.md) separates research
item views, detailed semantic assertions and the later corpus comparison.
Unknown tool/media/control variants remain native detail with diagnostics.
Snapshot overlap without ordering proof and cumulative counter decreases remain
explicitly unresolved under CODEX-SNAPSHOT-1 and CODEX-USAGE-3. No reset epoch or
cross-source identity is inferred. History conversion, the bounded differential
and large-source performance measurement belong to the cut-over work.

CODEX-TEXT-1: agent-message, reasoning-summary, raw-reasoning and command-output
wire fragments retain whitespace-only text. Native file content parts retain
whitespace too. `TestCodexWhitespaceDeltasRemainContent` and
`TestCodexWhitespaceContentPartKeepsNativeText` reproduced the earlier loss.
Identity and discriminator parsing still require nonempty identifiers.

The item fixtures include schema-only completed snapshots whose native status
is still `inProgress`. Their generation is complete and their native outcome
remains running; those are separate facts. This is a named difference from the
research draft's blanket unknown outcome. Display text now includes native tool
names, web queries and review text where the draft left empty text. Captured
command argv remains intact in native detail while the display uses the extracted
legacy command helper. An image result string gains no inferred encoding.

The item-view layer covers 213 single-input Codex cases against the original
research expectations and the explicit `testdata/item-views.json` differences.
The genuine paired case uses `TestCodexPairedFirstTurn`; the size recipe uses
`TestCodexResearchOversizeRecipeAdvancesSource`, which materializes the specified
33,554,433-character body in memory, verifies bounded checkpoint retention and
source advancement, and reads the following valid row through a saved checkpoint.

- CODEX-NOTICE-1: coordination records without native prose retain the legacy
  readable event label. Child states remain native detail, not parent terminals.
- CODEX-COMPACTION-2: a compaction summary and an adjacent marker with no shared
  native operation ID remain separate evidence. Adjacency cannot prove replay
  or operation identity. Replacement-history entries are not emitted as new
  conversation content.

CODEX-CALL-1: `testdata/ambiguous-call-id` supplies two generated
invocations with one native call ID in one turn, followed by a result. Both
invocations survive; `ambiguous_call_id` and `unresolved_tool_result` explain
why the result cannot choose the last invocation. The checkpoint retains
that ambiguity instead of silently replacing the earlier binding.

CODEX-ACTION-2: a begin event after terminal execution evidence with the same
scoped call ID cannot reopen the earlier action. Subsequent records retain
separate items with `ambiguous_action_call_id`; neither replay nor a new
invocation is guessed. CODEX-TWIN-2 retains multiple possible equal-text twins
as separate items with `ambiguous_serialization_twin`. Both constructed
regressions are in `ambiguous_action_test.go`.

CODEX-EVICTION-1: growing-source continuation retires earlier settled turns
and their item/native/twin/call indexes. The latest turn, unfinished turns and
pending request context remain; of a settled retained turn only the items a
later record can re-emit stay: final answers, call items, items a pending
request names, items whose serialization twin has not arrived, and the
prompts of turns without native ids (the next prompt's boundary reads them)
(`TestGrowingCheckpointsRetireSettledTurns`, one package above); the wire
keeps a settled turn whole for the pane's live rows. Consumers retain emitted output separately
and resolve newly observed native identities against stored rows. Open state
above 4 MiB emits `continuation_limit_exceeded` without dropping evidence.
`TestGrowingCheckpointsRetireSettledTurns` checks file and wire
checkpoint restores; `TestCodexAndGrokOversizedOpenContinuationIsDiagnosed`
checks the warning boundary. Connection-level process and control retention
still needs cut-over review.

CODEX-PROCESS-2: `process/exited` ends byte assembly and evicts the process's
stdout/stderr buffers and item payload from continuation. The consumer keeps
the emitted result, native process-handle alias, separate streams and capture
status. An unknown exit code stays unknown; byte-stream closure does not
complete a conversation turn. `TestCodexProcessStreamsAssembleBytesAcrossChunks`
checks retention before exit and eviction afterward.

CODEX-META-3: rollout rows emit typed catalog fields extracted from the old
metadata adapter during the same decode as transcript assembly. Guardian,
subagent and fork lineage, nested collaboration settings, title/preview
precedence and native clock fallbacks are preserved. `testdata/catalog.jsonl`
is compared with the old adapter after every prefix in history tests.
