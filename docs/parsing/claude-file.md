# Claude Code historical file format

This page adapts the September 2026 native-format study. Its sample counts,
code comparisons and probe results describe that dated inspection. References
to the “current parser” below mean the parser inspected then. They do not
certify the new readers' behavior.

Use the [model contract](model.md) for normalized meanings and the
[Claude reader notes](../../internal/transcriptparse/claude/README.md)
for implemented coverage and remaining differences. The [dump command](command.md)
lets you inspect native records. Proposed standard fields in the study are
research guidance; the accepted model uses `Item` and `Turn`, separate capture,
outcome and finality, and no confidence score.

The new reader preserves all native fields in the 23 captured attachment
subtypes as bounded, scoped observations. Titles, settings, backup and queue
records also remain observations; unknown metadata retains its payload and a
diagnostic. They do not create the empty system items proposed by the study.
Summary text and compaction boundaries produce compaction items without a new
human turn. The task-journal `started`/`result` envelope is a separate source
fact and cannot enter conversation completion evidence. Session cost metadata
keeps its native start clock and totals outside per-turn token usage.

Command, supported Edit/Write and plan-bearing mode-tool inputs use specialized
item types. Their first part retains native arguments while the item's text
provides a command, path or plan display. These items describe the invocation;
they do not prove successful execution, applied edits or plan approval.

Fields outside message content remain in a scoped envelope observation.
Prompt IDs and API request IDs have separate item aliases and do not become
native turn IDs. Effort, service tier and native stop reasons remain available
alongside normalized turn summaries.

This reference covers observed shapes, not a complete vendor schema. It combines current Nano code, its fixtures, and a bounded local scan. No Claude process was started. No provider files or repository files were changed.

Research date: 2026-09-07. Repository HEAD at inspection: `db88af8b44ed712a99283bf5fd147ea072700b00`. The inspected source had unrelated worktree changes. Code citations refer to this inspection, not a frozen vendor specification. Current adapters identify themselves as `claude-transcript-v11`, `claude-completion-v3`, and `claude-metadata-v5`.

## Source population and scan limits

The path enumeration found 5,315 JSONL files: 4,497 session transcripts, 811 agent transcripts, and seven `journal.jsonl` files beneath subagent directories. The scan selected 80 newest and up to 120 seeded random files from each of two path groups: outside and inside `subagents/`. Seed: 2796. This selected 200 session files, 196 worker transcripts, and four worker journals. The initial counter called all 818 files under `subagents/` workers; the second pass corrected the distinction.

Each file contributed at most 4 MiB. Larger files contributed their first and last 2 MiB; incomplete segment-edge lines were skipped. The scan parsed 81,279 rows from 263,858,289 sampled bytes: **81,087 transcript rows and 192 journal rows**. It skipped 27 boundary fragments. It found no malformed complete row in this sample. This is a stratified convenience sample, not a prevalence estimate for the whole corpus. Newest-file bias, long-file omission, and active-file changes limit conclusions.

The [research archive](evidence/README.md) retains these study artifacts: `claude/corpus-counts.json` records counts, field inventories, selected source aliases, file sizes and the scan rule. `claude/edge-evidence.json` separates journals and records additional fields. No private path or message text is needed to inspect these results. The 878 timestamp decreases and seven repeated UUID observations are sample observations, not proof of duplicate semantic messages or a particular producer bug.

Versions observed on rows span 2.1.167–2.1.257, with gaps. Versionless rows inherit no guaranteed version. The largest version groups were 2.1.237 (29,380 rows), 2.1.257 (16,055), 2.1.222 (5,698), and 2.1.220 (4,863). `entrypoint` occurred as `nano` 53,405 times, `cli` 15,270 times, and `sdk-cli` 833 times. These are row counts, not session counts.

## Files and framing

The main file is `~/.claude/projects/<encoded-project>/<session-uuid>.jsonl`. A worker file is `<parent-session>/subagents/agent-<agent-id>.jsonl`. Parent linkage comes from its enclosing directory and row `sessionId`; worker identity comes from filename and `agentId`. `isSidechain:true` means secondary to the parent, but the same rows are primary in their dedicated worker file.

