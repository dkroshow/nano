# Grok Build: historical native files

This page adapts the September 2026 native-format study. Its sample counts,
code comparisons and probe results describe that dated inspection. References
to the “current parser” below mean the parser inspected then. They do not
certify the new readers' behavior.

Use the [model contract](model.md) for normalized meanings and the
[Grok reader notes](../../internal/transcriptparse/grok/README.md)
for implemented coverage and remaining differences. The [dump command](command.md)
lets you inspect native records. Proposed standard fields in the study are
research guidance; the accepted model uses `Item` and `Turn`, separate capture,
outcome and finality, and no confidence score.

The main transcript is `updates.jsonl`. It contains ACP update envelopes, with a file timestamp and a different extension-method spelling from the live fixtures. It is not a recording of every JSON-RPC frame. `summary.json` is a rewritten metadata snapshot. Native model history, execution telemetry, compaction, plans, and lineage also live in sibling files. Nano reads only a subset.

This reference records repository state `db88af8b44ed712a99283bf5fd147ea072700b00`, inspected on 2026-09-07. Provider versions are separate: repository compatibility evidence names 0.2.118 and 1.0.3; sampled native records do not carry a dependable CLI version.

## Evidence and bounds

A directory inventory found 137 session directories under `~/.grok/sessions/<encoded-cwd>/<native-session-id>/`. All 137 currently have both summary and updates files. Their updates total about 245.5 MB. The parser also supports older summary-only sessions; none occurred in this inventory.

The content scan selected 39 directories by a stride through sorted directory names, plus every directory with subagents, compaction, or hunk records. It read at most 4 MiB or 15,000 complete rows per updates file. Eight files hit the byte cap. The sample contains **17,169 rows / 67,900,279 bytes**. It is a prefix-biased structural sample, not a random estimate of global frequencies. No matching result means “not observed in this sample.” It does not establish absence. The corpus was live and no atomic snapshot was taken.

The [research archive](evidence/README.md) retains these study artifacts: `grok/corpus-counts.json` gives counts and field/type paths. `grok/supplement-counts.json` gives usage and hook details. `grok/scan_corpus.py` records the scan method. It never opens authentication files. Auxiliary JSONL sampling stops after 40 rows per selected file; large auxiliary files are skipped. The generated examples replace private values and limit arrays to three members.

## Files and write behavior

| File | Inventory count | Shape, timing, and Nano treatment |
|---|---:|---|
| `updates.jsonl` | 137 | Append-oriented update evidence. One JSON object per newline. Provider event metadata identifies session, event, and sometimes prompt. Discovery makes this the canonical source whenever present. |
| `summary.json` | 137 | Whole-document metadata snapshot, refreshed during activity and housekeeping. Nano reparses it from offset zero. Reconnect timestamps are not reliable conversation recency. |
| `subagents/<child>/meta.json` | 3 parent directories; 13 sidecars sampled | Parent-owned relationship snapshot. Child sessions have their own summary/updates files and can use a different encoded working directory. Nano reads only parent and child native ids for discovery. |
| `chat_history.jsonl` | 137 | Model-facing history, not ACP display history. User/system content, assistant tool-call arrays, tool results, reasoning summaries and encrypted content, backend tool calls. Nano ignores it. The snapshot does not prove an append-only contract across compaction or rewind. |
| `events.jsonl` | 137 | Execution telemetry: loops, phases, turns, first token, tools and permissions. Nano ignores it as a transcript source. |
| `rewind_points.jsonl` | 128 | Prompt-indexed before/after file snapshots with timestamps and nullable content. Nano ignores it. It is not evidence that a file change actually ran. |
| `compaction/` | 5 | Markdown artifacts, including small and large documents. No native compaction update discriminant appeared in sampled ACP rows. Contents were not copied. |
| `compaction_requests/` | 5 | Versioned JSON request snapshots with trigger, model, source model history, tools and prompt context. Six bounded specimens read. These are file-only evidence in this audit. |
| `compaction_checkpoints/` | 5 | Versioned snapshots of compacted model history, checkpoint id, prompt index, creation time, original user info and reread paths. Six specimens read. |
| `plan.json`, `plan.md`, `plan_mode.json` | 5 / 12 / 17 | Todo-map snapshot, plan text, and approval/mode state. Nano does not read these for historical plan cards. |
| `hunk_records.jsonl` | 1 | Hunk ids, file ranges, added/removed counts, author/session ids, prompt index, source and event type. One sampled row. Not projected. |
| `prompt_context.json`, `system_prompt.txt` | 137 each | Resolved context and prompt text. JSON includes mode, audience, shell/OS, instructions, build timestamp, noninteractive flag. Context only; not a conversation turn. |
| `resources_state.json` | 116 | Tool parameters and tool state, including todos, reported task completions, timeout and background policies. Not projected. |
| `signals.json` | 125 | Session counters and operational metrics, including compaction, cancellation, context use, latency and tool failures. Not per-turn billing. |
| `announcement_state.json` | 130 | Announced skills and MCP fingerprints. Bookkeeping, not user messages. |
| `terminal/`, `web_fetch/` | 93 / 4 | Tool artifact directories. File names and presence inventoried; content not exhaustively inspected. Tool outputs can refer here. |
| `title_refresh_idx` | 80 | Auxiliary refresh bookkeeping; content semantics not verified. |
| `*.lock` | summary/updates: 137 each; chat: 136; rewind: 128 | Lock companions. Existence was inventoried; lock protocol and crash guarantees were not probed. |

[Discovery and lineage](../../internal/history/discovery.go) open the native sources; the module emits typed summary facts through its [catalog reader](../../internal/transcriptparse/grok/metadata.go).

## Update envelope

A minimal scrubbed example is:

```json
{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"session-a","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Answer."}},"_meta":{"eventId":"event-a","promptId":"prompt-a","agentTimestampMs":1893456000123,"chunkId":1}}}
```

`timestamp` is integer Unix seconds. `params._meta.agentTimestampMs` is integer Unix milliseconds and wins when present. File append order and event time are separate. The scan found **4,627 adjacent timestamp decreases** within files. It found no missing event ids, duplicate event ids, missing millisecond timestamps, malformed rows, or true replay markers in the scanned prefixes. These zero observations are not format guarantees.

`params._meta.promptId` is absent from 6,933 rows. User echoes, hooks, terminal notices and some tool updates can lack it. Terminal identity is instead `update.prompt_id`. Other metadata includes `totalTokens`, `streamStartMs`, `turnStartMs`, `updateType`, `chunkId`, and `updateParams`. There were 1,573 rows with `chunkId`. Nano's shared decoder ignores those timing, chunk and update-type fields except the fallback tool status in `updateParams`.

The two `_meta` locations mean different things. `params._meta` describes an event. `params.update._meta` describes content, visibility, or a tool. They remain separate in [shared decoding](../../internal/transcriptparse/grok/decode.go) and native observations.

## Coverage by update record

“Typed” means recognized today, not fully represented in the proposed model. Every row below has a scrubbed fixture under `internal/transcriptparse/grok/testdata/research/capture-*`; the generated appendix lists the exact example and field sets.

| Native method / `sessionUpdate` | Sample count | When and fields | Old parser (db88af8) |
|---|---:|---|---|
| `session/update` / `user_message_chunk` | 127 | User echo or injected content. Single content block; optional content metadata `modelId`, `promptIndex`, `hideFromScrollback`. | Text typed. Four hidden rows intentionally suppressed. Each remaining nonduplicate echo opens a history turn, even if it is only a chunk. Human origin is not proven by role alone. |
| `session/update` / `agent_message_chunk` | 413 | Assistant text segment or delta; event/prompt/chunk metadata. | Text typed. History emits separate completed rows. Live coalesces adjacent assistant chunks. No native final-answer phase is supplied. |
| `session/update` / `agent_thought_chunk` | 1,160 | Reasoning text; same envelope. | Text typed. History emits separate rows; live coalesces. |
| `session/update` / `tool_call` | 2,887 | Starts a call. `toolCallId`, title, `rawInput`, tool descriptor. Initial `Pending` often lives in event metadata. | Typed call. Raw arguments remain native detail in history; live extracts input. |
| `session/update` / `tool_call_update` | 5,730 | Call revision: title/kind, status, locations, raw input/output, content array. Many revisions have no top-level status. | History preserves each revision and classifies terminal statuses as tool results. Live merges by call id. Live loses nontext output and diff structure. |
| `session/update` / `plan` | 46 | Snapshot `entries:[{content,priority,status}]`. | Recognized but shared decoder extracts only a single text content block. Native entries stay in historical Detail; live plan text can be empty and entries are not copied. |
| `_x.ai/session/update` / `turn_completed` | 119 | End of a native prompt, with `prompt_id`, `stop_reason`, optional usage. | Typed terminal and per-turn totals in history, subject to prompt matching. Usage details are incompletely retained. |
| `_x.ai/session/update` / `hook_execution` | 6,572 | `event_name`, optional prompt/tool name, `runs:[{name,status:{status,elapsed_ms,error?}}]`. | Known but raw-only. Includes pre-tool 2,862; post-tool 2,844; stop 483; session-end 380; user-submit 3. A hook “stop” does not establish turn completion. |
| `_x.ai/session/update` / `subagent_spawned` | 13 | Parent prompt/session, child/subagent ids, type, description, model, context source, capability mode. | Compact system activity. Several typed fields are discarded by both assemblers. |
| `_x.ai/session/update` / `subagent_finished` | 13 | Child id, status, duration, calls, turns, tokens, output, `will_wake`. | Compact system activity. Child output remains raw-only. |
| `_x.ai/session/update` / `retry_state` | 4 | `type:retrying`, attempt, maximum retries and reason. | Unclassified event; not typed retry state. Does not itself end a turn. |
| `_x.ai/session/update` / `task_backgrounded` | 40 | `tool_call_id`, task id, command, cwd, output file and description. | Unclassified. Snake-case call id does not populate shared `CallID`. |
| `_x.ai/session/update` / `task_completed` | 39 | Task snapshot, exit/signal, times, output completeness and wake flag. | Unclassified. Command outcome and prompt outcome remain unjoined. |
| `session/update` / `current_mode_update` | 6 | `currentModeId`. | Unclassified; not a typed model/mode observation. |
| `subagent_progress` | 0 corpus; 1 parent-wire fixture | Child metrics and progress. | Recognized but raw-only. |
| Other `subagent_*` | 0; constructed tests | Unknown coordination extension. | Classified as subagent diagnostic, then deliberately suppressed. This bypasses inspectable unknown items. |
| `available_commands_update`, unknown discriminant | 0; constructed tests | Commands or new provider payload. | Shared decoder returns unclassified. History retains detail; live keeps mainly event type. |

