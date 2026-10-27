# Codex historical file format

This page adapts the September 2026 native-format study. Its sample counts,
code comparisons and probe results describe that dated inspection. References
to the “current parser” below mean the parser inspected then. They do not
certify the new readers' behavior.

Use the [model contract](model.md) for normalized meanings and the
[Codex reader notes](../../internal/transcriptparse/codex/README.md)
for implemented coverage and remaining differences. The [dump command](command.md)
lets you inspect native records. Proposed standard fields in the study are
research guidance; the accepted model uses `Item` and `Turn`, separate capture,
outcome and finality, and no confidence score.

Research date: 2026-09-07. Repository revision: `db88af8b44ed712a99283bf5fd147ea072700b00`.
This is a bounded evidence reference, not a claim to know every future provider shape.

## Evidence and coverage

The tree contained 1,675 rollouts (4,342,909,342 bytes). The primary sample read 130 files, 190,703 complete rows and 411,793,770 bytes. It spans October 2025 through September 2026 and CLI headers 0.51.0-alpha.2 through 0.153.3.

Selection: 10 date-spaced files per month, 20 largest, 15 smallest, 20 latest; unique union. Full files <=12 MiB; otherwise first/last 6 MiB trimmed to complete line boundaries. No corpus-wide content scan. Samples may be live and are not a transaction snapshot. Twenty-one files were sampled at both ends. Counts are sample occurrences, not provider-wide frequencies. Absence means not observed. The selection overrepresents large, recent and small sessions.

The [research archive](evidence/README.md) retains these study artifacts: `codex/corpus-inventory.json` records counts, field types, date/size strata and hashed source identities with byte segments. `codex/edge-inventory.json` is a second, later read of the complete-file subset. Live files grew between reads: its counts must not be added to the primary table. No timestamps decreased lexically in the primary sample; this does not prove writes can never arrive out of order. No malformed lines were seen in that sample.

Repository evidence includes the named fixture trees, current source, architecture and usage decisions. The July parser audit and start-surface document describe earlier versions; current code/schema take precedence for current behavior. The newer records below are observed facts, not guesses from those older documents.

## Container and identity

A rollout is UTF-8 newline-delimited JSON at `sessions/YYYY/MM/DD/rollout-<date>-<native-thread-id>.jsonl`. One line is one record, not one conversation item. All sampled rows have `timestamp` with exactly three fractional digits and `type`; most have `payload`. Newer rows also have a root `ordinal`. Preserve that ordinal separately from line, item sequence and turn ordinal.

Most historical response rows have no native item id. Use a stable source identity plus line/emission coordinate until native aliases arrive. A repeated session header does not start a new logical conversation. The primary sample had 133 session headers in 130 files and 121 byte-identical duplicate rows within files. Those counts alone do not distinguish resume, copying, and provider re-emission.

Source order is authoritative evidence order; do not sort by timestamp. Envelope write time, event occurrence time, task start/end time and observation time are different clocks. The module retains their meaning, precision and source field in [time observations](../../internal/transcriptparse/codex/time.go). History chooses its display clock at the conversion boundary.

## Shape coverage

“Recognized” means a decoder handles the row, not full semantic fidelity. Known rows with no content emission retain source observations; history supplies lazy evidence handles. Unknown rows retain diagnostics and inspectable evidence. See [file assembly](../../internal/transcriptparse/codex/file.go) and [history conversion](../../internal/history/transcript_parsed_batch.go).

| Shape | Primary corpus rows | Repo fixture rows | Old parser (db88af8) |
|---|---:|---:|---|
| `session_meta` | 133 | 4 | Bookkeeping/intentional semantic skip; housekeeping evidence stub |
| `turn_context` | 4403 | 2 | Bookkeeping/intentional semantic skip; housekeeping evidence stub |
| `compacted` | 199 | 0 | Recognized; reduced text/detail (see mapping) |
| `world_state` | 248 | 1 | Recognized; reduced text/detail (see mapping) |
| `inter_agent_communication_metadata` | 103 | 1 | Recognized; reduced text/detail (see mapping) |
| `token_usage_record` | 2121 | 0 | Unknown diagnostic + inspectable provider-event stub |
| `event_msg/item_completed` | 1603 | 0 | Unknown diagnostic + inspectable provider-event stub |
| `response_item/message` | 16232 | 7 | Recognized; reduced text/detail (see mapping) |
| `response_item/agent_message` | 103 | 0 | Message + sidechain flag; author/recipient not mapped |
| `response_item/reasoning` | 24044 | 1 | Recognized; reduced text/detail (see mapping) |
| `response_item/function_call` | 17651 | 1 | Recognized; reduced text/detail (see mapping) |
| `response_item/custom_tool_call` | 12904 | 0 | Recognized; reduced text/detail (see mapping) |
| `response_item/local_shell_call` | 0 | 0 | Recognized; reduced text/detail (see mapping) |
| `response_item/web_search_call` | 92 | 0 | Recognized; reduced text/detail (see mapping) |
| `response_item/tool_search_call` | 6 | 0 | Recognized; reduced text/detail (see mapping) |
| `response_item/function_call_output` | 17650 | 1 | Recognized; reduced text/detail (see mapping) |
| `response_item/custom_tool_call_output` | 12898 | 0 | Recognized; reduced text/detail (see mapping) |
| `response_item/local_shell_call_output` | 0 | 0 | Recognized; reduced text/detail (see mapping) |
| `response_item/tool_search_output` | 6 | 0 | Recognized; tools payload absent from normalized text |
| `response_item/plan` | 0 | 0 | Recognized; reduced text/detail (see mapping) |
| `event_msg/user_message` | 3640 | 2 | Recognized; reduced text/detail (see mapping) |
| `event_msg/agent_message` | 11790 | 1 | Recognized; reduced text/detail (see mapping) |
| `event_msg/agent_reasoning` | 16610 | 0 | Recognized; reduced text/detail (see mapping) |
| `event_msg/task_started` | 3564 | 0 | Bookkeeping/intentional semantic skip; housekeeping evidence stub |
| `event_msg/task_complete` | 3414 | 1 | Recognized; reduced text/detail (see mapping) |
| `event_msg/turn_aborted` | 157 | 0 | Recognized; reduced text/detail (see mapping) |
| `event_msg/context_compacted` | 196 | 0 | Recognized; reduced text/detail (see mapping) |
| `event_msg/thread_settings_applied` | 1883 | 1 | Recognized; reduced text/detail (see mapping) |
| `event_msg/sub_agent_activity` | 131 | 1 | Recognized; reduced text/detail (see mapping) |
| `event_msg/guardian_assessment` | 534 | 0 | Recognized; reduced text/detail (see mapping) |
| `event_msg/thread_goal_updated` | 1 | 0 | Recognized; reduced text/detail (see mapping) |
| `event_msg/thread_name_updated` | 3 | 0 | Recognized; reduced text/detail (see mapping) |
| `event_msg/web_search_end` | 112 | 0 | Recognized; results payload absent from normalized text |
| `event_msg/mcp_tool_call_end` | 12 | 0 | Recognized; reduced text/detail (see mapping) |
| `event_msg/collab_waiting_end` | 89 | 0 | Recognized; reduced text/detail (see mapping) |
| `event_msg/collab_agent_spawn_end` | 8 | 0 | Recognized; reduced text/detail (see mapping) |
| `event_msg/collab_close_end` | 2 | 0 | Recognized; reduced text/detail (see mapping) |
| `event_msg/collab_agent_interaction_end` | 0 | 0 | Recognized; reduced text/detail (see mapping) |
| `event_msg/error` | 3 | 0 | Recognized; reduced text/detail (see mapping) |
| `event_msg/token_count` | 32257 | 0 | Bookkeeping/intentional semantic skip; housekeeping evidence stub |
| `event_msg/exec_command_begin` | 0 | 0 | Bookkeeping/intentional semantic skip; housekeeping evidence stub |
| `event_msg/exec_command_end` | 3751 | 0 | Bookkeeping/intentional semantic skip; housekeeping evidence stub |
| `event_msg/patch_apply_begin` | 0 | 0 | Bookkeeping/intentional semantic skip; housekeeping evidence stub |
| `event_msg/patch_apply_end` | 2150 | 0 | Bookkeeping/intentional semantic skip; housekeeping evidence stub |
| `future_native_row` | 0 | 1 | Unknown diagnostic + inspectable provider-event stub |
| `future_private_row` | 0 | 1 | Unknown diagnostic + inspectable provider-event stub |