A line is a JSON object. Empty lines are ignorable framing. An unfinished final line is not an accepted record yet. Store byte offset, line number, raw-line hash and source incarnation independently from message identity. Reading a tail without earlier context cannot recover every tool link or turn boundary.

The native stream contains no proven universal turn id. `promptId` is present in many user rows, but the scan does not establish it as a shared assistant/tool turn identifier. Keep it as a native prompt alias. `parentUuid` describes message ancestry, not a turn id. `sessionId` identifies the conversation. Do not equate any of these with Nano's turn ordinal.

Native timestamps in the sample all used UTC RFC3339 strings with **three fractional digits**: 70,968 rows. Many housekeeping rows lack a timestamp. File mtime is file activity, not a missing message time. `cost-state.startTime` is numeric epoch milliseconds. Nano's generic timestamp reader also tolerates RFC3339Nano and numeric seconds/milliseconds; that tolerance is broader than this sample.

[Discovery](../../internal/history/discovery.go) accepts Claude JSONL and classifies dedicated worker paths. The module emits typed [catalog facts](../../internal/transcriptparse/claude/catalog.go); history applies filename identity fallback. Worker conversation scope is handled by [shared assembly](../../internal/transcriptparse/claude/assemble.go).

## Top-level coverage

“Housekeeping” means a durable lazy evidence item remains. It does not mean bytes disappear. The module keeps [metadata observations](../../internal/transcriptparse/claude/metadata.go); [history conversion](../../internal/history/transcript_parsed_batch.go) retains their source evidence handles.

| Type | Sample rows | Meaning and write trigger | Old parser (db88af8) | Scrubbed fixture |
|---|---:|---|---|---|
| `queue-operation` | 1,421 | A queued prompt is enqueued, dequeued or removed. Queue state is not accepted human-turn completion. | Recognized housekeeping; no queue semantics. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-queue-operation/input.jsonl) |
| `user` | 14,506 | Human input, tool output, injected context, notification or compact summary. Written when accepted or appended to context. Type alone does not identify the author. | Typed content; compaction and injected paths; partial origin handling. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-user/input.jsonl) |
| `attachment` | 31,467 | Context and harness material attached while assembling or advancing a request. Exact producer timing is inferred from content, not instrumented. | Top-level recognized; all attachment subtypes collapse to housekeeping, without nested drift diagnostics. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-attachment/input.jsonl) |
| `ai-title` | 3,277 | Generated session title, repeatedly appended as it changes. | Metadata fallback title; transcript housekeeping. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-ai-title/input.jsonl) |
| `atis-latch` | 2,845 | A persistent atis string keyed by sessionId. Exact purpose and write trigger unconfirmed. | Unknown top-level type; diagnostic and unknown evidence item. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-atis-latch/input.jsonl) |
| `assistant` | 22,802 | One or more completed content blocks from an API message. New releases write several rows per message.id. Client-generated error stubs use the same row type. | Text/thinking/tools supported; block index inferred, native apiBlockIndex ignored; error flags ignored. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-assistant/input.jsonl) |
| `last-prompt` | 3,243 | Latest-prompt preview and optional leafUuid. | Metadata preview; transcript housekeeping. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-last-prompt/input.jsonl) |
| `system` | 733 | Lifecycle, error, hook, local-command and compaction evidence. Trigger depends on subtype. | Subtype switch; lifecycle items, hidden housekeeping or unknown stub. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-system/input.jsonl) |
| `mode` | 290 | Persisted mode setting; producer writes on changes or saves. | Recognized housekeeping; mode value not promoted by this metadata adapter. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-mode/input.jsonl) |
| `permission-mode` | 284 | Persisted permission setting. Often lacks timestamp. | Metadata permission plus transcript settings notice. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-permission-mode/input.jsonl) |
| `file-history-snapshot` | 158 | Undo/checkpoint snapshot associates a message with tracked backups; isSnapshotUpdate distinguishes refreshes. | Recognized housekeeping; no file-change semantics. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-file-history-snapshot/input.jsonl) |
| `file-history-delta` | 39 | An incremental file backup/tracking update. Also supported as a system subtype in older files. | Recognized housekeeping; not an applied edit. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-file-history-delta/input.jsonl) |
| `bridge-session` | 1 | Bridge-session identity and sequence/account ownership metadata. Exact lifecycle unconfirmed. | Unknown top-level type; diagnostic and unknown evidence item. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-bridge-session/input.jsonl) |
| `cost-state` | 1 | Session cost/duration/tool/file-change accumulators; epoch-ms startTime. Exact checkpoint timing unconfirmed. | Unknown type; must not treat these cumulative numbers as per-turn usage. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-cost-state/input.jsonl) |
| `agent-setting` | 1 | Persisted agentSetting string. Exact semantics and trigger unconfirmed. | Unknown top-level type; diagnostic and unknown evidence item. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-agent-setting/input.jsonl) |
| `agent-name` | 10 | Persisted agent display name. | Recognized housekeeping; no native worker-link inference. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-agent-name/input.jsonl) |
| `custom-title` | 9 | Explicit session title. | Metadata preferred title; transcript housekeeping. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-custom-title/input.jsonl) |
| `started` | 96 | Worker journal entry with key and agentId. This is journal.jsonl, not the agent transcript. | Discovery accepts the JSONL; parser type unknown; source should first be classified as journal. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-started/input.jsonl) |
| `result` | 96 | In this scan, only worker journal result entries (key, agentId, result). Stream-json result is a different shape with the same type name. | File parser blindly completes a semantic turn for any result; does not distinguish journal or is_error. | [input](../../internal/transcriptparse/claude/testdata/research/capture-row-result/input.jsonl) |