Shared classifications are in the module [projector](../../internal/transcriptparse/grok/decode.go), with durable view conversion in [history](../../internal/history/transcript_parsed_item.go). The old “plan unverified” statement describes its original capture, not this corpus.

## Tool content and background execution

Sampled content arrays contain 3,013 `type:content` wrappers and 553 `type:diff` blocks. Nested content is text in 3,004 cases and image in nine. Diff blocks contain `path`, `oldText`, `newText`, and sometimes `_meta` line numbers or edit details. Locations can include both path and line. A status-less diff revision is evidence of a proposed/reported edit, not proof of successful execution.

```json
{"sessionUpdate":"tool_call_update","toolCallId":"call-a","kind":"edit","content":[{"type":"diff","path":"/workspace/example.txt","oldText":"old","newText":"new"}],"locations":[{"path":"/workspace/example.txt","line":1}]}
```

Tool descriptors under `update._meta["x.ai/tool"]` include version, name, kind, namespace, label, read-only flag and safe display input. The top-level ACP kind and the machine tool kind can differ. `rawOutput` is tool-specific: tagged `FileContent`, `ImageContent`, `EditsApplied`, `TodosUpdated`, `Content`, `Result`, `MultiResult`, error variants, or plain command/search objects. The field/type inventory records observed nested variants. Arbitrary tool input and output prevent a finite closed schema for every tool name.

Command output may have `truncated`, byte counts, output file, exit code, signal, timeout and retrieval hints. Twenty direct raw outputs mark truncation. One task snapshot marks truncation. Truncated output is not an interrupted command. A background task may finish after the assistant turn ends; `will_wake` is scheduling evidence, not a human prompt or successful parent turn.

Task timestamps use `{secs_since_epoch,nanos_since_epoch}`. The latter is the fractional nanosecond part. Preserve this precision; do not collapse it to event milliseconds. See `capture-x-ai-session-update-task-completed` in the fixture index.

## Completion, repetition and source damage

109 terminals report `end_turn`; ten report `cancelled`. Six cancellations expose event metadata `cancellationCategory`: five `MidTurnAbort`, one `PermissionRejected`. Other stop reasons are tested in code, not observed here. Transport EOF is not terminal proof because the native leader survives its client.

118 terminals include usage. Every usage object includes `modelUsage`; one also has `usageIsIncomplete:true`. Per-model keys include Grok 4.6 Build and 4.5 Build. Scalars include input, output, total, cache-read, cache-creation, reasoning, model calls, API duration, cost ticks and number of model turns. These are native prompt totals, not session counters. Reasoning overlaps output; cache reads are included in input. Missing is not zero. The module preserves model buckets and the incomplete flag in [native usage observations](../../internal/transcriptparse/grok/assemble.go).

Grok's stream identities can repeat by design. Tool revisions repeat call IDs. Assistant chunks repeat prompt IDs. Distinct events with identical text are legitimate repeated deltas. The module uses native event identity for replay; the fixture `duplicate-versus-identical-delta` asserts this distinction.

The [JSONL reader](../../internal/transcriptparse/grok/reader.go) retains an unterminated tail until append or explicit close. Malformed and oversized records retain source diagnostics. The caller supplies the native row limit; framing and shared decoding use that same bound.

Rewrites of summary, sidecars, and compaction snapshots are expected. The updates-file append contract comes from Nano's documented integration; this read-only audit cannot prove that the provider never rewrites or loses a log. A source generation/replacement must remain distinct from a logical conversation identity.

## Model history, origin and compaction limits

The module's file reader now recognizes the six sampled model-history row
types. It keeps system, user and assistant text, readable reasoning summaries,
opaque encrypted content, tool calls and tool results. An assistant row's tool
calls become separate items with exact argument strings and indexed source
paths. Their model and effort fields remain native detail. No model-history
row supplies an ACP turn boundary or final-answer designation.

A result joins a preceding call only when its model-history call ID is unique
within the source and known session scope. Repeated call IDs stay ambiguous;
missing calls are diagnosed. Identical rows are separate observations because
the sampled format supplies no replay event ID. Native synthetic_reason is
retained as origin evidence. Backend tool status applies only to that tool.

The eight sampled execution-telemetry types remain metadata observations with
all native fields, including source timestamps. They create no empty transcript
items or turns. Invalid scrubbed timestamps stay explicit null time values with
their original text and a diagnostic. Unknown auxiliary rows retain their full
bounded native object with `unknown_auxiliary_record`. These differences from
the research draft are GROK-AUX-1: three nested calls become separate facets,
reasoning summaries become readable parts, and empty telemetry items become
metadata observations. Model-history bytes are recognized only by the file
reader; they are not ACP wire frames.

The auxiliary sample contains six model-history types: system 39, user 157, assistant 173, reasoning 186, tool_result 561, backend_tool_call 8. Assistant rows carry `model_id`, `model_fingerprint`, `reasoning_effort` and native tool calls with JSON-encoded argument strings. Reasoning carries summary blocks and opaque `encrypted_content`. User rows can carry `synthetic_reason`, `prompt_index`, and prior interruption evidence. These are not ACP event ids or safe cross-file join keys by themselves.

This means “the provider has no model/effort evidence” is too broad. ACP terminal `modelUsage` supplies model observations. Model history also supplies assistant model/effort observations. Their relationship to ACP turns needs an explicit, confidence-labelled join; Nano does not currently perform one.

The sample has three summaries with `session_kind:subagent`. Nano's summary struct ignores that key and uses parent-owned sidecars to establish lineage. No dedicated guardian record type was observed. Do not infer “human” from visible user role, or “guardian” from private prompt prose. Programmatic prompts, parent coordination and provider continuations need admitted sender metadata or native origin evidence.

50 sampled rows reference `subagent-completed-*` prompt ids. These are native provider continuations. They must not claim an unresolved operator turn. Hidden user content and task completions also need separate origin/scope fields. Worker transcript bodies remain in their own session, even when carried over the parent's wire.

Compaction request/checkpoint examples are snapshots with model-facing history. No verified one-to-one wire counterpart appeared. Request presence proves a request, checkpoint presence proves saved compacted history; neither establishes exact user-visible turn boundaries without more evidence. Native write ordering among these auxiliary files remains unverified.

The module recognizes the sampled request schema 2 and checkpoint schema 1.
Each produces one compaction item with a `requested` or `checkpoint_saved`
phase, its own native ID alias, creation time and full bounded snapshot detail.
The request's summary is a readable part when supplied. Embedded model history
stays context; it never creates a second set of conversation items. Body capture
can be complete while execution outcome remains unknown. Unknown versions or
malformed history containers retain metadata with `unverified_compaction_schema`.
This interpretation is GROK-COMPACTION-1.

The accepted model also changes three general draft conventions. Empty system
and unknown placeholders become observations, while malformed bytes remain
source records with diagnostics (GROK-OBSERVATION-1). Parent coordination notices
retain child status in native detail; it is not the notice's execution outcome
or parent completion (GROK-COORDINATION-1). Adjacent chunks in one scoped segment
assemble as one item, even when clocks are missing; each chunk retains its own
observation (GROK-SEGMENT-1). The reader's README names every affected fixture.

## Auxiliary examples and precise fields

The appendix below is generated from scrubbed captures. It complements the semantic tables above. Each fixture has its own expected model rendering and manifest. Arrays are short structural excerpts; a placeholder string can stand for an opaque encrypted or image body. See [research fixture README](../../internal/transcriptparse/grok/testdata/research/README.md) for all normal and adversarial cases.

<!-- captured-shape-appendix -->

### summary

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"info":{"id":"id-1","cwd":"/workspace/example"},"session_summary":"[text]","created_at":"2030-01-01T00:00:00.000000Z","updated_at":"2030-01-01T00:00:00.000000Z","num_messages":750,"num_chat_messages":238,"current_model_id":"[text]","next_trace_turn":10,"chat_format_version":1,"git_root_dir":"[text]","git_remotes":["[text]"],"head_commit":"[text]","head_branch":"[text]","request_id":"[text]","grok_home":"[text]","last_active_at":"2030-01-01T00:00:00.000000Z","generated_title":"[text]","agent_name":"[text]","sandbox_profile":"off","reasoning_effort":"high","last_turn_summary":"[text]","last_turn_summary_prompt_id":"[text]"}
```

Observed fields (path:type; dynamic map keys are placeholders):

`agent_name:str`, `chat_format_version:int`, `created_at:str`, `current_model_id:str`, `generated_title:str`, `git_remotes:list`, `git_root_dir:str`, `grok_home:str`, `head_branch:str`, `head_commit:str`, `info.cwd:str`, `info.id:str`, `info:dict`, `last_active_at:str`, `last_turn_summary:str`, `last_turn_summary_prompt_id:str`, `next_trace_turn:int`, `num_chat_messages:int`, `num_messages:int`, `reasoning_effort:str`, `request_id:str`, `sandbox_profile:str`, `session_kind:str`, `session_summary:str`, `updated_at:str`

### session/update:user_message_chunk

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787067254,"method":"session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"[text]"},"_meta":{"modelId":"[text]","promptIndex":0}},"_meta":{"eventId":"id-2","agentTimestampMs":1787067253047}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.eventId:str`, `params._meta:dict`, `params.sessionId:str`, `params.update._meta.hideFromScrollback:bool`, `params.update._meta.modelId:str`, `params.update._meta.promptIndex:int`, `params.update._meta:dict`, `params.update.content.text:str`, `params.update.content.type:str`, `params.update.content:dict`, `params.update.sessionUpdate:str`, `params.update:dict`, `params:dict`, `timestamp:int`