## Record reference

Fields below are unions of observed payload keys. They are not required-field schemas. `codex/corpus-inventory.json` ([research archive](evidence/README.md)) retains per-field types/counts. For code-only shapes, fixture fields are illustrative. Examples replace all private prose and IDs; bounded arrays are excerpts.

### `session_meta` → system

Thread identity and origin at session creation or repeated header; instructions stay injected context.

Fields: `id`, `timestamp`, `cwd`, `originator`, `cli_version`, `instructions`, `source`, `model_provider`, `git`, `base_instructions`, `forked_from_id`, `agent_nickname`, `agent_role`, `thread_source`, `parent_thread_id`, `multi_agent_version`, `session_id`, `agent_path`, `history_mode`, `context_window`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"session_meta","payload":{"id":"id-a851e30611","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/workspace/example","originator":"codex_vscode","cli_version":"0.51.0-alpha.2","instructions":null,"source":"[redacted]","model_provider":"[redacted]","git":{"commit_hash":"[redacted]","branch":"[redacted]","repository_url":"[redacted]"}}}
```

Fixture: [file-session-meta](../../internal/transcriptparse/codex/testdata/research/file-session-meta/manifest.md).

### `turn_context` → turn

Effective settings before model work. Bind turn_id; model/effort may be nested under collaboration_mode.settings.

Fields: `cwd`, `approval_policy`, `sandbox_policy`, `model`, `effort`, `summary`, `collaboration_mode`, `user_instructions`, `truncation_policy`, `personality`, `turn_id`, `current_date`, `timezone`, `realtime_active`, `permission_profile`, `file_system_sandbox_policy`, `final_output_json_schema`, `workspace_roots`, `multi_agent_version`, `comp_hash`, `approvals_reviewer`, `multi_agent_mode`, `root_turn_id`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"turn_context","payload":{"cwd":"/workspace/example","approval_policy":"[redacted]","sandbox_policy":{"mode":"workspace-write","writable_roots":["[redacted]"],"network_access":false,"exclude_tmpdir_env_var":false,"exclude_slash_tmp":false},"model":"[redacted]","effort":"medium","summary":"[redacted]"}}
```

Fixture: [file-turn-context](../../internal/transcriptparse/codex/testdata/research/file-turn-context/manifest.md).

### `compacted` → compaction

Context replacement boundary. Retain summary, window lineage and replacement_history as replay context, not new human messages.

Fields: `message`, `replacement_history`, `window_number`, `first_window_id`, `previous_window_id`, `window_id`, `guardian_history`, `compaction_response_id`, `latest_token_usage_record`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"compacted","payload":{"message":"[redacted]","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"[redacted]"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"[redacted]"}],"phase":"final_answer"},{"type":"message","role":"user","content":[{"type":"input_text","text":"[redacted]"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"[redacted]"}],"phase":"final_answer"}]}}
```

Fixture: [file-compacted](../../internal/transcriptparse/codex/testdata/research/file-compacted/manifest.md).

### `world_state` → system

Full or incremental collaboration context snapshot. Preserve full/state and nested fields.

Fields: `full`, `state`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"world_state","payload":{"full":true,"state":{"agents_md":{"directory":"[redacted]","text":"[redacted]"},"apps_instructions":true,"environments":{"environments":{"local":{"cwd":"/workspace/example","status":"available","shell":"[redacted]"}},"current_date":"[redacted]","timezone":"[redacted]","filesystem":"[redacted]"},"plugins_instructions":true,"skills":{"includeInstructions":true}}}}
```

Fixture: [file-world-state](../../internal/transcriptparse/codex/testdata/research/file-world-state/manifest.md).

### `inter_agent_communication_metadata` → system

Metadata preceding an inter-agent response message; trigger_turn is causation, not proof of human input.

Fields: `trigger_turn`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"inter_agent_communication_metadata","payload":{"trigger_turn":true}}
```

Fixture: [file-inter-agent-communication-metadata](../../internal/transcriptparse/codex/testdata/research/file-inter-agent-communication-metadata/manifest.md).

### `token_usage_record` → usage

New response-, turn- and thread-grain usage snapshot. Preserve response_id/root_turn_id and cache_write_input_tokens; do not add all three grains.

Fields: `thread_id`, `turn_id`, `session_id`, `root_turn_id`, `response_id`, `usage`, `turn_token_usage`, `thread_token_usage`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"token_usage_record","payload":{"thread_id":"id-e4317253b0","turn_id":"id-3af80e7419","session_id":"[redacted]","root_turn_id":"[redacted]","response_id":"[redacted]","usage":{"input_tokens":220308,"cached_input_tokens":11136,"cache_write_input_tokens":0,"output_tokens":1180,"reasoning_output_tokens":838,"total_tokens":221488},"turn_token_usage":{"input_tokens":220308,"cached_input_tokens":11136,"cache_write_input_tokens":0,"output_tokens":1180,"reasoning_output_tokens":838,"total_tokens":221488},"thread_token_usage":{"input_tokens":220308,"cached_input_tokens":11136,"cache_write_input_tokens":0,"output_tokens":1180,"reasoning_output_tokens":838,"total_tokens":221488}}}
```

Fixture: [file-token-usage-record](../../internal/transcriptparse/codex/testdata/research/file-token-usage-record/manifest.md).

### `event_msg/item_completed` → nested item

New typed completion record. item uses PascalCase tags and snake_case fields; preserve envelope ordinal and per-item millisecond timing.