Additional code/fixture vocabulary with **zero sampled rows**: top-level `summary`, `progress`, `worktree-state`; system `turn-duration`, `compaction`, `init`, `status`, `hook_started`, `hook_response`, `scheduled_task_fire`, `worktree-state`, `file-history-delta`; assistant `redacted_thinking`, `server_tool_use`; stop reason `api_error`. A missing sample is not evidence that the shape is impossible. `summary` carries summary/leaf data and becomes a compaction notice; `progress` and `worktree-state` remain housekeeping. Exact progress payload variants were not established by this sample.

## Record field inventory and examples

Fields below are a union, not required properties. Fields such as error, message, toolUseResult and attachment have variant-dependent shapes. The linked inputs are valid, short scrubbed examples; they are preferable to reconstructing a required schema from this table.

### `queue-operation`

Fields observed: `type`, `operation`, `timestamp`, `sessionId`, `content`, `reason`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-queue-operation/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-queue-operation/expected.json).

### `user`

Fields observed: `parentUuid`, `isSidechain`, `promptId`, `type`, `message`, `uuid`, `timestamp`, `permissionMode`, `promptSource`, `userType`, `entrypoint`, `cwd`, `sessionId`, `version`, `gitBranch`, `toolUseResult`, `sourceToolAssistantUUID`, `origin`, `queueSkipAttachments`, `isMeta`, `turnCompanion`, `sourceToolUseID`, `session_id`, `toolDenialKind`, `interruptedMessageId`, `userFeedback`, `slug`, `isVisibleInTranscriptOnly`, `isCompactSummary`, `sessionKind`, `agentId`, `toolEndsTurn`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-user/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-user/expected.json).

### `attachment`

Fields observed: `parentUuid`, `isSidechain`, `attachment`, `type`, `uuid`, `timestamp`, `userType`, `entrypoint`, `cwd`, `sessionId`, `version`, `gitBranch`, `session_id`, `slug`, `sessionKind`, `agentId`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-attachment/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-attachment/expected.json).

### `ai-title`

Fields observed: `type`, `aiTitle`, `sessionId`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-ai-title/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-ai-title/expected.json).

### `atis-latch`

Fields observed: `type`, `atis`, `sessionId`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-atis-latch/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-atis-latch/expected.json).

### `assistant`

Fields observed: `parentUuid`, `isSidechain`, `message`, `apiBlockIndex`, `requestId`, `type`, `uuid`, `timestamp`, `effort`, `userType`, `entrypoint`, `cwd`, `sessionId`, `version`, `gitBranch`, `attributionSkill`, `session_id`, `error`, `isApiErrorMessage`, `apiErrorStatus`, `slug`, `sessionKind`, `agentId`, `attributionAgent`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-assistant/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-assistant/expected.json).

### `last-prompt`

Fields observed: `type`, `lastPrompt`, `leafUuid`, `sessionId`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-last-prompt/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-last-prompt/expected.json).

