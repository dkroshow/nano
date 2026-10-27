# Grok reader extraction

`ParseFile` consumes chunks of `updates.jsonl`; `ParseWire` consumes one ACP
frame. Set `Input.Format` to `json` for a whole summary or sidecar, and set
`Closed` only after its final bytes. Whole documents produce metadata
observations, never transcript turns. `CheckLineage` diagnoses conflicting
native parent facts across those source observations. Both update readers use the decoder extracted from `internal/grokacp` at `115207e`.
The old decoder and history adapters still serve Nano. No call site has moved.

Pass a caller label and incarnation in `model.Input.Source`. Restore `State`
through JSON and resume a file at `State.Framing.Offset`, which includes pending
bytes. Changing source identity, source kind, row limit or checkpoint version
requires a new state. Returned changes upsert logical items and turns; their
keys and positions are local to this source. Treat checkpoint fields as immutable.

This initial reader handles ACP message/thought segments, scoped tool updates,
terminal evidence and prompt usage. Hook/hidden/coordination controls stay
independent observations. Unsupported envelopes and updates remain accounted
for with source references and diagnostics.

## Named changes from the old parser

These are implemented changes with focused assertions. They are not an
exhaustive differential result.

| Name | New behavior | Executable evidence |
| --- | --- | --- |
| GROK-RPC-2 | Pending RPC replay keeps the first request and its scope; conflicting reuse is diagnosed. Matched first replies have typed result_of evidence. Replayed replies cannot repeat native completion or load effects; conflicting replies preserve the first interpretation. Completed IDs can name new requests. Non-string/non-number IDs cannot join the request map, and reverse requests cannot arrive in the client-send direction. | `TestGrokPendingRPCReplayKeepsFirstRequestEvidence`; prompt-reply replay, completed-ID reuse and malformed-ID tests |
| GROK-QUEUE-1 | Empty native queues retire text-binding hints without completing turns. Queue authority requires explicit session identity and provider direction. Only a matching non-replayed terminal can retire the current hint; historical replay does not change current queue binding. | `TestGrokEmptyQueueCannotBindLaterUserToAnOldPrompt`; queue authority and non-replay terminal tests |
| GROK-LOAD-1 | A matched session/load reply records its scoped running prompt without creating a turn; replayed content cannot change it. Missing running ID clears only the reconnect binding, never native outcome/end time. Exact load-reply replay keeps evidence without overwriting a newer queue; conflicting replies are diagnosed. Missing load text cannot bind an empty user chunk. | `TestGrokLoadRunningPromptSurvivesStraddlingReplay`; `TestGrokLoadWithoutRunningPromptClearsOnlyBinding`; load replay and empty-text tests |
| GROK-CONTROL-2 | RPC stopReason supplies terminal evidence only for a matched session/prompt reply, without an RPC error and in the reply direction. Other replies retain unproven_rpc_terminal. Missing-session prompt_complete can use only an exact prompt binding on the same connection; unknown session bindings cannot be dereferenced. RPC and notification terminals preserve different source paths. | `TestGrokRPCStopReasonRequiresPromptReplyAuthority`; `TestGrokMissingSessionTerminalCannotBorrowAnotherConnection` |
| GROK-OBS-1 | Equal deltas with distinct events append; replay keeps another observation without another append. | `duplicate-versus-identical-delta`; `TestReadersPreserveIdenticalDeltasAndReplay` |
| GROK-OBS-2 | Replaying an earlier message does not reopen its segment after tool activity. | `TestReplayDoesNotReopenAnEarlierSegment` |
| GROK-ID-1 | Source-derived segment IDs stay local. Missing native item/turn IDs remain unknown. | `missing-identity-and-time`; `TestWireReaderDoesNotInventNativeTimeOrIdentity` |
| GROK-TIME-1 | File seconds, agent milliseconds, capture time and receipt time stay separate. Unrepresentable native time retains its raw number and a diagnostic. | `TestWireReaderDoesNotInventNativeTimeOrIdentity`; `TestOutOfRangeNativeTimeKeepsCheckpointSerializable` |
| GROK-SCOPE-1 | A child frame cannot inherit its parent's prompt. A terminal for another prompt remains evidence for that prompt. | `TestChildFrameCannotInheritParentPrompt`; `scoped-tool-lifecycle.jsonl` |
| GROK-TOOL-1 | Progress revises an invocation with `progress_of`; terminal output is a separate result with `result_of`. Tool failure does not fail its parent turn. | `TestNativeToolLifecycleDoesNotFailParentTurn` |
| GROK-END-1 | Missing terminal reason stays unknown. Cancellation, interruption and provider error remain distinct outcomes. Successful completion supplies derived finality without a native final phase. | `terminal-missing-reason`, `terminal-cancelled`, `terminal-interrupted`, `terminal-error`; lifecycle test |
| GROK-SCOPE-2 | The constructed paired-segments-tools-late-prompt binds an open same-session user segment to a later explicit prompt, and missing-prompt tool revisions inherit a unique proven call. Old observations keep their original scope. | `TestPairedLatePromptAndCallBinding`; `TestGrokLateBindingSurvivesCheckpointAndUserReplay` |
| GROK-END-2 | Same-event terminal replay retains observations and a replay relation without duplicate completion/usage. Conflicting same-event content is diagnosed without rewriting outcome. Later adverse terminals retract derived finality. | terminal replay, field-order and later-failure tests |
| GROK-META-1 | Whole summaries and sidecars preserve native session identity, parent identity, separate clocks and snapshot detail without creating turns or restamping old models. Conflicting parents remain explicit. | `capture-summary`, `capture-subagent-meta`, `summary-rewrite-lineage-conflict`; document tests |
| GROK-META-3 | Summary catalog fields and native child lineage have typed metadata values beneath their observation. Known/unknown states, native clocks and source scope remain distinct. History can reduce them without native-envelope decoding. | Summary and sidecar document tests; `TestParsedGrokMetadataMatchesExistingCatalogFields` in history |
| GROK-CONTENT-1 | Message images, nested tool images, diffs and ordered plan entries retain typed parts and native detail. | `unknown-coordination-and-message-image`, `capture-tool-nested-image`, `capture-tool-content-diff`, `capture-session-update-plan` |
| GROK-TASK-1 | Background task completion is a thread-scoped command when no prompt is known. Execution success remains separate from truncated output and its reported byte count. | `task-truncated-output-after-turn` |
| GROK-TIME-2 | Native clock regressions are diagnosed within one session and clock field; observations retain source order. | `TestGrokNativeClockRegressionDoesNotReorderContent` |
| GROK-CONTROL-1 | Capture wrappers retain direction, capture sequence/time and native event provenance. Exact same-session queue text can bind a user prompt; replayed user text cannot claim a fresh prompt. Alternate terminals target the proven prompt/session. | `paired-repo-parent-child`; wrapped-capture and replay-binding tests |
| GROK-RPC-1 | Reverse requests and replies retain typed scalar IDs, method and request refs at connection/session scope. Directionless replies with two matching namespaces remain ambiguous. Unknown result credentials are omitted. | `reverse-requests`; RPC namespace/scalar/privacy tests |
| GROK-INTERACTION-1 | Three reverse requests become three interaction items with ordered reply parts. Native option kinds determine permission decisions; ambiguous options or question keys remain unknown. Decisions do not complete tool execution or parent turns. | `TestGrokReverseFixtureKeepsThreeInteractionFacets`, permission and question reply tests |
| GROK-PROVENANCE-1 | Terminal and interaction part paths retain the capture wrapper and identify their native result, error or nested request field. | `TestGrokWrappedTerminalRetainsNativeSourcePath`, `TestGrokInteractionPartsRetainWrappedNativePaths` |
| GROK-TIME-3 | Controls retain caller capture/observed clocks at each observation's resolved scope, without native creation timestamps. | `TestGrokControlClocksKeepCallerAndNativeEvidenceSeparate` |
| GROK-META-2 | Mode/retry updates are session metadata; native envelope metadata and unknown notification fields remain bounded observations. | mode/retry fixtures; `TestGrokUpdateEnvelopeMetadataSurvivesReplay`, `TestGrokUnknownNotificationRetainsNativeFields` |
| GROK-DIRECTION-1 | Sent update-shaped frames have no provider content or completion authority. | `TestGrokClientSentUpdateCannotClaimProviderCompletion` |
| GROK-AUX-1 | Model-history bodies, readable summaries and nested calls remain distinct from ACP turns; exact scoped call IDs support result links. Telemetry retains native fields as observations; unknown rows retain bounded detail. | `capture-chat-history`, `capture-events`; auxiliary source, ambiguity and checkpoint tests |
| GROK-COMPACTION-1 | Known request and checkpoint snapshots produce compaction items with native phases, IDs and clocks. Embedded context creates no turns; unknown schema versions remain diagnosed metadata. | `capture-compaction-requests`, `capture-compaction-checkpoints`; document schema tests |
| GROK-OBSERVATION-1 | Empty draft system/unknown placeholders become control, telemetry, hidden-content, terminal or unknown observations; malformed bytes remain records plus diagnostics. Their evidence does not require empty items. | Exact case lists in `grokResearchViewDifferences`; detailed control, metadata, terminal and framing tests |
| GROK-COORDINATION-1 | Parent coordination notices expose the native spawn description. Child status remains native detail and does not become the notice's execution outcome or the parent's completion. | captured subagent spawn/finish; `TestGrokResearchItemViews` |
| GROK-SEGMENT-1 | Adjacent same-role chunks within the same scoped segment assemble into one item with separate ordered observations. Missing clocks do not create a segment boundary. | `missing-identity-and-time`; replay and clock tests |
| GROK-REPLAY-2 | An explicit replay whose native event is outside retained continuation emits standalone source evidence and a `native.eventId` endpoint. It cannot reopen a turn or bind a replayed user to the current prompt. A replay-only capture preserves both chunks separately for history resolution. | `TestExplicitReplayAfterEvictionKeepsNativeEndpoint`; `TestReplayOnlyCapturePreservesStandaloneEvidence`; `replay-straddles-load` |
| GROK-REPLAY-3 | Event digests retain exact payload and classification equality across delivery clocks and closed-state eviction. History attaches an unmarked late replay to its original row and keeps earlier completion when the later partial turn has no new completion fact. | `testdata/late-replay-after-eviction.jsonl`; `TestGrokEventDigestSurvivesEvictionAndClockChange`; history `TestParsedTranscriptGrokLateReplayKeepsContentAndCompletion` |
| GROK-EVICTION-1 | Earlier settled turns and their item/event/segment indexes leave growing-source continuation. Latest, unfinished and pending-request turns remain, with the items their event, segment and task indexes still name (a replayed event binds to them). Open state above 4 MiB is diagnosed without truncation. | `TestGrowingCheckpointsRetireSettledTurns`; `TestCodexAndGrokOversizedOpenContinuationIsDiagnosed` |
| GROK-USAGE-1 | Prompt usage retains model buckets, incomplete flags and missing counters. A malformed counter cannot hide other valid counters. | lifecycle fixture; `TestMalformedUsageRetainsValidCounters` |