Fields: `type`, `thread_id`, `turn_id`, `item`, `started_at_ms`, `completed_at_ms`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","ordinal":9,"type":"event_msg","payload":{"type":"item_completed","thread_id":"id-6d22c54faa","turn_id":"id-b609c1de20","item":{"type":"UserMessage","id":"id-670e45b017","content":[{"type":"text","text":"[redacted]","text_elements":[]}]},"started_at_ms":1788671858782,"completed_at_ms":1788671858782}}
```

Fixture: [file-event-msg-item-completed](../../internal/transcriptparse/codex/testdata/research/file-event-msg-item-completed/manifest.md).

### `response_item/message` → message

Persisted model input/output message. Split typed content; classify role and injected context separately.

Fields: `type`, `role`, `content`, `phase`, `internal_chat_message_metadata_passthrough`, `id`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"[redacted]"}]}}
```

Fixture: [file-response-item-message](../../internal/transcriptparse/codex/testdata/research/file-response-item-message/manifest.md).

### `response_item/agent_message` → message

Inter-agent message using author/recipient, not a normal role field. Preserve endpoint identities and direction.

Fields: `type`, `author`, `recipient`, `content`, `internal_chat_message_metadata_passthrough`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"response_item","payload":{"type":"agent_message","author":"[redacted]","recipient":"[redacted]","content":[{"type":"input_text","text":"[redacted]"},{"type":"encrypted_content","encrypted_content":"[opaque ciphertext omitted]"}],"internal_chat_message_metadata_passthrough":{"turn_id":"id-2ef89832ca"}}}
```

Fixture: [file-response-item-agent-message](../../internal/transcriptparse/codex/testdata/research/file-response-item-agent-message/manifest.md).

### `response_item/reasoning` → reasoning

Summary/content or opaque encrypted reasoning. Empty visible text does not mean no reasoning.

CODEX-REASONING-1: the module assembles readable text from the ordered summary
and raw reasoning parts, using the same rule for file and wire items. Native
`summary_text` and `reasoning_text` blocks retain their whitespace. A summary
does not hide later raw content; encrypted content never becomes readable text.
Both `file-response-item-reasoning` and `schema-response-item-reasoning` assert
the visible summary, alongside a constructed mixed-parts regression.

Fields: `type`, `summary`, `content`, `encrypted_content`, `id`, `internal_chat_message_metadata_passthrough`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"[redacted]"}],"content":null,"encrypted_content":"[opaque ciphertext omitted]"}}
```

Fixture: [file-response-item-reasoning](../../internal/transcriptparse/codex/testdata/research/file-response-item-reasoning/manifest.md).

### `response_item/function_call` → tool_call

Model tool invocation; arguments can be a JSON string or object. call_id joins result; id is a different alias.

Fields: `type`, `name`, `arguments`, `call_id`, `namespace`, `id`, `internal_chat_message_metadata_passthrough`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\": [\"[redacted]\", \"[redacted]\", \"[redacted]\"], \"workdir\": \"/workspace/example\"}","call_id":"id-da02e37a2d"}}
```

Fixture: [file-response-item-function-call](../../internal/transcriptparse/codex/testdata/research/file-response-item-function-call/manifest.md).

### `response_item/custom_tool_call` → tool_call

Freeform tool invocation, often patch input or code orchestration; input need not be JSON.

Fields: `type`, `status`, `call_id`, `name`, `input`, `id`, `internal_chat_message_metadata_passthrough`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"id-ab345c9ae3","name":"apply_patch","input":"[redacted]"}}
```

Fixture: [file-response-item-custom-tool-call](../../internal/transcriptparse/codex/testdata/research/file-response-item-custom-tool-call/manifest.md).

### `response_item/local_shell_call` → command

Older shell invocation with action/command payload. Code-supported, not observed in this sample.

Fields: `type`, `id`, `call_id`, `text`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"response_item","payload":{"type":"local_shell_call","id":"item-example","call_id":"call-example","text":"Example text."}}
```

Fixture: [file-response-item-local-shell-call](../../internal/transcriptparse/codex/testdata/research/file-response-item-local-shell-call/manifest.md).

### `response_item/web_search_call` → tool_call

Native web action; action discriminates search/open/find. May have no call_id/id.

Fields: `type`, `status`, `action`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"response_item","payload":{"type":"web_search_call","status":"completed","action":{"type":"search","query":"[redacted]"}}}
```

Fixture: [file-response-item-web-search-call](../../internal/transcriptparse/codex/testdata/research/file-response-item-web-search-call/manifest.md).

### `response_item/tool_search_call` → tool_call

Deferred-tool discovery; execution and arguments describe the lookup.

Fields: `type`, `call_id`, `status`, `execution`, `arguments`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"response_item","payload":{"type":"tool_search_call","call_id":"id-5f4d2a7355","status":"completed","execution":"[redacted]","arguments":{"query":"[redacted]","limit":5}}}
```

Fixture: [file-response-item-tool-search-call](../../internal/transcriptparse/codex/testdata/research/file-response-item-tool-search-call/manifest.md).

### `response_item/function_call_output` → tool_result

Tool result can be text, encoded JSON, object or content array. Preserve typed output.

Fields: `type`, `call_id`, `output`, `internal_chat_message_metadata_passthrough`, `id`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"id-da02e37a2d","output":"{\"output\": \"[redacted]\", \"metadata\": {\"exit_code\": 0, \"duration_seconds\": 0.0}}"}}
```

Fixture: [file-response-item-function-call-output](../../internal/transcriptparse/codex/testdata/research/file-response-item-function-call-output/manifest.md).

### `response_item/custom_tool_call_output` → tool_result

Freeform tool result. Can report a running cell; a later wait is a separate call.

Fields: `type`, `call_id`, `output`, `internal_chat_message_metadata_passthrough`, `id`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"id-ab345c9ae3","output":"{\"output\": \"[redacted]\", \"metadata\": {\"exit_code\": 0, \"duration_seconds\": 0.0}}"}}
```

Fixture: [file-response-item-custom-tool-call-output](../../internal/transcriptparse/codex/testdata/research/file-response-item-custom-tool-call-output/manifest.md).

### `response_item/local_shell_call_output` → tool_result

Older shell result; code-supported, not observed in this sample.

Fields: `type`, `id`, `call_id`, `text`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"response_item","payload":{"type":"local_shell_call_output","id":"item-example","call_id":"call-example","text":"Example text."}}
```

Fixture: [file-response-item-local-shell-call-output](../../internal/transcriptparse/codex/testdata/research/file-response-item-local-shell-call-output/manifest.md).

### `response_item/tool_search_output` → tool_result

Tool discovery result stores definitions under tools, not output; current Nano text is empty for this shape.

Fields: `type`, `call_id`, `status`, `execution`, `tools`.

Scrubbed example: [input](../../internal/transcriptparse/codex/testdata/research/file-response-item-tool-search-output/input.jsonl). The array-heavy sample is linked to keep this reference readable.