### `system`

Fields observed: `parentUuid`, `isSidechain`, `type`, `subtype`, `hookCount`, `hookInfos`, `hookErrors`, `hookAdditionalContext`, `preventedContinuation`, `stopReason`, `hasOutput`, `level`, `timestamp`, `uuid`, `toolUseID`, `userType`, `entrypoint`, `cwd`, `sessionId`, `version`, `gitBranch`, `session_id`, `durationMs`, `messageCount`, `pendingBackgroundAgentCount`, `isMeta`, `error`, `retryInMs`, `retryAttempt`, `maxRetries`, `source`, `slug`, `logicalParentUuid`, `content`, `compactMetadata`, `sessionKind`, `direction`, `scope`, `trigger`, `originalModel`, `fallbackModel`, `requestId`, `apiRefusalCategory`, `apiRefusalExplanation`, `retractedMessageUuids`, `refusedUserMessageUuid`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-system/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-system/expected.json).

### `mode`

Fields observed: `type`, `mode`, `sessionId`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-mode/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-mode/expected.json).

### `permission-mode`

Fields observed: `type`, `permissionMode`, `sessionId`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-permission-mode/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-permission-mode/expected.json).

### `file-history-snapshot`

Fields observed: `type`, `messageId`, `snapshot`, `isSnapshotUpdate`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-file-history-snapshot/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-file-history-snapshot/expected.json).

### `file-history-delta`

Fields observed: `type`, `messageId`, `snapshotMessageId`, `trackingPath`, `backup`, `timestamp`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-file-history-delta/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-file-history-delta/expected.json).

### `bridge-session`

Fields observed: `type`, `sessionId`, `bridgeSessionId`, `lastSequenceNum`, `ownerAccountUuid`, `ownerOrganizationUuid`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-bridge-session/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-bridge-session/expected.json).

### `cost-state`

Fields observed: `type`, `sessionId`, `totalCostUSD`, `totalAPIDuration`, `totalAPIDurationWithoutRetries`, `totalToolDuration`, `totalLinesAdded`, `totalLinesRemoved`, `totalDuration`, `startTime`, `modelUsage`, `hasUnknownModelCost`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-cost-state/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-cost-state/expected.json).

### `agent-setting`

Fields observed: `type`, `agentSetting`, `sessionId`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-agent-setting/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-agent-setting/expected.json).

### `agent-name`

Fields observed: `type`, `agentName`, `sessionId`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-agent-name/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-agent-name/expected.json).

### `custom-title`

Fields observed: `type`, `customTitle`, `sessionId`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-custom-title/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-custom-title/expected.json).

### `started`

Fields observed: `type`, `key`, `agentId`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-started/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-started/expected.json).

### `result`

Fields observed: `type`, `key`, `agentId`, `result`.

[Scrubbed example](../../internal/transcriptparse/claude/testdata/research/capture-row-result/input.jsonl); [expected standard output](../../internal/transcriptparse/claude/testdata/research/capture-row-result/expected.json).

## Message and content variants

Common envelope fields include `uuid`, `parentUuid`, `sessionId`, `timestamp`, `version`, `entrypoint`, `cwd`, `gitBranch`, `isSidechain`, and optional `agentId`. They do not all appear on every row. `message` carries a native API `id`, `role`, `model`, `content`, `stop_reason`, `stop_sequence`, `usage` and newer optional `stop_details`, `diagnostics`, `context_management` or `container` fields. Keep opaque extras bounded.

`message.content` can be a string or a block array. Nano's file parser also tolerates arrays mixing strings and objects, a code-supported case not observed here. It discards unsupported scalar entries. User-role rows often carry tool results; role=user in the envelope does not make tool output human-authored.