### session/update:agent_thought_chunk

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787067256,"method":"session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"[text]"}},"_meta":{"totalTokens":10104,"eventId":"id-3","agentTimestampMs":1787067254613,"promptId":"id-4","streamStartMs":1787067253681,"turnStartMs":1787067253048,"updateType":"[text]","chunkId":45}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.chunkId:int`, `params._meta.eventId:str`, `params._meta.promptId:str`, `params._meta.streamStartMs:int`, `params._meta.totalTokens:int`, `params._meta.turnStartMs:int`, `params._meta.updateType:str`, `params._meta:dict`, `params.sessionId:str`, `params.update.content.text:str`, `params.update.content.type:str`, `params.update.content:dict`, `params.update.sessionUpdate:str`, `params.update:dict`, `params:dict`, `timestamp:int`

### session/update:agent_message_chunk

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787067260,"method":"session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"[text]"}},"_meta":{"totalTokens":10104,"eventId":"id-5","agentTimestampMs":1787067257579,"promptId":"id-4","streamStartMs":1787067253681,"turnStartMs":1787067253048,"updateType":"[text]","chunkId":77}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.chunkId:int`, `params._meta.eventId:str`, `params._meta.promptId:str`, `params._meta.streamStartMs:int`, `params._meta.totalTokens:int`, `params._meta.turnStartMs:int`, `params._meta.updateType:str`, `params._meta:dict`, `params.sessionId:str`, `params.update.content.text:str`, `params.update.content.type:str`, `params.update.content:dict`, `params.update.sessionUpdate:str`, `params.update:dict`, `params:dict`, `timestamp:int`

### _x.ai/session/update:hook_execution

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787067260,"method":"_x.ai/session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"hook_execution","event_name":"pre_tool_use","tool_name":"[text]","runs":[{"name":"[text]","status":{"status":"failed","error":"[text]","elapsed_ms":0}}]},"_meta":{"eventId":"id-6","agentTimestampMs":1787067260792}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.eventId:str`, `params._meta:dict`, `params.sessionId:str`, `params.update.event_name:str`, `params.update.prompt_id:str`, `params.update.runs:list`, `params.update.runs[].name:str`, `params.update.runs[].status.elapsed_ms:int`, `params.update.runs[].status.error:str`, `params.update.runs[].status.status:str`, `params.update.runs[].status:dict`, `params.update.sessionUpdate:str`, `params.update.tool_name:str`, `params.update:dict`, `params:dict`, `timestamp:int`

### session/update:tool_call

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787067260,"method":"session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"tool_call","toolCallId":"id-7","title":"[text]","rawInput":{"todos":[{"key-0":"id-8","key-1":"[text]","key-2":"in_progress"},{"key-0":"id-9","key-1":"[text]","key-2":"pending"},{"key-0":"id-10","key-1":"[text]","key-2":"pending"}],"merge":false},"_meta":{"x.ai/tool":{"version":1,"name":"[text]","kind":"plan","namespace":"[text]","label":"[text]","read_only":false}}},"_meta":{"totalTokens":20627,"eventId":"id-11","agentTimestampMs":1787067260792,"promptId":"id-4","streamStartMs":1787067253681,"turnStartMs":1787067253048,"updateType":"[text]","updateParams":{"toolCallId":"id-7","title":"[text]","kind":"Other","status":"Pending"}}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.eventId:str`, `params._meta.promptId:str`, `params._meta.streamStartMs:int`, `params._meta.totalTokens:int`, `params._meta.turnStartMs:int`, `params._meta.updateParams.kind:str`, `params._meta.updateParams.status:str`, `params._meta.updateParams.title:str`, `params._meta.updateParams.toolCallId:str`, `params._meta.updateParams:dict`, `params._meta.updateType:str`, `params._meta:dict`, `params.sessionId:str`, `params.update._meta.backend:bool`, `params.update._meta.subagentBackground:bool`, `params.update._meta.x.ai/tool.kind:str`, `params.update._meta.x.ai/tool.label:str`, `params.update._meta.x.ai/tool.name:str`, `params.update._meta.x.ai/tool.namespace:str`, `params.update._meta.x.ai/tool.read_only:bool`, `params.update._meta.x.ai/tool.version:int`, `params.update._meta.x.ai/tool:dict`, `params.update._meta:dict`, `params.update.kind:str`, `params.update.rawInput.-A:int`, `params.update.rawInput.-i:bool`, `params.update.rawInput.backend:bool`, `params.update.rawInput.background:bool`, `params.update.rawInput.capability_mode:str`, `params.update.rawInput.command:str`, `params.update.rawInput.content:str`, `params.update.rawInput.cwd:str`, `params.update.rawInput.description:str`, `params.update.rawInput.file_path:str`, `params.update.rawInput.glob:str`, `params.update.rawInput.head_limit:int`, `params.update.rawInput.limit:int`, `params.update.rawInput.merge:bool`, `params.update.rawInput.model:str`, `params.update.rawInput.new_string:str`, `params.update.rawInput.offset:int`, `params.update.rawInput.old_string:str`, `params.update.rawInput.path:str`, `params.update.rawInput.pattern:str`, `params.update.rawInput.prompt:str`, `params.update.rawInput.query:str`, `params.update.rawInput.replace_all:bool`, `params.update.rawInput.subagent_type:str`, `params.update.rawInput.target_directory:str`, `params.update.rawInput.target_file:str`, `params.update.rawInput.task_id:str`, `params.update.rawInput.task_ids:list`, `params.update.rawInput.timeout:int`, `params.update.rawInput.timeout_ms:int`, `params.update.rawInput.todos:list`, `params.update.rawInput.todos[].content:str`, `params.update.rawInput.todos[].id:str`, `params.update.rawInput.todos[].status:str`, `params.update.rawInput.url:str`, `params.update.rawInput.variant:str`, `params.update.rawInput:dict`, `params.update.sessionUpdate:str`, `params.update.status:str`, `params.update.title:str`, `params.update.toolCallId:str`, `params.update:dict`, `params:dict`, `timestamp:int`