Fixture: [file-response-item-tool-search-output](../../internal/transcriptparse/codex/testdata/research/file-response-item-tool-search-output/manifest.md).

### `response_item/plan` → plan

Code-supported native plan row; not observed in bounded corpus.

Fields: `type`, `id`, `call_id`, `text`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"response_item","payload":{"type":"plan","id":"item-example","call_id":"call-example","text":"Example text."}}
```

Fixture: [file-response-item-plan](../../internal/transcriptparse/codex/testdata/research/file-response-item-plan/manifest.md).

### `event_msg/user_message` → message

User-visible event serialization, often a twin of response_item/message; contains images/audio and text spans too.

Fields: `type`, `message`, `images`, `local_images`, `text_elements`, `audio`, `local_audio`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"[redacted]","images":[]}}
```

Fixture: [file-event-msg-user-message](../../internal/transcriptparse/codex/testdata/research/file-event-msg-user-message/manifest.md).

### `event_msg/agent_message` → message

Assistant event serialization; may carry phase and memory_citation. Often followed by response twin.

Fields: `type`, `message`, `phase`, `memory_citation`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"[redacted]"}}
```

Fixture: [file-event-msg-agent-message](../../internal/transcriptparse/codex/testdata/research/file-event-msg-agent-message/manifest.md).

### `event_msg/agent_reasoning` → reasoning

Reasoning event summary; often overlaps a response reasoning row. Do not merge by text alone.

Fields: `type`, `text`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"agent_reasoning","text":"[redacted]"}}
```

Fixture: [file-event-msg-agent-reasoning](../../internal/transcriptparse/codex/testdata/research/file-event-msg-agent-reasoning/manifest.md).

### `event_msg/task_started` → turn

Native task boundary; turn_id plus started_at where available; model context limit is not usage.

Fields: `type`, `turn_id`, `model_context_window`, `collaboration_mode_kind`, `started_at`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"id-f621ac34db","model_context_window":258400,"collaboration_mode_kind":"[redacted]"}}
```

Fixture: [file-event-msg-task-started](../../internal/transcriptparse/codex/testdata/research/file-event-msg-task-started/manifest.md).

### `event_msg/task_complete` → turn

Native terminal event. Bind its turn_id and preserve started_at/completed_at/duration_ms and final text evidence.

Fields: `type`, `turn_id`, `last_agent_message`, `completed_at`, `duration_ms`, `time_to_first_token_ms`, `started_at`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"id-f621ac34db","last_agent_message":"[redacted]"}}
```

Fixture: [file-event-msg-task-complete](../../internal/transcriptparse/codex/testdata/research/file-event-msg-task-complete/manifest.md).

### `event_msg/turn_aborted` → turn

Interruption with reason, turn_id, timing where available. Does not imply failure of every prior item.

Fields: `type`, `reason`, `turn_id`, `completed_at`, `duration_ms`, `started_at`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted"}}
```

Fixture: [file-event-msg-turn-aborted](../../internal/transcriptparse/codex/testdata/research/file-event-msg-turn-aborted/manifest.md).

### `event_msg/context_compacted` → compaction

Compaction lifecycle notice, possibly paired with compacted replacement data. Link rather than count two replacements.

Fields: `type`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"context_compacted"}}
```

Fixture: [file-event-msg-context-compacted](../../internal/transcriptparse/codex/testdata/research/file-event-msg-context-compacted/manifest.md).

### `event_msg/thread_settings_applied` → system

Applied settings notice; explicit service tier overrides context fallback on the same open turn.

Fields: `type`, `thread_settings`, `thread_id`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"model":"[redacted]","model_provider_id":"[redacted]","service_tier":"default","approval_policy":"[redacted]","approvals_reviewer":"[redacted]","permission_profile":{"type":"managed","file_system":{"type":"restricted","entries":[{"path":{"type":"special","value":{"kind":"root"}},"access":"[redacted]"},{"path":{"type":"path","path":"/workspace/example"},"access":"[redacted]"},{"path":{"type":"special","value":{"kind":"slash_tmp"}},"access":"[redacted]"},{"path":{"type":"special","value":{"kind":"tmpdir"}},"access":"[redacted]"}]},"network":"[redacted]"},"cwd":"/workspace/example","reasoning_effort":"high","personality":"[redacted]","collaboration_mode":{"mode":"default","settings":{"model":"[redacted]","reasoning_effort":"high","developer_instructions":"[redacted]"}}}}}
```

Fixture: [file-event-msg-thread-settings-applied](../../internal/transcriptparse/codex/testdata/research/file-event-msg-thread-settings-applied/manifest.md).

### `event_msg/sub_agent_activity` → system

Child started/interacted/interrupted. event_id and occurred_at_ms are native evidence worth preserving.

Fields: `type`, `event_id`, `occurred_at_ms`, `agent_thread_id`, `agent_path`, `kind`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"sub_agent_activity","event_id":"[redacted]","occurred_at_ms":1783654118233,"agent_thread_id":"id-a7e225dea8","agent_path":"/workspace/example","kind":"started"}}
```

Fixture: [file-event-msg-sub-agent-activity](../../internal/transcriptparse/codex/testdata/research/file-event-msg-sub-agent-activity/manifest.md).

### `event_msg/guardian_assessment` → interaction

Guardian assessment started/completed; action, risk, authorization, rationale and decision_source remain typed.

Fields: `type`, `id`, `target_item_id`, `turn_id`, `status`, `action`, `risk_level`, `user_authorization`, `rationale`, `decision_source`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"guardian_assessment","id":"id-fd91057917","target_item_id":"[redacted]","turn_id":"id-01c89e6541","status":"in_progress","action":{"type":"command","source":"[redacted]","command":"[redacted]","cwd":"/workspace/example"}}}
```

Fixture: [file-event-msg-guardian-assessment](../../internal/transcriptparse/codex/testdata/research/file-event-msg-guardian-assessment/manifest.md).

### `event_msg/thread_goal_updated` → system

Goal snapshot; turn goal usage is a distinct grain from model-token usage.

Fields: `type`, `threadId`, `goal`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"thread_goal_updated","threadId":"id-51ae1106ff","goal":{"threadId":"id-51ae1106ff","objective":"[redacted]","status":"paused","tokensUsed":4353565,"timeUsedSeconds":48108,"createdAt":1785542762,"updatedAt":1785567236}}}
```

Fixture: [file-event-msg-thread-goal-updated](../../internal/transcriptparse/codex/testdata/research/file-event-msg-thread-goal-updated/manifest.md).

### `event_msg/thread_name_updated` → system

Thread name mutation evidence; not user conversation.