| Block or form | Count | Fields and meaning | Old parser (db88af8) |
|---|---:|---|---|
| `user.content:string` | 522 | `string` — Text input or injected context | Decoded into text block; [example](../../internal/transcriptparse/claude/testdata/research/bare-string-message/input.jsonl) |
| `user.block:text` | 578 | `type,text` — Text input; flag/origin determines author | User item or injected notice; [example](../../internal/transcriptparse/claude/testdata/research/capture-user-block-text/input.jsonl) |
| `user.block:tool_result` | 13,398 | `type,tool_use_id,content,is_error` — Tool reply; content string or typed array | Tool result; AskUserQuestion correlation can become user interaction; [example](../../internal/transcriptparse/claude/testdata/research/capture-user-block-tool-result/input.jsonl) |
| `user.block:image` | 35 | `type,source` — Image content supplied with prompt | Unknown content diagnostic; image not retained as typed part; [example](../../internal/transcriptparse/claude/testdata/research/capture-user-block-image/input.jsonl) |
| `assistant.block:text` | 3,392 | `type,text` — Assistant text block | Message item; [example](../../internal/transcriptparse/claude/testdata/research/capture-assistant-block-text/input.jsonl) |
| `assistant.block:thinking` | 6,001 | `type,thinking,signature` — Reasoning text and opaque signature | Reasoning text; signature not promoted; [example](../../internal/transcriptparse/claude/testdata/research/capture-assistant-block-thinking/input.jsonl) |
| `assistant.block:tool_use` | 13,415 | `type,id,name,input` — Tool invocation; input object | Tool/command/file-change/interaction heuristic; [example](../../internal/transcriptparse/claude/testdata/research/capture-assistant-block-tool-use/input.jsonl) |
| `assistant.block:fallback` | 4 | `type,from,to` — Provider model fallback marker | System notice; [example](../../internal/transcriptparse/claude/testdata/research/capture-assistant-block-fallback/input.jsonl) |

`redacted_thinking {type,data}` is code-supported and becomes a redaction notice. It should retain its opaque redacted content type. `server_tool_use {type,id,name,input}` follows the tool path, but server-result block variants were not found. Unknown blocks need explicit unknown items with their block paths. A whole-row fallback alone cannot identify which nested block failed.

`toolUseResult` is a separate top-level result object. It can contain structured stdout/stderr, file details, interruption flags, or `questions` and `answers`. Observed `type` discriminators included `text` (1,414), `create` (205), `image` (31), `update` (15), `parts` (1), and `file_unchanged` (1). These counts cover only objects with a type field. They are not the full tool-result count. Answers use question text as map keys. Real multi-select arrays remain unconfirmed in this sample; code tolerates them.

## System subtype coverage

- `stop_hook_summary`: **572** rows. Hook completion summary; hookCount/hookInfos/hookErrors/hookAdditionalContext, preventedContinuation, stopReason, hasOutput, level, toolUseID. Hidden housekeeping. A hook summary is not sufficient turn completion. [Example](../../internal/transcriptparse/claude/testdata/research/capture-system-stop-hook-summary/input.jsonl).
- `turn_duration`: **122** rows. Turn timing: durationMs, optional messageCount and pendingBackgroundAgentCount. Completes the current semantic turn in Nano. [Example](../../internal/transcriptparse/claude/testdata/research/capture-system-turn-duration/input.jsonl).
- `api_error`: **18** rows. Provider error/retry report: error, retryInMs, retryAttempt, maxRetries. Emits a failed notice; does not close the turn. [Example](../../internal/transcriptparse/claude/testdata/research/capture-system-api-error/input.jsonl).
- `compact_boundary`: **12** rows. Compaction boundary: logicalParentUuid, content, compactMetadata including trigger/preTokens/postTokens/duration/preservedSegment/preservedMessages/cumulativeDroppedTokens. Emits compaction but drops these details. [Example](../../internal/transcriptparse/claude/testdata/research/capture-system-compact-boundary/input.jsonl).
- `local_command`: **8** rows. Local command echo/context; content and level. Hidden housekeeping. [Example](../../internal/transcriptparse/claude/testdata/research/capture-system-local-command/input.jsonl).
- `model_refusal_fallback`: **1** rows. Fallback after refusal: originalModel, fallbackModel, requestId, retractedMessageUuids, refusedUserMessageUuid, trigger, direction, scope, refusal metadata. Recognized but hidden; retraction is not applied. [Example](../../internal/transcriptparse/claude/testdata/research/capture-system-model-refusal-fallback/input.jsonl).

## Attachment subtype coverage

These attachments are native context records, not ordinary human messages. All currently hit the same top-level housekeeping branch. Nano does not validate the nested type vocabulary. Fields below are observed; each fixture preserves an example with neutral values. “When” describes the attachment's apparent purpose; exact emission cadence was not traced.