### session/update:tool_call_update

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787067260,"method":"session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"tool_call_update","toolCallId":"id-7","kind":"think","title":"[text]","locations":[],"rawInput":{"variant":"[text]","merge":false,"todos":[{"key-0":"id-8","key-1":"[text]","key-2":"in_progress"},{"key-0":"id-9","key-1":"[text]","key-2":"pending"},{"key-0":"id-10","key-1":"[text]","key-2":"pending"}]},"_meta":{"x.ai/tool":{"version":1,"name":"[text]","kind":"plan","namespace":"[text]","label":"[text]","read_only":false}}},"_meta":{"totalTokens":20627,"eventId":"id-12","agentTimestampMs":1787067260792,"promptId":"id-4","streamStartMs":1787067253681,"turnStartMs":1787067253048,"updateType":"[text]","updateParams":{"toolCallId":"id-7","status":null}}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.eventId:str`, `params._meta.promptId:str`, `params._meta.streamStartMs:int`, `params._meta.totalTokens:int`, `params._meta.turnStartMs:int`, `params._meta.updateParams.status:NoneType`, `params._meta.updateParams.status:str`, `params._meta.updateParams.toolCallId:str`, `params._meta.updateParams:dict`, `params._meta.updateType:str`, `params._meta:dict`, `params.sessionId:str`, `params.update._meta.x.ai/tool.input.command:str`, `params.update._meta.x.ai/tool.input.description:str`, `params.update._meta.x.ai/tool.input.directory:str`, `params.update._meta.x.ai/tool.input.limit:int`, `params.update._meta.x.ai/tool.input.offset:int`, `params.update._meta.x.ai/tool.input.path:str`, `params.update._meta.x.ai/tool.input.pattern:str`, `params.update._meta.x.ai/tool.input:dict`, `params.update._meta.x.ai/tool.kind:str`, `params.update._meta.x.ai/tool.label:str`, `params.update._meta.x.ai/tool.name:str`, `params.update._meta.x.ai/tool.namespace:str`, `params.update._meta.x.ai/tool.read_only:bool`, `params.update._meta.x.ai/tool.version:int`, `params.update._meta.x.ai/tool:dict`, `params.update._meta:dict`, `params.update.content:list`, `params.update.content[]._meta.details:list`, `params.update.content[]._meta.details[].context_after:str`, `params.update.content[]._meta.details[].context_before:str`, `params.update.content[]._meta.details[].line_prefix:str`, `params.update.content[]._meta.details[].new_line:int`, `params.update.content[]._meta.details[].new_string:str`, `params.update.content[]._meta.details[].old_line:int`, `params.update.content[]._meta.details[].old_string:str`, `params.update.content[]._meta.new_line:int`, `params.update.content[]._meta.old_line:int`, `params.update.content[]._meta:dict`, `params.update.content[].content.data:str`, `params.update.content[].content.mimeType:str`, `params.update.content[].content.text:str`, `params.update.content[].content.type:str`, `params.update.content[].content:dict`, `params.update.content[].newText:str`, `params.update.content[].oldText:str`, `params.update.content[].path:str`, `params.update.content[].type:str`, `params.update.kind:str`, `params.update.locations:list`, `params.update.locations[].line:int`, `params.update.locations[].path:str`, `params.update.rawInput.-A:int`, `params.update.rawInput.-i:bool`, `params.update.rawInput.capability_mode:str`, `params.update.rawInput.command:str`, `params.update.rawInput.content:str`, `params.update.rawInput.cwd:str`, `params.update.rawInput.description:str`, `params.update.rawInput.file_path:str`, `params.update.rawInput.glob:NoneType`, `params.update.rawInput.glob:str`, `params.update.rawInput.head_limit:int`, `params.update.rawInput.is_background:bool`, `params.update.rawInput.limit:int`, `params.update.rawInput.merge:bool`, `params.update.rawInput.model:str`, `params.update.rawInput.multiline:bool`, `params.update.rawInput.new_string:str`, `params.update.rawInput.offset:int`, `params.update.rawInput.old_string:str`, `params.update.rawInput.path:NoneType`, `params.update.rawInput.path:str`, `params.update.rawInput.pattern:str`, `params.update.rawInput.persistent:bool`, `params.update.rawInput.prompt:str`, `params.update.rawInput.query:str`, `params.update.rawInput.replace_all:bool`, `params.update.rawInput.run_in_background:bool`, `params.update.rawInput.subagent_type:str`, `params.update.rawInput.target_directory:str`, `params.update.rawInput.target_file:str`, `params.update.rawInput.task_id:NoneType`, `params.update.rawInput.task_id:str`, `params.update.rawInput.task_ids:list`, `params.update.rawInput.timeout:int`, `params.update.rawInput.timeout_ms:NoneType`, `params.update.rawInput.timeout_ms:int`, `params.update.rawInput.todos:list`, `params.update.rawInput.todos[].content:NoneType`, `params.update.rawInput.todos[].content:str`, `params.update.rawInput.todos[].id:str`, `params.update.rawInput.todos[].status:str`, `params.update.rawInput.type:NoneType`, `params.update.rawInput.url:str`, `params.update.rawInput.variant:str`, `params.update.rawInput:dict`, `params.update.rawOutput.Content.absolute_root_path:str`, `params.update.rawOutput.Content.bytes:int`, `params.update.rawOutput.Content.content:str`, `params.update.rawOutput.Content.content_type:str`, `params.update.rawOutput.Content.status_code:int`, `params.update.rawOutput.Content.url:str`, `params.update.rawOutput.Content:dict`, `params.update.rawOutput.CrossHostRedirect.original_host:str`, `params.update.rawOutput.CrossHostRedirect.redirect_url:str`, `params.update.rawOutput.CrossHostRedirect:dict`, `params.update.rawOutput.EditsApplied.absolute_path:str`, `params.update.rawOutput.EditsApplied.edits.details:list`, `params.update.rawOutput.EditsApplied.edits.details[].context_after:str`, `params.update.rawOutput.EditsApplied.edits.details[].context_before:str`, `params.update.rawOutput.EditsApplied.edits.details[].line_prefix:str`, `params.update.rawOutput.EditsApplied.edits.details[].new_line:int`, `params.update.rawOutput.EditsApplied.edits.details[].new_string:str`, `params.update.rawOutput.EditsApplied.edits.details[].old_line:int`, `params.update.rawOutput.EditsApplied.edits.details[].old_string:str`, `params.update.rawOutput.EditsApplied.edits:dict`, `params.update.rawOutput.EditsApplied.new_string:str`, `params.update.rawOutput.EditsApplied.old_string:str`, `params.update.rawOutput.EditsApplied.tool_output_for_prompt:str`, `params.update.rawOutput.EditsApplied.tool_output_for_prompt_concise:str`, `params.update.rawOutput.EditsApplied:dict`, `params.update.rawOutput.Entered.message:str`, `params.update.rawOutput.Entered.plan_file_path:str`, `params.update.rawOutput.Entered.plan_file_seed:str`, `params.update.rawOutput.Entered.tool_hints.ask_user:str`, `params.update.rawOutput.Entered.tool_hints.exit_plan:str`, `params.update.rawOutput.Entered.tool_hints.task:str`, `params.update.rawOutput.Entered.tool_hints:dict`, `params.update.rawOutput.Entered:dict`, `params.update.rawOutput.FileContent.absolute_path:str`, `params.update.rawOutput.FileContent.content:str`, `params.update.rawOutput.FileContent.content_concise:str`, `params.update.rawOutput.FileContent.limit:int`, `params.update.rawOutput.FileContent.offset:NoneType`, `params.update.rawOutput.FileContent.offset:int`, `params.update.rawOutput.FileContent.raw_output:str`, `params.update.rawOutput.FileContent.total_lines:int`, `params.update.rawOutput.FileContent:dict`, `params.update.rawOutput.FileNotFound:str`, `params.update.rawOutput.ImageContent.data:str`, `params.update.rawOutput.ImageContent.mime_type:str`, `params.update.rawOutput.ImageContent:dict`, `params.update.rawOutput.InvalidInput:str`, `params.update.rawOutput.IsADirectory:str`, `params.update.rawOutput.IsAFile:str`, `params.update.rawOutput.MultiResult.mode:str`, `params.update.rawOutput.MultiResult.results:list`, `params.update.rawOutput.MultiResult.results[].command:str`, `params.update.rawOutput.MultiResult.results[].duration_secs:float`, `params.update.rawOutput.MultiResult.results[].ended:NoneType`, `params.update.rawOutput.MultiResult.results[].ended:str`, `params.update.rawOutput.MultiResult.results[].exit_code:NoneType`, `params.update.rawOutput.MultiResult.results[].exit_code:int`, `params.update.rawOutput.MultiResult.results[].output:str`, `params.update.rawOutput.MultiResult.results[].output_file:str`, `params.update.rawOutput.MultiResult.results[].raw_output_bytes:int`, `params.update.rawOutput.MultiResult.results[].started:str`, `params.update.rawOutput.MultiResult.results[].status:str`, `params.update.rawOutput.MultiResult.results[].task_id:str`, `params.update.rawOutput.MultiResult.results[].truncated:bool`, `params.update.rawOutput.MultiResult.results[].truncation_hint:str`, `params.update.rawOutput.MultiResult.summary:str`, `params.update.rawOutput.MultiResult:dict`, `params.update.rawOutput.NoMatchesFound.file_path:str`, `params.update.rawOutput.NoMatchesFound.message:str`, `params.update.rawOutput.NoMatchesFound:dict`, `params.update.rawOutput.NotFound:str`, `params.update.rawOutput.PlanReady.message:str`, `params.update.rawOutput.PlanReady.plan_content:str`, `params.update.rawOutput.PlanReady.plan_file_path:str`, `params.update.rawOutput.PlanReady:dict`, `params.update.rawOutput.Result.command:str`, `params.update.rawOutput.Result.duration_secs:float`, `params.update.rawOutput.Result.ended:NoneType`, `params.update.rawOutput.Result.ended:str`, `params.update.rawOutput.Result.exit_code:NoneType`, `params.update.rawOutput.Result.exit_code:int`, `params.update.rawOutput.Result.message:str`, `params.update.rawOutput.Result.outcome:str`, `params.update.rawOutput.Result.output:str`, `params.update.rawOutput.Result.output_file:str`, `params.update.rawOutput.Result.raw_output_bytes:int`, `params.update.rawOutput.Result.started:str`, `params.update.rawOutput.Result.status:str`, `params.update.rawOutput.Result.task_id:str`, `params.update.rawOutput.Result.truncated:bool`, `params.update.rawOutput.Result.truncation_hint:str`, `params.update.rawOutput.Result:dict`, `params.update.rawOutput.TaskNotFound:str`, `params.update.rawOutput.TodosUpdated.state.todos.<key>:dict`, `params.update.rawOutput.TodosUpdated.state.todos:dict`, `params.update.rawOutput.TodosUpdated.state:dict`, `params.update.rawOutput.TodosUpdated.summary_for_prompt:str`, `params.update.rawOutput.TodosUpdated.todos:list`, `params.update.rawOutput.TodosUpdated.todos[].content:str`, `params.update.rawOutput.TodosUpdated.todos[].priority:str`, `params.update.rawOutput.TodosUpdated.todos[].status:str`, `params.update.rawOutput.TodosUpdated:dict`, `params.update.rawOutput.action.query:str`, `params.update.rawOutput.action.sources:list`, `params.update.rawOutput.action.sources[].type:str`, `params.update.rawOutput.action.sources[].url:str`, `params.update.rawOutput.action.type:str`, `params.update.rawOutput.action.url:str`, `params.update.rawOutput.action:dict`, `params.update.rawOutput.call_id:str`, `params.update.rawOutput.command:str`, `params.update.rawOutput.content:str`, `params.update.rawOutput.current_dir:str`, `params.update.rawOutput.description:str`, `params.update.rawOutput.error:str`, `params.update.rawOutput.exit_code:int`, `params.update.rawOutput.file_matches:list`, `params.update.rawOutput.file_matches[].matches:list`, `params.update.rawOutput.file_matches[].matches[].content:str`, `params.update.rawOutput.file_matches[].matches[].line_number:int`, `params.update.rawOutput.file_matches[].path:str`, `params.update.rawOutput.id:str`, `params.update.rawOutput.input:str`, `params.update.rawOutput.match_count:int`, `params.update.rawOutput.message:str`, `params.update.rawOutput.name:str`, `params.update.rawOutput.output:list`, `params.update.rawOutput.output_file:str`, `params.update.rawOutput.output_for_prompt:str`, `params.update.rawOutput.persistent:bool`, `params.update.rawOutput.pid:int`, `params.update.rawOutput.result_count:int`, `params.update.rawOutput.retrieval_hint:str`, `params.update.rawOutput.signal:NoneType`, `params.update.rawOutput.status:str`, `params.update.rawOutput.stderr:list`, `params.update.rawOutput.stdout:list`, `params.update.rawOutput.summary:str`, `params.update.rawOutput.taskId:str`, `params.update.rawOutput.task_id:str`, `params.update.rawOutput.task_type:str`, `params.update.rawOutput.text:str`, `params.update.rawOutput.timed_out:bool`, `params.update.rawOutput.timeoutMs:int`, `params.update.rawOutput.total_bytes:int`, `params.update.rawOutput.truncated:bool`, `params.update.rawOutput.type:str`, `params.update.rawOutput:dict`, `params.update.sessionUpdate:str`, `params.update.status:str`, `params.update.title:str`, `params.update.toolCallId:str`, `params.update:dict`, `params:dict`, `timestamp:int`