The research snapshots use their original draft schema. The accepted model
removes finality confidence, names the existing history item types and retains
replay observations. Tests assert the relevant semantic subset explicitly;
the unchanged research JSON is not claimed to pass by structural equality.

`TestGrokResearchItemViews` compares type, role, text, phase, outcome and native
turn ID for all 52 single-input cases. It reads the original draft expectations
and applies explicit, named changes listed in the test. It does not generate
expected values from the reader. The four paired/recipe/rewrite cases use their
scenario tests. These item views complement the detailed behavior tests; they
do not by themselves establish full fixture conformance.

## Coverage limits

The [conformance index](../../../docs/parsing/conformance.md) separates research
item views, detailed semantic assertions and the later corpus comparison.
GROK-RPC-1/2, GROK-LOAD-1 and GROK-SCOPE-2 bound request and late-prompt joins by
typed identity, direction and native scope. Missing proof remains unresolved.
GROK-AUX-1 and GROK-COMPACTION-1 preserve unknown sidecar/schema detail with
diagnostics rather than inventing conversation content. Checkpoints retain
latest, unfinished and pending-request turns; settled output belongs to the
consumer. The row limit does not bound unfinished-turn memory. The cut-over
requires large-source measurements and converted-store comparison.

`testdata/scoped-tool-lifecycle.jsonl` is a constructed regression fixture.
It combines documented ACP fields and makes no claim of a new live capture.

GROK-BINDING-2: binding a native prompt emits a distinct revision for every
already assembled item, ordered by first source position. The constructed
checkpoint in `TestPromptBindingEmitsOrderedIndependentRevisions` exposed
reused range-variable pointers under this module's Go 1.21 language version.
The test checks the emitted changes, not only the retained state map.

GROK-PROMPT-INDEX-1: different native `promptIndex` values separate consecutive
unknown-ID prompts. Equal indices can continue one prompt. The index stays an
item alias and never becomes a fabricated native prompt ID. A later explicit
`promptId` binds only the newest unknown prompt; exact event replay still
reaches an earlier prompt after a serialized checkpoint.
`testdata/indexed-prompts` reproduces two bounded-corpus cases.