| Attachment type | Rows | Apparent purpose / when | Observed fields | Example |
|---|---:|---|---|---|
| `deferred_tools_delta` | 326 | Tool registry changes | `addedLines`, `addedNames`, `failedMcpServers`, `needsAuthMcpServers`, `pendingMcpServers`, `readdedNames`, `removedNames`, `type`, `wireHiddenNames` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-deferred-tools-delta/input.jsonl) |
| `agent_listing_delta` | 110 | Agent registry changes | `addedLines`, `addedTypes`, `isInitial`, `removedTypes`, `showConcurrencyNote`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-agent-listing-delta/input.jsonl) |
| `skill_listing` | 300 | Available skills supplied | `content`, `isInitial`, `names`, `skillCount`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-skill-listing/input.jsonl) |
| `auto_mode` | 72 | Automatic-mode instructions/settings | `autoModeConsentFlow`, `bashFirst`, `bypass`, `steerOnly`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-auto-mode/input.jsonl) |
| `total_tokens_reminder` | 5,527 | Context token reminder | `text`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-total-tokens-reminder/input.jsonl) |
| `output_style` | 5,439 | Output-style turn reminder | `style`, `turnReminder`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-output-style/input.jsonl) |
| `async_hook_response` | 18,379 | Asynchronous hook returned | `exitCode`, `hookEvent`, `hookName`, `processId`, `response`, `stderr`, `stdout`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-async-hook-response/input.jsonl) |
| `batching_reminder_sent` | 851 | Batching guidance injected | `model`, `text`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-batching-reminder-sent/input.jsonl) |
| `silent_turn_reminder` | 79 | Silent-turn guidance injected | `text`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-silent-turn-reminder/input.jsonl) |
| `queued_command` | 23 | Queued command enters context | `commandMode`, `origin`, `prompt`, `timestamp`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-queued-command/input.jsonl) |
| `date_change` | 46 | Date context updated | `newDate`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-date-change/input.jsonl) |
| `command_permissions` | 12 | Allowed tools supplied | `allowedTools`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-command-permissions/input.jsonl) |
| `task_reminder` | 110 | Task list reminder | `content`, `itemCount`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-task-reminder/input.jsonl) |
| `edited_text_file` | 41 | Changed file context supplied | `filename`, `snippet`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-edited-text-file/input.jsonl) |
| `hook_success` | 26 | Hook succeeded | `command`, `content`, `durationMs`, `exitCode`, `hookEvent`, `hookName`, `stderr`, `stdout`, `toolUseID`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-hook-success/input.jsonl) |
| `read_truncation_notice` | 47 | Read output was bounded | `banner`, `toolUseID`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-read-truncation-notice/input.jsonl) |
| `sandbox_instructions` | 1 | Sandbox context supplied | `content`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-sandbox-instructions/input.jsonl) |
| `hook_non_blocking_error` | 9 | Hook failed without blocking | `command`, `durationMs`, `exitCode`, `hookEvent`, `hookName`, `stderr`, `stdout`, `toolUseID`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-hook-non-blocking-error/input.jsonl) |
| `file` | 22 | File context attached | `content`, `displayPath`, `filename`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-file/input.jsonl) |
| `compact_file_reference` | 37 | File reference survives compaction | `displayPath`, `filename`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-compact-file-reference/input.jsonl) |
| `hook_additional_context` | 7 | Hook adds context | `content`, `hookEvent`, `hookName`, `toolUseID`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-hook-additional-context/input.jsonl) |
| `goal_status` | 2 | Goal progress/status reminder | `condition`, `durationMs`, `iterations`, `met`, `reason`, `sentinel`, `tokens`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-goal-status/input.jsonl) |
| `plan_mode` | 1 | Plan-mode context supplied | `isSubAgent`, `planExists`, `planFilePath`, `reminderType`, `type` | [input](../../internal/transcriptparse/claude/testdata/research/capture-attachment-plan-mode/input.jsonl) |

## Identity, order, duplicates and split messages

The sample contained 11,807 repeated `message.id` observations. Many are normal split-message rows, not duplicates. Newer rows include **`apiBlockIndex` (4,031 sampled rows)**. The native index is scoped to the API message. A composite `message.id:index` is a derived key assembled from native fields; it is not a native opaque item id.