### session/update:plan

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787067261,"method":"session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"plan","entries":[{"content":"[text]","priority":"medium","status":"in_progress"},{"content":"[text]","priority":"medium","status":"pending"},{"content":"[text]","priority":"medium","status":"pending"}]},"_meta":{"totalTokens":20627,"eventId":"id-32","agentTimestampMs":1787067261121,"promptId":"id-4","streamStartMs":1787067253681,"turnStartMs":1787067253048,"updateType":"[text]","updateParams":{"planSteps":3}}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.eventId:str`, `params._meta.promptId:str`, `params._meta.streamStartMs:int`, `params._meta.totalTokens:int`, `params._meta.turnStartMs:int`, `params._meta.updateParams.planSteps:int`, `params._meta.updateParams:dict`, `params._meta.updateType:str`, `params._meta:dict`, `params.sessionId:str`, `params.update.entries:list`, `params.update.entries[].content:str`, `params.update.entries[].priority:str`, `params.update.entries[].status:str`, `params.update.sessionUpdate:str`, `params.update:dict`, `params:dict`, `timestamp:int`

### _x.ai/session/update:turn_completed

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787067431,"method":"_x.ai/session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"turn_completed","prompt_id":"id-4","stop_reason":"end_turn","usage":{"inputTokens":765298,"outputTokens":9583,"totalTokens":774881,"cachedReadTokens":568064,"cacheCreationTokens":0,"reasoningTokens":6883,"modelCalls":11,"apiDurationMs":175390,"costUsdTicks":1351196600,"modelUsage":{"key-0":{"inputTokens":765298,"outputTokens":9583,"totalTokens":774881,"cachedReadTokens":568064,"cacheCreationTokens":0,"reasoningTokens":6883,"modelCalls":11,"apiDurationMs":175390,"costUsdTicks":1351196600}},"numTurns":11}},"_meta":{"eventId":"id-309","agentTimestampMs":1787067431531}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.cancellationCategory:str`, `params._meta.eventId:str`, `params._meta:dict`, `params.sessionId:str`, `params.update.prompt_id:str`, `params.update.sessionUpdate:str`, `params.update.stop_reason:str`, `params.update.usage.apiDurationMs:int`, `params.update.usage.cacheCreationTokens:int`, `params.update.usage.cachedReadTokens:int`, `params.update.usage.costUsdTicks:int`, `params.update.usage.inputTokens:int`, `params.update.usage.modelCalls:int`, `params.update.usage.modelUsage.<key>.apiDurationMs:int`, `params.update.usage.modelUsage.<key>.cacheCreationTokens:int`, `params.update.usage.modelUsage.<key>.cachedReadTokens:int`, `params.update.usage.modelUsage.<key>.costUsdTicks:int`, `params.update.usage.modelUsage.<key>.inputTokens:int`, `params.update.usage.modelUsage.<key>.modelCalls:int`, `params.update.usage.modelUsage.<key>.outputTokens:int`, `params.update.usage.modelUsage.<key>.reasoningTokens:int`, `params.update.usage.modelUsage.<key>.totalTokens:int`, `params.update.usage.modelUsage.<key>:dict`, `params.update.usage.modelUsage:dict`, `params.update.usage.numTurns:int`, `params.update.usage.outputTokens:int`, `params.update.usage.reasoningTokens:int`, `params.update.usage.totalTokens:int`, `params.update.usage.usageIsIncomplete:bool`, `params.update.usage:dict`, `params.update:dict`, `params:dict`, `timestamp:int`

### _x.ai/session/update:subagent_spawned

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787069335,"method":"_x.ai/session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"subagent_spawned","subagent_id":"id-654","parent_session_id":"id-1","parent_prompt_id":"id-525","child_session_id":"id-654","subagent_type":"[text]","description":"[text]","effective_context_source":"new","capability_mode":"all","model":"[text]"},"_meta":{"eventId":"id-655","agentTimestampMs":1787069335265}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.eventId:str`, `params._meta:dict`, `params.sessionId:str`, `params.update.capability_mode:str`, `params.update.child_session_id:str`, `params.update.description:str`, `params.update.effective_context_source:str`, `params.update.model:str`, `params.update.parent_prompt_id:str`, `params.update.parent_session_id:str`, `params.update.sessionUpdate:str`, `params.update.subagent_id:str`, `params.update.subagent_type:str`, `params.update:dict`, `params:dict`, `timestamp:int`

### _x.ai/session/update:subagent_finished

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787069593,"method":"_x.ai/session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"subagent_finished","subagent_id":"id-664","child_session_id":"id-664","status":"completed","tool_calls":41,"turns":1,"duration_ms":258102,"tokens_used":210854,"output":"[text]","will_wake":true},"_meta":{"eventId":"id-679","agentTimestampMs":1787069593503}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.eventId:str`, `params._meta:dict`, `params.sessionId:str`, `params.update.child_session_id:str`, `params.update.duration_ms:int`, `params.update.output:str`, `params.update.sessionUpdate:str`, `params.update.status:str`, `params.update.subagent_id:str`, `params.update.tokens_used:int`, `params.update.tool_calls:int`, `params.update.turns:int`, `params.update.will_wake:bool`, `params.update:dict`, `params:dict`, `timestamp:int`

### hidden_user

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787069595,"method":"session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"[text]"},"_meta":{"modelId":"[text]","promptIndex":5,"hideFromScrollback":true}},"_meta":{"eventId":"id-680","agentTimestampMs":1787069593557}}}
```

### synthetic_prompt

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787069595,"method":"session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"[text]"}},"_meta":{"totalTokens":188810,"eventId":"id-681","agentTimestampMs":1787069595652,"promptId":"subagent-completed-id-682","streamStartMs":1787069594459,"turnStartMs":1787069593560,"updateType":"[text]","chunkId":45}}}
```

### _x.ai/session/update:retry_state

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787069891,"method":"_x.ai/session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"retry_state","type":"retrying","attempt":1,"max_retries":15,"reason":"[text]"},"_meta":{"eventId":"id-743","agentTimestampMs":1787069891606}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.eventId:str`, `params._meta:dict`, `params.sessionId:str`, `params.update.attempt:int`, `params.update.max_retries:int`, `params.update.reason:str`, `params.update.sessionUpdate:str`, `params.update.type:str`, `params.update:dict`, `params:dict`, `timestamp:int`

### terminal_cancelled

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787170806,"method":"_x.ai/session/update","params":{"sessionId":"id-1","update":{"sessionUpdate":"turn_completed","prompt_id":"id-805","stop_reason":"cancelled"},"_meta":{"eventId":"id-806","agentTimestampMs":1787170806533,"cancellationCategory":"MidTurnAbort"}}}
```

### chat_history.jsonl:system

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"type":"system","content":"[text]"}
```

Observed fields (path:type; dynamic map keys are placeholders):

`content:list`, `content:str`, `content[].text:str`, `content[].type:str`, `encrypted_content:str`, `id:str`, `kind.action.query:str`, `kind.action.sources:list`, `kind.action.sources[].type:str`, `kind.action.sources[].url:str`, `kind.action.type:str`, `kind.action.url:str`, `kind.action:dict`, `kind.id:str`, `kind.status:str`, `kind.tool_type:str`, `kind:dict`, `model_fingerprint:str`, `model_id:str`, `prior_turn_interrupt:str`, `prompt_index:int`, `reasoning_effort:str`, `status:str`, `summary:list`, `summary[].text:str`, `summary[].type:str`, `synthetic_reason:str`, `tool_call_id:str`, `tool_calls:list`, `tool_calls[].arguments:str`, `tool_calls[].id:str`, `tool_calls[].name:str`, `type:str`

### chat_history.jsonl:user

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"type":"user","content":[{"type":"text","text":"[text]"}]}
```

Observed fields (path:type; dynamic map keys are placeholders):

`content:list`, `content:str`, `content[].text:str`, `content[].type:str`, `encrypted_content:str`, `id:str`, `kind.action.query:str`, `kind.action.sources:list`, `kind.action.sources[].type:str`, `kind.action.sources[].url:str`, `kind.action.type:str`, `kind.action.url:str`, `kind.action:dict`, `kind.id:str`, `kind.status:str`, `kind.tool_type:str`, `kind:dict`, `model_fingerprint:str`, `model_id:str`, `prior_turn_interrupt:str`, `prompt_index:int`, `reasoning_effort:str`, `status:str`, `summary:list`, `summary[].text:str`, `summary[].type:str`, `synthetic_reason:str`, `tool_call_id:str`, `tool_calls:list`, `tool_calls[].arguments:str`, `tool_calls[].id:str`, `tool_calls[].name:str`, `type:str`

### chat_history.jsonl:reasoning

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"type":"reasoning","id":"id-895","summary":[{"type":"summary_text","text":"[text]"}],"encrypted_content":"[text]","status":"completed"}
```

Observed fields (path:type; dynamic map keys are placeholders):