Fields: `type`, `thread_id`, `thread_name`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"thread_name_updated","thread_id":"id-3362dc196d","thread_name":"[redacted]"}}
```

Fixture: [file-event-msg-thread-name-updated](../../internal/transcriptparse/codex/testdata/research/file-event-msg-thread-name-updated/manifest.md).

### `event_msg/web_search_end` → tool_result

Web output: results is a typed list. Current Nano reads result/output/message, missing results.

Fields: `type`, `call_id`, `query`, `action`, `results`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"web_search_end","call_id":"id-2c315f2072","query":"[redacted]","action":{"type":"search","query":"[redacted]","queries":["[redacted]","[redacted]","[redacted]","[redacted]"]}}}
```

Fixture: [file-event-msg-web-search-end](../../internal/transcriptparse/codex/testdata/research/file-event-msg-web-search-end/manifest.md).

### `event_msg/mcp_tool_call_end` → tool_result

MCP invocation, structured result or error and duration. Preserve content arrays and call relation.

Fields: `type`, `call_id`, `invocation`, `duration`, `result`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"mcp_tool_call_end","call_id":"id-480590b559","invocation":{"server":"[redacted]","tool":"[redacted]","arguments":{"query":"[redacted]","limit":5}},"duration":{"secs":0,"nanos":370005786},"result":{"Ok":{"content":[{"type":"text","text":"[redacted]"}]}}}}
```

Fixture: [file-event-msg-mcp-tool-call-end](../../internal/transcriptparse/codex/testdata/research/file-event-msg-mcp-tool-call-end/manifest.md).

### `event_msg/collab_waiting_end` → system

Wait completion; statuses/agent_statuses are per-child state, not parent completion.

Fields: `type`, `sender_thread_id`, `call_id`, `statuses`, `agent_statuses`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"collab_waiting_end","sender_thread_id":"[redacted]","call_id":"id-6f52f78eba","statuses":{}}}
```

Fixture: [file-event-msg-collab-waiting-end](../../internal/transcriptparse/codex/testdata/research/file-event-msg-collab-waiting-end/manifest.md).

### `event_msg/collab_agent_spawn_end` → system

Spawn completion; new thread identity/model/prompt are child lineage, not a new parent user message.

Fields: `type`, `call_id`, `sender_thread_id`, `new_thread_id`, `new_agent_nickname`, `new_agent_role`, `prompt`, `model`, `reasoning_effort`, `status`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"collab_agent_spawn_end","call_id":"id-768846051b","sender_thread_id":"[redacted]","new_thread_id":"[redacted]","new_agent_nickname":"[redacted]","new_agent_role":"[redacted]","prompt":"[redacted]","model":"[redacted]","reasoning_effort":"xhigh","status":"pending_init"}}
```

Fixture: [file-event-msg-collab-agent-spawn-end](../../internal/transcriptparse/codex/testdata/research/file-event-msg-collab-agent-spawn-end/manifest.md).

### `event_msg/collab_close_end` → system

Child close outcome. Parent turn may continue.

Fields: `type`, `call_id`, `sender_thread_id`, `receiver_thread_id`, `receiver_agent_nickname`, `receiver_agent_role`, `status`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"collab_close_end","call_id":"id-7c24b452cf","sender_thread_id":"[redacted]","receiver_thread_id":"[redacted]","receiver_agent_nickname":"[redacted]","receiver_agent_role":"[redacted]","status":{"completed":"[redacted]"}}}
```

Fixture: [file-event-msg-collab-close-end](../../internal/transcriptparse/codex/testdata/research/file-event-msg-collab-close-end/manifest.md).

### `event_msg/collab_agent_interaction_end` → system

Child interaction end. Code-supported, unobserved in bounded scan.

Fields: `type`, `id`, `call_id`, `text`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"collab_agent_interaction_end","id":"item-example","call_id":"call-example","text":"Example text."}}
```

Fixture: [file-event-msg-collab-agent-interaction-end](../../internal/transcriptparse/codex/testdata/research/file-event-msg-collab-agent-interaction-end/manifest.md).

### `event_msg/error` → system

Provider error notice; not by itself a failed turn. codex_error_info preserves classification.

Fields: `type`, `message`, `codex_error_info`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"error","message":"[redacted]","codex_error_info":"[redacted]"}}
```

Fixture: [file-event-msg-error](../../internal/transcriptparse/codex/testdata/research/file-event-msg-error/manifest.md).

### `event_msg/token_count` → usage

Usage and rate-limit snapshot; info may be null. last is model-call usage, total is cumulative. Repeated snapshots exist.

Fields: `type`, `info`, `rate_limits`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":{"used_percent":0.0,"window_minutes":300,"resets_at":1761873011},"secondary":{"used_percent":0.0,"window_minutes":10080,"resets_at":1762459811}}}}
```

Fixture: [file-event-msg-token-count](../../internal/transcriptparse/codex/testdata/research/file-event-msg-token-count/manifest.md).

### `event_msg/exec_command_begin` → command

Command execution start, not sampled. File parser intentionally discards semantic event path.

Fields: `type`, `id`, `call_id`, `text`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"exec_command_begin","id":"item-example","call_id":"call-example","text":"Example text."}}
```

Fixture: [file-event-msg-exec-command-begin](../../internal/transcriptparse/codex/testdata/research/file-event-msg-exec-command-begin/manifest.md).

### `event_msg/exec_command_end` → command

Command terminal: stdout/stderr/status/exit_code/duration/parsed_cmd. May correspond to a response output or later poll.

Fields: `type`, `call_id`, `process_id`, `turn_id`, `command`, `cwd`, `parsed_cmd`, `source`, `stdout`, `stderr`, `aggregated_output`, `exit_code`, `duration`, `formatted_output`, `status`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"exec_command_end","call_id":"id-1194912ba9","process_id":"[redacted]","turn_id":"id-2279c2c61c","command":["[redacted]","[redacted]","[redacted]"],"cwd":"/workspace/example","parsed_cmd":[{"type":"list_files","cmd":"[redacted]","path":"/workspace/example"}],"source":"[redacted]","stdout":"[redacted]","stderr":"[redacted]","aggregated_output":"[redacted]","exit_code":2,"duration":{"secs":0,"nanos":14261},"formatted_output":"[redacted]","status":"failed"}}
```

Fixture: [file-event-msg-exec-command-end](../../internal/transcriptparse/codex/testdata/research/file-event-msg-exec-command-end/manifest.md).

### `event_msg/patch_apply_begin` → file_change

Patch start, not sampled. Do not assume a duplicate without evidence.

Fields: `type`, `id`, `call_id`, `text`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"patch_apply_begin","id":"item-example","call_id":"call-example","text":"Example text."}}
```

Fixture: [file-event-msg-patch-apply-begin](../../internal/transcriptparse/codex/testdata/research/file-event-msg-patch-apply-begin/manifest.md).

### `event_msg/patch_apply_end` → file_change

Patch terminal: changes map, success/status/stdout/stderr. Some full files have no same-call response output.