The module keeps a block map per API message and honors explicit `apiBlockIndex`. Counted indices remain provisional when their native position is absent. Interleaved messages and replay have executable assertions. The original focused scan found no non-incrementing explicit indices or returns to an earlier indexed message; those examples are stress cases, not a frequency claim.

Seven repeated `uuid` observations appeared within the selected research samples. Source position and native identity remain available to distinguish replay from repeated content. Identical prose under distinct block identities stays distinct. The module uses [native replay evidence](../../internal/transcriptparse/claude/replay.go); history resolves late references after parser state eviction.

Source order is the stable observation order. Timestamps decreased 878 times within sampled segments. Do not sort rows by time to repair them. Compaction can reset parentUuid and preserve references to older messages. `model_refusal_fallback.retractedMessageUuids` explicitly revises earlier output. These are logical revisions. This single scan did not watch any physical file rewrite occur.

The pre-cut-over parser probes reproduce the split-index and completion gaps with synthetic inputs; see `claude/current-parser-probe-summary.json` ([research archive](evidence/README.md)). They do not establish frequency of those failures in native sessions.

Implementation: [block assembly](../../internal/transcriptparse/claude/assemble.go), [native replay](../../internal/transcriptparse/claude/replay.go), and [history identity resolution](../../internal/history/transcript_parsed_item.go). The research probe code refers to the retired parser, available through the retired parser.

## Origins and worker scope

Observed flags: isSidechain 16,105; isMeta 61; isCompactSummary 12; isVisibleInTranscriptOnly 12; isApiErrorMessage 2. These overlap. Observed origins: human 111, task-notification 42, coordinator 12. `sessionKind:bg` occurred eight times. `promptSource` values were sdk 655, typed 119, queued 3, and system 13. Keep these dimensions separate.

`entrypoint:cli` and `nano` are currently classified as interactive; `sdk-cli` as programmatic. That is launch provenance, not proof that every message was human-written. No distinct Claude guardian record was confirmed. A guardian flag requires external Nano attribution; do not infer it from `sdk-cli` or model choice.

Catalog message activity retains its established origin policy: nonempty origin objects do not advance that clock. Transcript observations separately preserve native authorship, synthetic context and notification evidence. These facts serve different consumers; neither catalog recency nor role alone establishes a human turn.

Workers keep their own usage and completion. Inline sidechain rows must not close the parent turn. `Agent` tool calls, textual async launch results, and XML-like `<task-notification>` bodies provide parent coordination evidence; they do not contain a native parent turn id.

## Compaction, stops, errors and partial files

`summary` and `system.compact_boundary` differ from `user.isCompactSummary`. The last is a machine-generated context summary and must not start a new human turn. Preserve summary content and lineage; the current parser emits only notices. `isVisibleInTranscriptOnly` must remain a separate native visibility fact.

Observed stop reasons: tool_use 15,702; end_turn 1,462; max_tokens 2; stop_sequence 3. Many assistant rows omit or null the reason. An end_turn on an intermediate split row is provisional until the message/turn settles. Tool use means invocation generation finished, not that execution succeeded.

The scan found 29 `[Request interrupted by user]` markers and two `[Request interrupted by user for tool use]` markers. Nano recognizes only the first exact bracket string, plus `<turn_aborted>` substring. The second currently becomes user text. An observed synthetic API-error row had stop_sequence, so stop reason alone is not sufficient final-answer evidence.

A wire-shaped `result` may be success, failure or interruption. The historical handler treats all `result` types as completion and ignores is_error and terminal_reason. In this scan every file result came from worker journals, underlining why source kind must precede event interpretation. `system.turn_duration` also reopens completed/truncated turns; a following duration can overwrite max_tokens truncation with completion. Duration and outcome need independent fields.

EOF is not successful completion. Missing final newline is partial framing. Malformed complete JSON needs a diagnostic and unknown item. Nano defers incomplete tail lines, emits unknown evidence for malformed complete lines, and diagnoses oversized lines. Its default historical row cap is 32 MiB. Oversized rows currently receive a diagnostic without the same row evidence stub. Native truncation, source truncation, and display/body truncation are different conditions.