`content:list`, `content:str`, `content[].text:str`, `content[].type:str`, `encrypted_content:str`, `id:str`, `kind.action.query:str`, `kind.action.sources:list`, `kind.action.sources[].type:str`, `kind.action.sources[].url:str`, `kind.action.type:str`, `kind.action.url:str`, `kind.action:dict`, `kind.id:str`, `kind.status:str`, `kind.tool_type:str`, `kind:dict`, `model_fingerprint:str`, `model_id:str`, `prior_turn_interrupt:str`, `prompt_index:int`, `reasoning_effort:str`, `status:str`, `summary:list`, `summary[].text:str`, `summary[].type:str`, `synthetic_reason:str`, `tool_call_id:str`, `tool_calls:list`, `tool_calls[].arguments:str`, `tool_calls[].id:str`, `tool_calls[].name:str`, `type:str`

### chat_history.jsonl:assistant

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"type":"assistant","content":"[text]","tool_calls":[{"id":"id-7","name":"[text]","arguments":"[text]"},{"id":"id-14","name":"[text]","arguments":"[text]"},{"id":"id-18","name":"[text]","arguments":"[text]"}],"model_id":"[text]","model_fingerprint":"[text]","reasoning_effort":"high"}
```

Observed fields (path:type; dynamic map keys are placeholders):

`content:list`, `content:str`, `content[].text:str`, `content[].type:str`, `encrypted_content:str`, `id:str`, `kind.action.query:str`, `kind.action.sources:list`, `kind.action.sources[].type:str`, `kind.action.sources[].url:str`, `kind.action.type:str`, `kind.action.url:str`, `kind.action:dict`, `kind.id:str`, `kind.status:str`, `kind.tool_type:str`, `kind:dict`, `model_fingerprint:str`, `model_id:str`, `prior_turn_interrupt:str`, `prompt_index:int`, `reasoning_effort:str`, `status:str`, `summary:list`, `summary[].text:str`, `summary[].type:str`, `synthetic_reason:str`, `tool_call_id:str`, `tool_calls:list`, `tool_calls[].arguments:str`, `tool_calls[].id:str`, `tool_calls[].name:str`, `type:str`

### chat_history.jsonl:tool_result

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"type":"tool_result","tool_call_id":"id-7","content":"[text]"}
```

Observed fields (path:type; dynamic map keys are placeholders):

`content:list`, `content:str`, `content[].text:str`, `content[].type:str`, `encrypted_content:str`, `id:str`, `kind.action.query:str`, `kind.action.sources:list`, `kind.action.sources[].type:str`, `kind.action.sources[].url:str`, `kind.action.type:str`, `kind.action.url:str`, `kind.action:dict`, `kind.id:str`, `kind.status:str`, `kind.tool_type:str`, `kind:dict`, `model_fingerprint:str`, `model_id:str`, `prior_turn_interrupt:str`, `prompt_index:int`, `reasoning_effort:str`, `status:str`, `summary:list`, `summary[].text:str`, `summary[].type:str`, `synthetic_reason:str`, `tool_call_id:str`, `tool_calls:list`, `tool_calls[].arguments:str`, `tool_calls[].id:str`, `tool_calls[].name:str`, `type:str`

### events.jsonl:turn_started

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"ts":"[text]","type":"turn_started","session_id":"[text]","turn_number":0,"model_id":"[text]","yolo_mode":true,"conversation_message_count":3,"session_relationship":"[text]","schema_version":"[text]"}
```

Observed fields (path:type; dynamic map keys are placeholders):

`conversation_message_count:int`, `decision:str`, `duration_ms:int`, `loop_index:int`, `model_id:str`, `outcome:str`, `phase:str`, `schema_version:str`, `session_id:str`, `session_relationship:str`, `tool_call_id:str`, `tool_name:str`, `ts:str`, `turn_number:int`, `type:str`, `wait_ms:int`, `yolo_mode:bool`

### events.jsonl:loop_started

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"ts":"[text]","type":"loop_started","loop_index":0}
```

Observed fields (path:type; dynamic map keys are placeholders):

`conversation_message_count:int`, `decision:str`, `duration_ms:int`, `loop_index:int`, `model_id:str`, `outcome:str`, `phase:str`, `schema_version:str`, `session_id:str`, `session_relationship:str`, `tool_call_id:str`, `tool_name:str`, `ts:str`, `turn_number:int`, `type:str`, `wait_ms:int`, `yolo_mode:bool`

### events.jsonl:phase_changed

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"ts":"[text]","type":"phase_changed","phase":"waiting_for_model"}
```

Observed fields (path:type; dynamic map keys are placeholders):

`conversation_message_count:int`, `decision:str`, `duration_ms:int`, `loop_index:int`, `model_id:str`, `outcome:str`, `phase:str`, `schema_version:str`, `session_id:str`, `session_relationship:str`, `tool_call_id:str`, `tool_name:str`, `ts:str`, `turn_number:int`, `type:str`, `wait_ms:int`, `yolo_mode:bool`

### events.jsonl:first_token

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"ts":"[text]","type":"first_token"}
```

Observed fields (path:type; dynamic map keys are placeholders):

`conversation_message_count:int`, `decision:str`, `duration_ms:int`, `loop_index:int`, `model_id:str`, `outcome:str`, `phase:str`, `schema_version:str`, `session_id:str`, `session_relationship:str`, `tool_call_id:str`, `tool_name:str`, `ts:str`, `turn_number:int`, `type:str`, `wait_ms:int`, `yolo_mode:bool`

### rewind_points.jsonl:object

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"prompt_index":0,"created_at":"2030-01-01T00:00:00.000000Z","file_snapshots":{},"after_snapshots":{}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`after_snapshots.<key>.captured_at:str`, `after_snapshots.<key>.content:NoneType`, `after_snapshots.<key>.content:str`, `after_snapshots.<key>.path:str`, `after_snapshots.<key>:dict`, `after_snapshots:dict`, `created_at:str`, `file_snapshots.<key>.captured_at:str`, `file_snapshots.<key>.content:NoneType`, `file_snapshots.<key>.content:str`, `file_snapshots.<key>.path:str`, `file_snapshots.<key>:dict`, `file_snapshots:dict`, `prompt_index:int`

### signals.json:object

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"turnCount":13,"userMessageCount":13,"assistantMessageCount":45,"errorCount":5,"toolFailureCount":4}
```

Observed fields (path:type; dynamic map keys are placeholders):

`agentFilesTouched:int`, `agentLinesAdded:int`, `agentLinesAddedReverted:int`, `agentLinesRemoved:int`, `agentLinesRemovedReverted:int`, `assistantMessageCount:int`, `avgResponseTimeMs:int`, `avgTimeToFirstTokenMs:int`, `bashBareEchoCount:int`, `cancellationCount:int`, `compactionCount:int`, `consecutiveCancellations:int`, `contextTokensUsed:int`, `contextWindowTokens:int`, `contextWindowUsage:int`, `doomLoopRecoveryAbortedChunks:int`, `doomLoopRecoveryAcceptedAfterBudget:int`, `doomLoopRecoveryAttempts:int`, `doomLoopRecoveryTopTrigger:NoneType`, `editAndRetryCount:int`, `errorCount:int`, `gcsQueueCircuitBreakerTrips:int`, `gcsQueueEnqueued:int`, `gcsQueueFailed:int`, `gcsQueueFallbacks:int`, `gcsQueueOrphansCleaned:int`, `gcsQueuePending:int`, `gcsQueuePendingBytes:int`, `gcsQueueUploaded:int`, `gitCommitCount:int`, `hasReverted:bool`, `humanFilesTouched:int`, `humanLinesAdded:int`, `humanLinesAddedReverted:int`, `humanLinesRemoved:int`, `humanLinesRemovedReverted:int`, `inferenceIdleTimeoutConfiguredSecs:int`, `inferenceIdleTimeouts:int`, `itlMaxMs:NoneType`, `itlMaxMs:int`, `itlMeanMs:NoneType`, `itlMeanMs:int`, `itlP50Ms:NoneType`, `itlP50Ms:int`, `itlP99Ms:NoneType`, `itlP99Ms:int`, `itlSampleCount:int`, `latencySampleCount:int`, `longPausesCount:int`, `maxTimeToFirstTokenMs:int`, `minTimeToFirstTokenMs:int`, `modelsUsed:list`, `negativeRatings:int`, `peakRssBytes:int`, `positiveRatings:int`, `prCreatedCount:int`, `prMergedCount:int`, `primaryModelId:str`, `regenerationCount:int`, `sessionDurationSeconds:int`, `toolCallCount:int`, `toolFailureCount:int`, `toolsUsed:list`, `totalChunkCount:int`, `totalFilesTouched:int`, `totalTokensBeforeCompaction:int`, `turnCount:int`, `userMessageCount:int`

### prompt_context.json:object

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"version":1,"prompt_mode":"[text]","audience":"[text]","include_browser_verification":true,"agents_md_files":[{"file_name":"[text]","file_path":"/workspace/example","content":"[text]"},{"file_name":"[text]","file_path":"/workspace/example","content":"[text]"},{"file_name":"[text]","file_path":"/workspace/example","content":"[text]"}]}
```

Observed fields (path:type; dynamic map keys are placeholders):

`agents_md_files:list`, `agents_md_files[].content:str`, `agents_md_files[].file_name:str`, `agents_md_files[].file_path:str`, `audience:str`, `build_timestamp_utc:str`, `current_date:str`, `include_browser_verification:bool`, `is_non_interactive:bool`, `memory_enabled:bool`, `os_name:str`, `persona_summaries:list`, `prompt_body:str`, `prompt_mode:str`, `shell_path:str`, `system_prompt_label:str`, `version:int`, `working_directory:str`