Fields: `type`, `call_id`, `turn_id`, `stdout`, `stderr`, `success`, `changes`, `status`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"id-4a31b5c5d3","turn_id":"id-2279c2c61c","stdout":"[redacted]","stderr":"[redacted]","success":true,"changes":{"/workspace/file-86885a84":{"type":"add","content":"[redacted]"}},"status":"completed"}}
```

Fixture: [file-event-msg-patch-apply-end](../../internal/transcriptparse/codex/testdata/research/file-event-msg-patch-apply-end/manifest.md).

### `future_native_row` → unknown

Repository fixture for a future top-level record. Preserve unknown evidence.

Fields: .

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"future_native_row","payload":{}}
```

Fixture: [file-future-native-row](../../internal/transcriptparse/codex/testdata/research/file-future-native-row/manifest.md).

### `future_private_row` → unknown

Repository privacy fixture. Preserve a bounded inspectable unknown record, never private text in summaries.

Fields: `raw`.

```json
{"timestamp":"2026-01-01T00:00:00.000Z","type":"future_private_row","payload":{"raw":"[redacted]"}}
```

Fixture: [file-future-private-row](../../internal/transcriptparse/codex/testdata/research/file-future-private-row/manifest.md).

## New completion payloads and usage

A second read of the complete-file subset found seven `item_completed.item.type` values, all under session headers 0.153.3. This later read includes growing files, so it is a separate denominator.

| Tag | Later complete-file count | Fields |
|---|---:|---|
| `UserMessage` | 94 | `type`, `id`, `content` |
| `Reasoning` | 672 | `type`, `id`, `summary_text`, `raw_content` |
| `AgentMessage` | 221 | `type`, `id`, `content`, `phase` |
| `CommandExecution` | 580 | `type`, `id`, `process_id`, `command`, `cwd`, `parsed_cmd`, `source`, `status`, `stdout`, `stderr`, `aggregated_output`, `exit_code`, `duration`, `formatted_output` |
| `Extension` | 41 | `type`, `kind`, `id`, `query`, `action`, `results` |
| `ContextCompaction` | 2 | `type`, `id` |
| `FileChange` | 30 | `type`, `id`, `changes`, `status`, `stdout`, `stderr` |

`Extension` currently contains `kind: "web.search"`, action, query and results. It is not the live schema's `webSearch` object. `AgentMessage.content` uses `Text` parts; `Reasoning` uses summary_text/raw_content. Capture fixtures preserve each tag. Root ordinal and envelope started_at_ms/completed_at_ms are independent of the old millisecond timestamp string.

`token_usage_record` has response-local `usage`, turn cumulative `turn_token_usage`, and thread cumulative `thread_token_usage`. It includes thread_id, turn_id, session_id, root_turn_id, response_id. Each usage object includes input/cached-input/cache-write-input/output/reasoning-output/total. Do not add the three grains. The primary sample had 2,121 such records; the later complete-file subset had 666 under 0.153.3 headers. Headers identify a session's version, not necessarily every later writer after resume.

## Odd cases and current limits

| Case | Evidence / interpretation | Fixture |
|---|---|---|
| Duplicates and split serializations | Message twins can disagree by content-block newline joins. Nano dedupes adjacent equal-text messages and adjacent user twins only. | [edge-adjacent-message-twins](../../internal/transcriptparse/codex/testdata/research/edge-adjacent-message-twins/manifest.md) |
| Repeated real prose | Never globally dedupe by text. Nonadjacent twins without identity remain ambiguous. | [edge-nonadjacent-twins-and-real-repeat](../../internal/transcriptparse/codex/testdata/research/edge-nonadjacent-twins-and-real-repeat/manifest.md) |
| Injected blocks | User role alone does not prove human authorship. Preserve block origin when stripping display context. | [edge-injected-blocks-mixed-user](../../internal/transcriptparse/codex/testdata/research/edge-injected-blocks-mixed-user/manifest.md) |
| Task notifications | Sample markers include subagent_notification (28), task-notification (2), turn_aborted (156), skill (19), recommended_plugins (49), and from-footers. Counts are occurrences inside text and can include quotes. | [edge-task-notification-and-abort-wrapper](../../internal/transcriptparse/codex/testdata/research/edge-task-notification-and-abort-wrapper/manifest.md) |
| Guardian and programmatic files | Complete-file subset: 17 guardian, 6 structured child, 4 exec, 50 vscode, 34 cli session headers. Guardian review has source.subagent.other=guardian; codex_exec uses ordinary rollout records. | [capture-guardian-meta](../../internal/transcriptparse/codex/testdata/research/capture-guardian-meta/manifest.md) |
| Inter-agent communication | author/recipient plus trigger_turn metadata; current assistant default and sidechain flag lose direction. | [capture-agent-message](../../internal/transcriptparse/codex/testdata/research/capture-agent-message/manifest.md) |
| Compaction/replay | 199 compacted and 196 context_compacted primary rows. Replacement history and guardian_history are context snapshots, not fresh turns. Window IDs and latest_token_usage_record carry lineage/checkpoints. | [edge-compaction-replacement-replay](../../internal/transcriptparse/codex/testdata/research/edge-compaction-replacement-replay/manifest.md) |
| Command/patch twins | 1,924 exec/patch end records in the later full-file subset; 280 lacked same-call response output, all patch events in this check. Outer tool results may retain content under another ID. | [capture-patch-without-direct-twin](../../internal/transcriptparse/codex/testdata/research/capture-patch-without-direct-twin/manifest.md) |
| Usage repeats | 1,771 consecutive identical non-null usage-info observations in the primary segmented scan; 1,274 in later full-file subset. Rate-limit refresh can repeat usage info. Naive summing inflates it. | [capture-repeated-usage](../../internal/transcriptparse/codex/testdata/research/capture-repeated-usage/manifest.md) |
| Usage reset | Negative cumulative delta is diagnostic, never negative usage. Current guard applies only when last is missing. | [edge-usage-repeat-and-reset](../../internal/transcriptparse/codex/testdata/research/edge-usage-repeat-and-reset/manifest.md) |
| Partial line/truncation | Reader commits newline-terminated records only. Source EOF alone does not establish terminal/truncated execution. | [edge-partial-final-line](../../internal/transcriptparse/codex/testdata/research/edge-partial-final-line/manifest.md) |
| Malformed/oversize | Malformed full records receive evidence items; oversized lines currently receive only diagnostics. Test recipe avoids a 32 MiB fixture. | [edge-oversize-body-budget](../../internal/transcriptparse/codex/testdata/research/edge-oversize-body-budget/manifest.md) |
| Out-of-order terminal | Constructed stress case. Respect native turn_id; envelope timestamp sort cannot safely repair grouping. | [edge-late-terminal-native-turn](../../internal/transcriptparse/codex/testdata/research/edge-late-terminal-native-turn/manifest.md) |
| Rewrites/repeated headers | Observed repeated headers; inode/shrink handling is code evidence. Physical rewrite was not performed. | [edge-rewrite-and-repeated-header](../../internal/transcriptparse/codex/testdata/research/edge-rewrite-and-repeated-header/manifest.md) |
| Typed media/opaque reasoning | Old sampled content had input_text/output_text/encrypted_content and summary_text; newer PascalCase contents are additional shapes. Nontext is not empty text. | [edge-image-only-and-unknown-content](../../internal/transcriptparse/codex/testdata/research/edge-image-only-and-unknown-content/manifest.md) |
| Running tools and polls | A completed tool result can name a still-running cell. A later wait has another call ID. | [edge-freeform-exec-and-later-wait](../../internal/transcriptparse/codex/testdata/research/edge-freeform-exec-and-later-wait/manifest.md) |