The transcript's normalized inline text cap is 8 KiB; retained body cap is 1 MiB; detail cap is 8 KiB. These are Nano storage limits, not provider limits. Source replacement/truncation needs a new source incarnation and reconciliation; this research supplies a synthetic mutation fixture rather than touching any native file.

Completion and interruption evidence live in [shared assembly](../../internal/transcriptparse/claude/assemble.go) and [completion rules](../../internal/transcriptparse/claude/completion.go). [File framing](../../internal/transcriptparse/claude/reader.go) applies the native row limit; [history view helpers](../../internal/history/transcript_view_helpers.go) apply display/body budgets.

## Usage and sidecars

`message.usage` holds input_tokens, output_tokens, cache_read_input_tokens and cache_creation_input_tokens. Optional fields include output_tokens_details.thinking_tokens, cache_creation.ephemeral_5m_input_tokens/ephemeral_1h_input_tokens, server_tool_use, service_tier, inference_geo, iterations and speed. Thinking tokens are part of output, not additive. Input excludes cache in this provider's contract. Do not invent a total.

Repeated rows can repeat or revise usage for one API message. Current Nano keeps the latest observation per message.id, then sums distinct ids inside its inferred conversation turn. Worker scope is excluded from the parent sum. Missing message ids fall back to turn-level last-wins. Missing values remain nullable; a partial sum needs a coverage indicator so it cannot masquerade as complete usage. The current parser promotes cache-duration splits but drops most other usage extras.

Worker `.meta.json` files are companions, not transcript rows. Enumeration found 855 such files. A bounded read of 30 sorted files found agentType, description, toolUseId and spawnDepth in all 30; model in 14; parentAgentId in three. This does not establish a required schema. Keep the toolUseId link and parentAgentId separate from session identity. Other sidecar files include tool-results text/images/documents and saved plan files. Their contents were not copied. They can be referenced by native tool results or file attachments and should stay lazy, bounded resources.

`subagents/.../journal.jsonl` contains started/result records with key and agentId. It is a task journal, not the worker's conversation. There is no demonstrated wire equivalent for this journal. Snapshot backup fields can refer to external backup resources; their existence does not mean an assistant edit was applied.

The shim's spool is a different file format even though it is JSONL. Its seq/ts/dir/line wrapper belongs to Nano and carries both directions of live stream-json. It is covered in [the Claude wire reference](claude-wire.md), not counted as historical Claude transcript data.

Usage implementation: [native response observations](../../internal/transcriptparse/claude/assemble.go), [result aggregates](../../internal/transcriptparse/claude/usage.go), and [grain-aware reduction](../../internal/transcriptparse/internal/assemble/usage.go). History exposes its existing [turn usage contract](../decisions/thread-turn-usage.md) at the conversion boundary.

## Added paired evidence (2026-09-09)

The [genuine paired assistant excerpt](../../internal/transcriptparse/claude/testdata/captured/paired-assistant-excerpt/manifest.md)
contains two split rows with one API message ID and native `apiBlockIndex`
values 0 and 1. Their UUIDs and content match the corresponding wire snapshots.
The file stores final usage and `end_turn` on both rows, while the wire initially
stores partial usage and null stop reasons. Selecting the shared response
once gives 52 output tokens in either source. The wire also has a successful
native result, so finality evidence differs. The manifest records selection,
scrubbing and the limits of this excerpt.

Source replacement uses a new source incarnation and a fresh checkpoint.
`CLAUDE-REWRITE-1` tests the research mutation recipe: item IDs and line positions
change, while native `claude.rowUUID` aliases survive. A reduced row budget
produces explicit source truncation; it does not imply model truncation.
`CLAUDE-CONTEXT-1` names inclusion of native setup prompts and content omitted
by focused draft views. The exact case changes are in
`internal/transcriptparse/claude/testdata/item-views.json`.

`CLAUDE-CALL-1` keeps a reused native call ID ambiguous when two
distinct invocations occur in one turn. Later results retain their content
with diagnostics; they cannot choose the last invocation merely by position.

`CLAUDE-QUESTION-3` keeps row-level structured answers separate when several
native result blocks share the row. Their individual text and proven call
relationships survive. The complete row overlay remains in envelope detail
with `ambiguous_question_answer_overlay`; it is not assigned to blocks by guess.