### resources_state.json:object

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"state":{"grok_build.ReportedTaskCompletions":{"reported":["[text]","[text]","[text]"]},"grok_build.Todo":{"todos":{"key-0":{"content":"[text]","priority":"medium","status":"completed"},"key-1":{"content":"[text]","priority":"medium","status":"completed"},"key-2":{"content":"[text]","priority":"medium","status":"completed"}}}},"params":{"grok_build.Bash":{"timeout_secs":null,"max_timeout_secs":36000.0,"output_byte_limit":null,"cmd_prefix":null,"enabled_background":true,"auto_background_on_timeout":true,"foreground_block_budget_ms":null,"max_block_until_ms":null,"allow_background_operator":true,"surface_bg_completion_reminders":true},"grok_build.SearchReplace":{"skip_read_before_edit":false,"empty_old_string_does_not_override":false,"unicode_normalized_fallback":false,"include_user_edit_hint":true},"grok_build.ListDir":{"max_output_chars":null},"grok_build.ReadFile":{"cursor_rules_on_read":false},"grok_build.AskUserQuestion":{"timeout_enabled":true,"timeout_secs":1800,"non_interactive":null},"grok_build.Grep":{"max_output_bytes":null,"max_chars_per_line":null},"grok_build.UseTool":{"native_tool_correction":true},"grok_build.WebFetch":{"cache_ttl_secs":null,"max_cache_entries":null,"timeout_secs":null,"max_content_length":null,"max_markdown_length":null,"context_window_tokens":500000,"allowed_domains":null,"proxy_endpoint":null,"allow_local":null}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`params.grok_build.AskUserQuestion.non_interactive:NoneType`, `params.grok_build.AskUserQuestion.non_interactive:bool`, `params.grok_build.AskUserQuestion.timeout_enabled:bool`, `params.grok_build.AskUserQuestion.timeout_secs:int`, `params.grok_build.AskUserQuestion:dict`, `params.grok_build.Bash.allow_background_operator:bool`, `params.grok_build.Bash.auto_background_on_timeout:bool`, `params.grok_build.Bash.cmd_prefix:NoneType`, `params.grok_build.Bash.enabled_background:bool`, `params.grok_build.Bash.foreground_block_budget_ms:NoneType`, `params.grok_build.Bash.max_block_until_ms:NoneType`, `params.grok_build.Bash.max_timeout_secs:NoneType`, `params.grok_build.Bash.max_timeout_secs:float`, `params.grok_build.Bash.output_byte_limit:NoneType`, `params.grok_build.Bash.surface_bg_completion_reminders:bool`, `params.grok_build.Bash.timeout_secs:NoneType`, `params.grok_build.Bash:dict`, `params.grok_build.Grep.max_chars_per_line:NoneType`, `params.grok_build.Grep.max_output_bytes:NoneType`, `params.grok_build.Grep:dict`, `params.grok_build.ListDir.max_output_chars:NoneType`, `params.grok_build.ListDir:dict`, `params.grok_build.ReadFile.cursor_rules_on_read:bool`, `params.grok_build.ReadFile:dict`, `params.grok_build.SearchReplace.empty_old_string_does_not_override:bool`, `params.grok_build.SearchReplace.include_user_edit_hint:bool`, `params.grok_build.SearchReplace.skip_read_before_edit:bool`, `params.grok_build.SearchReplace.unicode_normalized_fallback:bool`, `params.grok_build.SearchReplace:dict`, `params.grok_build.UseTool.native_tool_correction:bool`, `params.grok_build.UseTool:dict`, `params.grok_build.WebFetch.allow_local:NoneType`, `params.grok_build.WebFetch.allowed_domains:NoneType`, `params.grok_build.WebFetch.cache_ttl_secs:NoneType`, `params.grok_build.WebFetch.context_window_tokens:int`, `params.grok_build.WebFetch.max_cache_entries:NoneType`, `params.grok_build.WebFetch.max_content_length:NoneType`, `params.grok_build.WebFetch.max_markdown_length:NoneType`, `params.grok_build.WebFetch.proxy_endpoint:NoneType`, `params.grok_build.WebFetch.timeout_secs:NoneType`, `params.grok_build.WebFetch:dict`, `params:dict`, `state.grok_build.ReportedTaskCompletions.reported:list`, `state.grok_build.ReportedTaskCompletions:dict`, `state.grok_build.Todo.todos.<key>.content:str`, `state.grok_build.Todo.todos.<key>.priority:str`, `state.grok_build.Todo.todos.<key>.status:str`, `state.grok_build.Todo.todos.<key>:dict`, `state.grok_build.Todo.todos:dict`, `state.grok_build.Todo:dict`, `state:dict`

### announcement_state.json:object

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"mcp_server_fingerprints":{"tasks":{"tool_count":9,"description_hash":12161962213042174405,"tool_names_hash":11988950945106971678},"voice":{"tool_count":1,"description_hash":12161962213042174405,"tool_names_hash":7918047924897568575}},"announced_skill_names":["[text]","[text]","[text]"]}
```

Observed fields (path:type; dynamic map keys are placeholders):

`announced_skill_names:list`, `mcp_server_fingerprints.tasks.description_hash:int`, `mcp_server_fingerprints.tasks.tool_count:int`, `mcp_server_fingerprints.tasks.tool_names_hash:int`, `mcp_server_fingerprints.tasks:dict`, `mcp_server_fingerprints.voice.description_hash:int`, `mcp_server_fingerprints.voice.tool_count:int`, `mcp_server_fingerprints.voice.tool_names_hash:int`, `mcp_server_fingerprints.voice:dict`, `mcp_server_fingerprints:dict`

### subagent_meta

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"subagent_id":"id-654","parent_session_id":"id-1","child_session_id":"id-654","subagent_type":"[text]","description":"[text]","prompt":"[text]","status":"completed","started_at":"2030-01-01T00:00:00.000000Z","completed_at":"2030-01-01T00:00:00.000000Z","duration_ms":386927,"tool_calls":135,"turns":1,"effective_context_source":"new","child_cwd":"[text]","effective_model_id":"[text]"}
```

Observed fields (path:type; dynamic map keys are placeholders):

`child_cwd:str`, `child_session_id:str`, `completed_at:str`, `description:str`, `duration_ms:int`, `effective_context_source:str`, `effective_model_id:str`, `parent_session_id:str`, `prompt:str`, `started_at:str`, `status:str`, `subagent_id:str`, `subagent_type:str`, `tool_calls:int`, `turns:int`

### _x.ai/session/update:task_backgrounded

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787271170,"method":"_x.ai/session/update","params":{"sessionId":"id-900","update":{"sessionUpdate":"task_backgrounded","tool_call_id":"id-954","task_id":"[text]","command":"[text]","cwd":"/workspace/example","output_file":"[text]","description":"[text]"},"_meta":{"eventId":"id-972","agentTimestampMs":1787271170392}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.eventId:str`, `params._meta:dict`, `params.sessionId:str`, `params.update.command:str`, `params.update.cwd:str`, `params.update.description:str`, `params.update.monitor_description:str`, `params.update.output_file:str`, `params.update.sessionUpdate:str`, `params.update.task_id:str`, `params.update.tool_call_id:str`, `params.update:dict`, `params:dict`, `timestamp:int`

### _x.ai/session/update:task_completed

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787271174,"method":"_x.ai/session/update","params":{"sessionId":"id-900","update":{"sessionUpdate":"task_completed","task_snapshot":{"task_id":"[text]","command":"[text]","cwd":"/workspace/example","start_time":{"secs_since_epoch":1787271155,"nanos_since_epoch":306815000},"end_time":{"secs_since_epoch":1787271174,"nanos_since_epoch":91104000},"output":"[text]","output_file":"[text]","truncated":false,"output_total_bytes":215,"exit_code":0,"signal":null,"completed":true,"kind":"bash","block_waited":false,"explicitly_killed":false,"kill_result_delivered":false,"owner_session_id":"[text]","description":"[text]","is_backgrounded":true},"will_wake":true},"_meta":{"eventId":"id-977","agentTimestampMs":1787271174091}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.eventId:str`, `params._meta:dict`, `params.sessionId:str`, `params.update.sessionUpdate:str`, `params.update.task_snapshot.block_waited:bool`, `params.update.task_snapshot.command:str`, `params.update.task_snapshot.completed:bool`, `params.update.task_snapshot.cwd:str`, `params.update.task_snapshot.description:str`, `params.update.task_snapshot.display_command:str`, `params.update.task_snapshot.end_time.nanos_since_epoch:int`, `params.update.task_snapshot.end_time.secs_since_epoch:int`, `params.update.task_snapshot.end_time:dict`, `params.update.task_snapshot.exit_code:NoneType`, `params.update.task_snapshot.exit_code:int`, `params.update.task_snapshot.explicitly_killed:bool`, `params.update.task_snapshot.is_backgrounded:bool`, `params.update.task_snapshot.kill_result_delivered:bool`, `params.update.task_snapshot.kind:str`, `params.update.task_snapshot.output:str`, `params.update.task_snapshot.output_file:str`, `params.update.task_snapshot.output_total_bytes:int`, `params.update.task_snapshot.owner_session_id:str`, `params.update.task_snapshot.signal:NoneType`, `params.update.task_snapshot.signal:str`, `params.update.task_snapshot.start_time.nanos_since_epoch:int`, `params.update.task_snapshot.start_time.secs_since_epoch:int`, `params.update.task_snapshot.start_time:dict`, `params.update.task_snapshot.task_id:str`, `params.update.task_snapshot.truncated:bool`, `params.update.task_snapshot:dict`, `params.update.will_wake:bool`, `params.update:dict`, `params:dict`, `timestamp:int`

### tool_nested_image

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1787274379,"method":"session/update","params":{"sessionId":"id-3097","update":{"sessionUpdate":"tool_call_update","toolCallId":"id-3820","status":"completed","content":[{"type":"content","content":{"type":"image","data":"[text]","mimeType":"[text]"}}]},"_meta":{"totalTokens":150585,"eventId":"id-3825","agentTimestampMs":1787274379637,"promptId":"id-3100","streamStartMs":1787274373772,"turnStartMs":1787273775901,"updateType":"[text]","updateParams":{"toolCallId":"id-3820","status":"Completed"}}}}
```

### plan.json:object

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"todos":{}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`todos:dict`

### plan_mode.json:object

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"state":"[text]","was_previously_active":false,"reminder_count":0,"pending_exit_reminder":false,"awaiting_plan_approval":false}
```