Physical source replacement resets generation/checkpoint on inode change or size shrink ([source storage](../../internal/history/store_write.go)). The module assigns source-local keys; history resolves native identities and preserves existing row positions at its [conversion boundary](../../internal/history/transcript_parsed_item.go). The original research did not perform a same-size rewrite or an archived-tree census.

## Sidecars and lineage

SQLite was opened with `mode=ro` and `query_only=ON`; no SQL wrote native state. Its table counts differ from the session-tree census because it also catalogs sessions outside the sampled roots and retained metadata; this research did not reconcile every row to a file.

The database had 2403 threads and 203 edges at read time. Source groups: `{"cli": 209, "exec": 786, "vscode": 926, "guardian": 279, "structured_subagent": 203}`. Structured source strings are summarized by category, never copied with real paths or names.

`threads` fields: `id` (TEXT), `rollout_path` (TEXT), `created_at` (INTEGER), `updated_at` (INTEGER), `source` (TEXT), `model_provider` (TEXT), `cwd` (TEXT), `title` (TEXT), `sandbox_policy` (TEXT), `approval_mode` (TEXT), `tokens_used` (INTEGER), `has_user_event` (INTEGER), `archived` (INTEGER), `archived_at` (INTEGER), `git_sha` (TEXT), `git_branch` (TEXT), `git_origin_url` (TEXT), `cli_version` (TEXT), `first_user_message` (TEXT), `agent_nickname` (TEXT), `agent_role` (TEXT), `memory_mode` (TEXT), `model` (TEXT), `reasoning_effort` (TEXT), `agent_path` (TEXT), `created_at_ms` (INTEGER), `updated_at_ms` (INTEGER), `thread_source` (TEXT), `preview` (TEXT), `recency_at` (INTEGER), `recency_at_ms` (INTEGER), `history_mode` (TEXT), `name` (TEXT), `is_pinned` (INTEGER), `thread_section_id` (TEXT), `section_position` (INTEGER), `section_entered_at_ms` (INTEGER), `project_id` (TEXT).

`thread_spawn_edges` fields: `parent_thread_id` (TEXT), `child_thread_id` (TEXT), `status` (TEXT).

`threads.created_at/updated_at` are separate from their `_ms` companions; retain field-specific precision. `tokens_used` is a thread metadata total, not a per-turn usage event. `thread_spawn_edges` gives parent/child/status; it does not create parent conversation rows. Header source may lack the parent ID for a guardian. Fork lineage can come from forked_from_id without a source.type=fork marker; current metadata detection is source-shape-dependent.

`session_index.jsonl`: all 34 rows inspected; id/thread_name/updated_at. Treat as thread-name/index evidence, not conversation. `history.jsonl`: first 500 rows inspected; session_id/ts/text. Its ts is an integer epoch value; it is a prompt-history sidecar, not the complete response stream. Neither replaces rollout parsing. Fixtures exist for both and for SQLite lineage.

## Evidence limits

High confidence in observed tags, exact schema inventory, and cited switch behavior. Medium confidence in cross-record equivalence and historical completeness. Sampling, live-file growth, missing archived sessions, encrypted content, version drift and schema-only features prevent a universal completeness claim. No daemon connection or production mutation occurred.

## Raw ResponseItem schema union

The client schema exposes another native union through thread/inject_items. It has 16 alternatives, listed below. These describe raw model-history items, not the live ThreadItem union. Five tags were absent from the bounded disk sample and are also absent from Nano's response-item switch: image_generation_call, compaction, compaction_trigger, context_compaction and other. This is schema-only coverage, not evidence of on-disk emission.

| Tag | Fields (* required) | Primary file count | Standard kind | Constructed example |
|---|---|---:|---|---|
| `message` | `content`*, `id`, `internal_chat_message_metadata_passthrough`, `phase`, `role`*, `type`* | 16232 | message | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-message/input.jsonl) |
| `agent_message` | `author`*, `content`*, `id`, `internal_chat_message_metadata_passthrough`, `recipient`*, `type`* | 103 | message | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-agent-message/input.jsonl) |
| `reasoning` | `content`, `encrypted_content`, `id`, `internal_chat_message_metadata_passthrough`, `summary`*, `type`* | 24044 | reasoning | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-reasoning/input.jsonl) |
| `local_shell_call` | `action`*, `call_id`, `id`, `internal_chat_message_metadata_passthrough`, `status`*, `type`* | 0 | command | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-local-shell-call/input.jsonl) |
| `function_call` | `arguments`*, `call_id`*, `id`, `internal_chat_message_metadata_passthrough`, `name`*, `namespace`, `type`* | 17651 | tool_call | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-function-call/input.jsonl) |
| `tool_search_call` | `arguments`*, `call_id`, `execution`*, `id`, `internal_chat_message_metadata_passthrough`, `status`, `type`* | 6 | tool_call | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-tool-search-call/input.jsonl) |
| `function_call_output` | `call_id`*, `id`, `internal_chat_message_metadata_passthrough`, `output`*, `type`* | 17650 | tool_result | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-function-call-output/input.jsonl) |
| `custom_tool_call` | `call_id`*, `id`, `input`*, `internal_chat_message_metadata_passthrough`, `name`*, `status`, `type`* | 12904 | tool_call | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-custom-tool-call/input.jsonl) |
| `custom_tool_call_output` | `call_id`*, `id`, `internal_chat_message_metadata_passthrough`, `name`, `output`*, `type`* | 12898 | tool_result | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-custom-tool-call-output/input.jsonl) |
| `tool_search_output` | `call_id`, `execution`*, `id`, `internal_chat_message_metadata_passthrough`, `status`*, `tools`*, `type`* | 6 | tool_result | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-tool-search-output/input.jsonl) |
| `web_search_call` | `action`, `id`, `internal_chat_message_metadata_passthrough`, `status`, `type`* | 92 | tool_call | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-web-search-call/input.jsonl) |
| `image_generation_call` | `id`, `internal_chat_message_metadata_passthrough`, `result`*, `revised_prompt`, `status`*, `type`* | 0 | tool_call | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-image-generation-call/input.jsonl) |
| `compaction` | `encrypted_content`*, `id`, `internal_chat_message_metadata_passthrough`, `type`* | 0 | compaction | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-compaction/input.jsonl) |
| `compaction_trigger` | `type`* | 0 | compaction | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-compaction-trigger/input.jsonl) |
| `context_compaction` | `encrypted_content`, `id`, `internal_chat_message_metadata_passthrough`, `type`* | 0 | compaction | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-context-compaction/input.jsonl) |
| `other` | `type`* | 0 | unknown | [example](../../internal/transcriptparse/codex/testdata/research/schema-response-item-other/input.jsonl) |