Observed fields (path:type; dynamic map keys are placeholders):

`awaiting_plan_approval:bool`, `pending_exit_reminder:bool`, `reminder_count:int`, `state:str`, `was_previously_active:bool`

### compaction_requests

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"schema_version":2,"request_id":"[text]","created_at":"2030-01-01T00:00:00.000000Z","trigger":"auto","model":"[text]","user_context":null}
```

Observed fields (path:type; dynamic map keys are placeholders):

`attempt_details:list`, `attempt_details[].attempt:int`, `attempt_details[].outcome:str`, `attempt_details[].summary_chars:int`, `attempts:int`, `chat_history:list`, `chat_history[].content:list`, `chat_history[].content:str`, `chat_history[].content[].text:str`, `chat_history[].content[].type:str`, `chat_history[].synthetic_reason:str`, `chat_history[].type:str`, `created_at:str`, `error:NoneType`, `model:str`, `prompt_variant:str`, `request_id:str`, `schema_version:int`, `summary:str`, `tools:list`, `tools[].description:str`, `tools[].name:str`, `tools[].parameters.$schema:str`, `tools[].parameters.properties.background.default:bool`, `tools[].parameters.properties.background.description:str`, `tools[].parameters.properties.background.type:str`, `tools[].parameters.properties.background:dict`, `tools[].parameters.properties.command.description:str`, `tools[].parameters.properties.command.type:str`, `tools[].parameters.properties.command:dict`, `tools[].parameters.properties.description.description:str`, `tools[].parameters.properties.description.type:str`, `tools[].parameters.properties.description:dict`, `tools[].parameters.properties.file_path.description:str`, `tools[].parameters.properties.file_path.type:str`, `tools[].parameters.properties.file_path:dict`, `tools[].parameters.properties.format.description:str`, `tools[].parameters.properties.format.type:list`, `tools[].parameters.properties.format:dict`, `tools[].parameters.properties.limit.description:str`, `tools[].parameters.properties.limit.type:str`, `tools[].parameters.properties.limit:dict`, `tools[].parameters.properties.new_string.description:str`, `tools[].parameters.properties.new_string.type:str`, `tools[].parameters.properties.new_string:dict`, `tools[].parameters.properties.offset.default:int`, `tools[].parameters.properties.offset.description:str`, `tools[].parameters.properties.offset.type:str`, `tools[].parameters.properties.offset:dict`, `tools[].parameters.properties.old_string.description:str`, `tools[].parameters.properties.old_string.type:str`, `tools[].parameters.properties.old_string:dict`, `tools[].parameters.properties.pages.description:str`, `tools[].parameters.properties.pages.type:list`, `tools[].parameters.properties.pages:dict`, `tools[].parameters.properties.replace_all.default:bool`, `tools[].parameters.properties.replace_all.description:str`, `tools[].parameters.properties.replace_all.type:str`, `tools[].parameters.properties.replace_all:dict`, `tools[].parameters.properties.target_file.description:str`, `tools[].parameters.properties.target_file.type:str`, `tools[].parameters.properties.target_file:dict`, `tools[].parameters.properties.timeout.default:int`, `tools[].parameters.properties.timeout.description:str`, `tools[].parameters.properties.timeout.format:str`, `tools[].parameters.properties.timeout.maximum:int`, `tools[].parameters.properties.timeout.minimum:int`, `tools[].parameters.properties.timeout.type:list`, `tools[].parameters.properties.timeout:dict`, `tools[].parameters.properties:dict`, `tools[].parameters.required:list`, `tools[].parameters.type:str`, `tools[].parameters:dict`, `trigger:str`, `user_context:NoneType`

### compaction_checkpoints

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"checkpoint_id":"[text]","prompt_index_at_compaction":25,"compacted_history":[{"type":"system","content":"[text]"},{"type":"user","content":[{"type":"text","text":"[text]"}],"synthetic_reason":"compaction_meta"},{"type":"user","content":[{"type":"text","text":"[text]"}]}],"schema_version":1,"created_at":"2030-01-01T00:00:00.000000Z","original_user_info":"[text]","reread_file_paths":[]}
```

Observed fields (path:type; dynamic map keys are placeholders):

`checkpoint_id:str`, `compacted_history:list`, `compacted_history[].content:list`, `compacted_history[].content:str`, `compacted_history[].content[].text:str`, `compacted_history[].content[].type:str`, `compacted_history[].synthetic_reason:str`, `compacted_history[].type:str`, `created_at:str`, `original_user_info:str`, `prompt_index_at_compaction:int`, `reread_file_paths:list`, `schema_version:int`

### chat_history.jsonl:backend_tool_call

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"type":"backend_tool_call","kind":{"tool_type":"[text]","action":{"type":"open_page","url":"[text]"},"id":"id-7867","status":"completed"}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`content:list`, `content:str`, `content[].text:str`, `content[].type:str`, `encrypted_content:str`, `id:str`, `kind.action.query:str`, `kind.action.sources:list`, `kind.action.sources[].type:str`, `kind.action.sources[].url:str`, `kind.action.type:str`, `kind.action.url:str`, `kind.action:dict`, `kind.id:str`, `kind.status:str`, `kind.tool_type:str`, `kind:dict`, `model_fingerprint:str`, `model_id:str`, `prior_turn_interrupt:str`, `prompt_index:int`, `reasoning_effort:str`, `status:str`, `summary:list`, `summary[].text:str`, `summary[].type:str`, `synthetic_reason:str`, `tool_call_id:str`, `tool_calls:list`, `tool_calls[].arguments:str`, `tool_calls[].id:str`, `tool_calls[].name:str`, `type:str`

### summary_session_kind

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"info":{"id":"id-15030","cwd":"/workspace/example"},"session_kind":"subagent"}
```

### session/update:current_mode_update

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"timestamp":1786650287,"method":"session/update","params":{"sessionId":"id-20241","update":{"sessionUpdate":"current_mode_update","currentModeId":"plan"},"_meta":{"eventId":"id-20251","agentTimestampMs":1786650287239}}}
```

Observed fields (path:type; dynamic map keys are placeholders):

`method:str`, `params._meta.agentTimestampMs:int`, `params._meta.eventId:str`, `params._meta:dict`, `params.sessionId:str`, `params.update.currentModeId:str`, `params.update.sessionUpdate:str`, `params.update:dict`, `params:dict`, `timestamp:int`

### events.jsonl:tool_started

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"ts":"[text]","type":"tool_started","tool_name":"[text]"}
```

Observed fields (path:type; dynamic map keys are placeholders):

`conversation_message_count:int`, `decision:str`, `duration_ms:int`, `loop_index:int`, `model_id:str`, `outcome:str`, `phase:str`, `schema_version:str`, `session_id:str`, `session_relationship:str`, `tool_call_id:str`, `tool_name:str`, `ts:str`, `turn_number:int`, `type:str`, `wait_ms:int`, `yolo_mode:bool`

### events.jsonl:permission_requested

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"ts":"[text]","type":"permission_requested","tool_name":"[text]"}
```

Observed fields (path:type; dynamic map keys are placeholders):

`conversation_message_count:int`, `decision:str`, `duration_ms:int`, `loop_index:int`, `model_id:str`, `outcome:str`, `phase:str`, `schema_version:str`, `session_id:str`, `session_relationship:str`, `tool_call_id:str`, `tool_name:str`, `ts:str`, `turn_number:int`, `type:str`, `wait_ms:int`, `yolo_mode:bool`

### events.jsonl:permission_resolved

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"ts":"[text]","type":"permission_resolved","tool_name":"[text]","decision":"[text]","wait_ms":0}
```

Observed fields (path:type; dynamic map keys are placeholders):

`conversation_message_count:int`, `decision:str`, `duration_ms:int`, `loop_index:int`, `model_id:str`, `outcome:str`, `phase:str`, `schema_version:str`, `session_id:str`, `session_relationship:str`, `tool_call_id:str`, `tool_name:str`, `ts:str`, `turn_number:int`, `type:str`, `wait_ms:int`, `yolo_mode:bool`

### events.jsonl:tool_completed

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"ts":"[text]","type":"tool_completed","tool_name":"[text]","duration_ms":0,"outcome":"[text]","tool_call_id":"id-20369"}
```

Observed fields (path:type; dynamic map keys are placeholders):

`conversation_message_count:int`, `decision:str`, `duration_ms:int`, `loop_index:int`, `model_id:str`, `outcome:str`, `phase:str`, `schema_version:str`, `session_id:str`, `session_relationship:str`, `tool_call_id:str`, `tool_name:str`, `ts:str`, `turn_number:int`, `type:str`, `wait_ms:int`, `yolo_mode:bool`

### hunk_records.jsonl:object

Scrubbed structural excerpt; optional/large fields may be omitted here. The matching fixture and field inventory retain the relevant structure.

```json
{"hunkId":"[text]","filePath":"[text]","hunkStart":1,"hunkEnd":1,"linesAdded":1,"linesRemoved":0,"authorType":"[text]","authorId":"[text]","agentId":"[text]","sessionId":"id-20447","timestamp":"[text]","promptIndex":0,"sourceType":"[text]","eventType":"[text]"}
```

Observed fields (path:type; dynamic map keys are placeholders):

`agentId:str`, `authorId:str`, `authorType:str`, `eventType:str`, `filePath:str`, `hunkEnd:int`, `hunkId:str`, `hunkStart:int`, `linesAdded:int`, `linesRemoved:int`, `promptIndex:int`, `sessionId:str`, `sourceType:str`, `timestamp:str`