LocalShellAction is an exec object with required command argv, optional env/timeout_ms/user/working_directory. The [shared content decoder](../../internal/transcriptparse/codex/content.go) retains the native action and derives the command display from it. Code-supported plan/local_shell_call_output are not members of this current raw union; treat them as compatibility paths of unverified vintage.

Raw ContentItem admits input_text, input_image and output_text. FunctionCallOutputContentItem admits input_text, input_image and encrypted_content. Its media form is schema evidence beyond the text-heavy corpus sample. Unknown or encrypted content must remain typed and inspectable without becoming ordinary text.

## Reader identity refinement (2026-09-09)

A file `session_meta` ID identifies its own observation and supplies the default
native thread for later rows in that source incarnation and caller connection.
An empty metadata ID cannot erase a known default. Explicit caller scope on
later rows takes precedence over that default. A second connection keeps its
own identity state. `TestCodexFileSessionMemoryStaysOnItsConnection` covers
these cases through serialized checkpoints.

## Captured completed-item support (2026-09-09)

`TestCodexCapturedCompletedItemVariants` exercises all seven captured
`item_completed` variants. The shared item decoder accepts the captured
Pascal-case union as well as the wire union. File assistant content uses a
`Text` block, reasoning uses `summary_text` and `raw_content`, and command
execution can carry argv instead of a string. Those fields now retain their
content and parts. The known `Extension` kind `web.search` retains structured
results as a tool facet. Unknown extension kinds stay inspectable with an
`unknown_extension` diagnostic. A completed item never completes its turn.

## Auxiliary files and unknown records (2026-09-09)

The history and session-index JSONL shapes parse as scoped auxiliary observations.
A history entry's `ts` keeps Unix-second meaning; a session-index `updated_at`
keeps its native timestamp string. These files can interleave threads, so each
row names its own native session without setting a default for later records.
Neither auxiliary shape creates a user prompt or turn. Unknown record types
retain the full bounded envelope, including fields outside `payload`, with
content-free diagnostics.

## Repeated and decreasing usage (2026-09-09)

Repeated last/total tuples retain separate observations and one selected
response contribution. `repeated_usage_snapshot` makes that interpretation
visible. Native counter objects remain available, and outer
`model_context_window` supplies capacity separately from token contributions.
A decrease in comparable cumulative counters produces `usage_counter_decrease`
within the same source, connection and native thread. The reader does not
subtract it or infer a reset epoch: a decrease alone could also be stale
evidence. Explicit reset/baseline evidence is still required to establish
an epoch. This is a named refinement of the research draft's reset diagnostic.

The `CODEX-CONTENT-2` fixtures keep the readable body of native system
notifications and abort wrappers. Image-only native user messages remain user
content, and unknown media keeps its payload with `unknown_content_part`.
`CODEX-COMPACTION-1` retains the actual compaction summary; replacement history
is bounded detail, not a new conversation replay. Encrypted compaction bodies
and image-generation result strings gain no invented readable text or encoding.

`CODEX-ACTION-1` retains `exec_command_begin/end` and `patch_apply_begin/end`
as execution items with arguments, changes and outputs. A scoped native call ID
connects begin/end observations. When the generated invocation is known,
`progress_of` links the execution item to it; later response output still uses
`result_of` on that generated invocation. Execution cannot complete the parent.
`CODEX-GENERATION-1` keeps completed function/custom/local-shell call generation
separate from execution. A known command's native object result can provide an
exit-code failure. A file-only question retains invocation and answer facets.

`CODEX-METADATA-3` keeps native world state, coordination metadata and thread
settings, name and goal changes as scoped observations. They do not invent
content or conversation turns. `CODEX-GUARDIAN-1` preserves the guardian review
as an interaction with its own native review alias and action detail. The
review ID does not become the target item's identity, and review status does
not settle target execution or parent completion.

The original research expectations are retained. The exact display-view changes
are named in `internal/transcriptparse/codex/testdata/item-views.json`.
`CODEX-NOTICE-1` keeps readable event labels for coordination records that have
no native prose. `CODEX-COMPACTION-2` keeps a summary and an unaddressed adjacent
compaction marker separate because adjacency does not establish one operation.
The research size recipe runs in memory and verifies that an oversized native
row produces a diagnostic, advances the source position and allows the next
valid row to be read after checkpoint restoration.

`CODEX-CALL-1` keeps a reused native call ID ambiguous when two
distinct invocations occur in one turn. Later results retain their content
with diagnostics; they cannot choose the last invocation merely by position.

`CODEX-ACTION-2` keeps a new begin after terminal evidence separate when the
same scoped call ID supplies no replay or new-invocation proof. Later records
remain ambiguous instead of reopening the earlier execution. `CODEX-TWIN-2`
keeps multiple possible serialization twins separate and diagnoses the missing
identity proof. Both rules have constructed native-row regression fixtures.

Reader v35 extends the user-message twin rule to `event_msg/item_completed`
`UserMessage` records paired with `response_item/message` in the same native
turn. A unique equal-text counterpart from the other serialization shares one
ordered item; both observations and both native item-ID aliases survive. The
pair is consumed once, so later genuine prompts remain separate, including
identical prompts in later turns. Multiple possible counterparts stay separate.
Multimodal completed-user records are not joined by caption alone. The rule
works across parser checkpoints and either record order. The opt-in regression
`TestCodexOperatorUserTwins` reads the operator-selected file without modifying it.

Reader v36 adds three pairs to the same rule, each only inside a
native turn and only for a unique equal-content counterpart of the same item
type: a completed `AgentMessage` with its `response_item/message`, a legacy
`event_msg/agent_reasoning` with its `response_item/reasoning`, and a completed
`Reasoning` with its `response_item/reasoning`. A growing `Reasoning` that Codex
completes more than once under one id is matched on its newest summary. The
joined row keeps its first native id, and the other copy's id becomes an alias:
history binds an item to one exact native id. Codex writes the completed event
first, so that is normally the id the live wire uses. Several summary events against one
combined response stay separate, as before. On rebuild, the retired copy's
public id is aliased to the surviving row when exactly one new item holds its
source line with unchanged bytes from the same file. Twin candidates are keyed
by item type as well as text, so an equal text of another type never makes a
pair ambiguous.

Reader v37 closes a twin candidate of an `AgentMessage` or `Reasoning` once a
newer item of the same type appears in the turn: Codex writes an item's two
copies next to each other, so an older lone copy with equal text is a different
item. Under v36, such a copy joined a later item with the same common text
(for example "Waiting for operation completion"), merging two items in a long
turn. A closed candidate still counts toward ambiguity, so neither copy of a
later equal-text pair is guessed and both stay as rows.
