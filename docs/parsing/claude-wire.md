# Claude Code live wire format

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

Claude Code speaks newline-delimited JSON over stdin/stdout. Nano runs `claude -p --verbose --input-format stream-json --output-format stream-json --include-partial-messages --permission-prompt-tool stdio`. Its shim preserves that protocol and wraps each line in a second transport envelope.

This reference covers every wire discriminator found in the named code, fixtures and survey. It is not a complete vendor schema. No Claude process was launched. No retained production spool was scanned. Exact payloads absent from the evidence are explicitly unverified; their fixtures are constructed. File-only discoveries do not establish wire support.

The current source was inspected at HEAD `db88af8b44ed712a99283bf5fd147ea072700b00`, on 2026-09-07. The July survey probed 2.1.201; control and readiness notes probed 2.1.220. The current file sample reaches 2.1.257. Those are different evidence dates. A newer transcript version does not certify an older wire probe's complete vocabulary.

## Three independent layers

The shim retains session spool files under
`~/.nanod/claude-shim/spools/` by default. They are capture inputs, separate
from historical files under `~/.claude/projects/`. See the
[shim contract](../decisions/claude-shim-daemon.md) for replay and lifetime.

1. **Claude stream-json:** `user`, `assistant`, `stream_event`, `system`, `result`, and control traffic. These are not JSON-RPC frames.
2. **Nano shim:** JSON-RPC over WebSocket. `session/line` wraps the unchanged native line. `session/replayDone` and `session/exited` describe transport state, not assistant utterances.
3. **Nano projection:** normalized item and turn updates, snapshots and replay watermarks. These are Nano output, not extra Claude-native events.

Keep source label, direction, source sequence, native event id and observation time at their actual layer. Claude `session_id`, shimSessionId, control request_id, API message.id and Nano turn ids identify different things.

The new readers accept raw native records, shim spool rows and `session/line`
notifications. The source hash and byte coordinates identify the outer record.
A path such as `$.params.line::json.event.content_block` means decode the JSON
string in `line`, then select the named native field. Shim `ts` is a capture
clock; it never fills a missing native timestamp. Within a source incarnation,
connection and shim session, an exact repeated `seq` adds replay evidence and
does not apply native content, usage or completion again. A changed payload at
the same sequence is diagnosed. `session/exited` and `session/replayDone` remain
transport observations. The constructed capture and checkpoint cases in
`capture_test.go` exercise these rules; complete transport schema coverage is
still pending.

Spawn arguments: [claude.go:184](../../claude.go). The code intentionally omits `--replay-user-messages`, since Nano already records its send lines. An optional echo from other clients is not a new human prompt. No provider-native snapshot-on-resume contract was established. Shim reattachment replays recorded traffic. A fresh `--resume` process reloads disk history but is not equivalent to that replay.

## Coverage table

Counts below describe only `testdata/live_projection/claude.jsonl`: 14 lines, including 13 JSON objects and one malformed line. It is a deliberately small test fixture, not a corpus frequency estimate. “Elsewhere” means code, tests or the recorded survey, not a fresh capture.

| Shape | Fixture lines | Evidence elsewhere | Old parser (db88af8) | File equivalent |
|---|---:|---|---|---|
| `user` | 2 | Send input; receive tool output/task notification | Text item; matched tool output mutates call | Yes, with richer native envelope |
| `assistant` | 2 | Full API message/block snapshots | Only tool_use blocks are processed | Yes; often split over rows |
| `stream_event.message_start` | 1 | Survey's real partial stream | Saves current message.id | No event row; assistant message.id survives |
| `content_block_start` | 2 | Text and tool blocks | Creates live item using message.id:index | Block snapshot in assistant rows |
| `content_block_delta` | 2 | text/thinking/input JSON | Appends text/thinking/partial_json | Final assembled content only |
| `content_block_stop` | 2 | Block boundary | Marks item completed | No distinct row required |
| `message_delta` | 0 | Survey observed | Adapter ignores | stop_reason/usage on assistant row |
| `message_stop` | 0 | Survey observed | Adapter ignores | No distinct event required |
| `system.init` | 0 | Code and 2.1.201/2.1.220 probes | Session identity/config elsewhere; no live item | Common row metadata, not equivalent full init |
| `system.status` | 0 | Survey observed | Raw wire retained; no live item | Recognized if present; zero sampled |
| `system.thinking_tokens` | 0 | Survey observed | Raw wire retained; no live item | usage.output_tokens_details partly overlaps |
| `rate_limit_event` | 0 | Survey observed | Raw wire retained; no live item | No equivalent established |
| Hook lifecycle system events | 0 | Optional flag in survey; file subtype code | No live item | Some hooks/attachments overlap |
| `result` | 1 | Success/error/aborted terminals in code | Completes live turn outside adapter | Some file evidence overlaps; journal result is different |
| `control_request` | 0 | Bidirectional controls and test frames | Interaction/config handler outside adapter | No exact control exchange |
| `control_response` | 0 | Bidirectional success/error responses | Correlates calls; replay answer evidence | Question toolUseResult is partial analogue |
| `control_cancel_request` | 0 | Name in 2.1.201 binary survey only | No dedicated current handler found | None established |
| Future unknown JSON | 1 | Test fixture | Adapter returns no change | File path emits unknown evidence |
| Malformed line | 1 | Test fixture | Adapter returns no change | File path emits malformed evidence |

The table describes the old adapter separately from other handlers. The current
live conversion is [live_parse_claude.go](../../live_parse_claude.go) over the
module. Historical line references below come from the original study; inspect
the deleted parser with the retired parser. “No live item”
does not mean transport bytes vanish: Nano records raw wire events. The old
adapter did not expose all those events as semantic items. Historical dispatch
references: old `live_projection.go:350` (adapter), `claude.go:897` (session
control), and `live_projection.go:849` (turn completion).

## Data messages

### Input and received user messages

Minimal stdin input:

```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Inspect the example."}]}}
```

The message content can also be a string. Optional native envelope properties include session_id, uuid and parent_tool_use_id in stream-json clients; their presence is not guaranteed by Nano's minimal input. Record direction. A send line is submission evidence, not provider confirmation. A receive user row may contain a tool result or task notification.

```json
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-example","content":[{"type":"text","text":"Example output."}],"is_error":false}]}}
```

`tool_use_id` links a reply to a call. Keep `is_error` as execution outcome, not content completeness. Nested output may be multimodal; the old live `claudeBlockText` extracted only object `.text` fields or a bare string. It joined array text without a separator, unlike historical content helpers.

The old adapter's `applyUser` used envelope uuid or a generated `claude-user:N`. Each text block in the same UUID upserted the same item, so the adapter retained the last block. It silently ignored tool results without a remembered call. Matched results attached output and isError to the tool item; they did not become separate result items. These were substantive departures from the target model.

Code: old `live_projection.go:512` (the retired parser), old `live_projection.go:3076` (the retired parser). Fixtures: [multiple blocks](../../internal/transcriptparse/claude/testdata/research/wire-multiple-user-blocks/manifest.md), [orphan output](../../internal/transcriptparse/claude/testdata/research/wire-orphan-result/manifest.md).

### Assistant snapshots

```json
{"type":"assistant","uuid":"row-example","message":{"id":"message-example","role":"assistant","model":"claude-example","content":[{"type":"text","text":"Ready."}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":4}}}
```

This is an assembled message observation. It may confirm content already streamed. It must not duplicate that content or double-count usage. Fable 5.1 can split an API message across observations; do not assume each assistant row is a complete array of every block in the message.

The old adapter's `applyAssistant` inspected only `tool_use`. It replaced tool details with the complete input object and changed NativeItemID to the call id. If the stream block was seen first, that overwrote its earlier message.id:index alias. Without stream deltas, text and reasoning snapshots produced no live items. `server_tool_use` snapshots also missed this branch, though the start-event mapper knew that block type.

Code: old `live_projection.go:453` (the retired parser). Fixture: [snapshot without deltas](../../internal/transcriptparse/claude/testdata/research/wire-snapshot-without-deltas/manifest.md).

### Partial-message events

An event sits inside a stream-json wrapper:

```json
{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Ready."}}}
```

The common sequence is message_start → block start/delta/stop, repeated for blocks → message_delta → message_stop. Full assistant snapshots and system notifications can interleave around that sequence. The survey recorded assistant rows after message events and thinking-token/rate-limit notices. Do not infer a universal strict order beyond the identities and scoped block indices.

| Event / delta | Fields | Meaning | Target behavior |
|---|---|---|---|
| `message_start` | message.id, role, model, content, usage | Opens an API message, not necessarily a human turn | Record message scope/model/initial usage |
| `content_block_start` | index, content_block | Opens one typed block; it can already contain initial text/input | Upsert block item, retain initial content |
| `text_delta` | delta.type, text | Incremental assistant text | Append within that block |
| `thinking_delta` | delta.type, thinking | Incremental reasoning text | Append reasoning |
| `input_json_delta` | delta.type, partial_json | Fragment of tool arguments | Keep raw fragments; parse only once complete |
| `signature_delta` | delta.type, signature | Opaque reasoning signature | Keep bounded opaque metadata; never display as prose |
| `content_block_stop` | index | Generation of the block ended | Completeness=complete; tool outcome remains unknown |
| `message_delta` | delta.stop_reason, stop_sequence; usage | API message stop and revised usage | Record native stop; merge usage observation |
| `message_stop` | type | API-message stream ended | Finalize message assembly, not automatically the conversation turn |

The exact typed delta names in constructed fixtures follow the message-stream convention. The named repo fixture omits delta.type entirely; the old parser switched on field presence instead. `signature_delta` was not found in a retained wire capture during this research. Its shape remains a constructed case, supported only indirectly by observed file signatures. Do not present that fixture as a version-certified wire capture.

`content_block_start` can carry text, thinking, tool_use, server_tool_use, or future block types. The old adapter stored the raw block but did not seed its normalized Text from initial text/thinking. It passed unknown block names through as type strings without the proposed stable unknown diagnostic. It ignored a delta arriving without a start. The old adapter held one current message id, so interleaved independent messages need explicit assembly scopes.

Code: old `live_projection.go:368` (the retired parser), old `live_projection.go:3063` (the retired parser). The [paired split fixtures](../../internal/transcriptparse/claude/testdata/research/paired-split-wire/manifest.md) are **constructed pairs**, not one real conversation captured twice.

## System, usage, rate limits and errors

`system.init` announces native session_id and configuration such as model, tools, mcp_servers, permissionMode, cwd, plugins, agents, skills or slash_commands. The exact inventory varies. Nano reads session_id/model/permissionMode and retains the raw init as session state. The survey also reports effort and output-style/memory information. Keep native configuration observations separate from accepted user settings and inferred model values.

A child may be alive and ready before system.init appears. The 2.1.220 probe found init appeared only after the first user input. An init replay is old evidence; it must not override a later model-setting decision merely because replay was observed now. Code: [claude.go:924](../../claude.go); [ready-on-transport decision](../../docs/decisions/claude-ready-on-transport.md).

`system.status`, `system.thinking_tokens`, and `rate_limit_event` were observed by the 2.1.201 survey. Their full retained payload schemas were not available in the named fixtures. They belong to system facts or usage observations, not fabricated assistant messages. Their example fixtures deliberately contain only the known discriminator and mark the missing detail. The old adapter ignored them. No equivalent historical rate-limit event was found.

Hook start/response events are optional instrumentation under `--include-hook-events`. Nano's inspected argv does not enable that flag. File hook summaries and hook attachments do not prove that every corresponding hook frame is forwarded live.

An assistant API message may carry usage in message_start, message_delta and its full snapshot. The final result can also carry usage, modelUsage, duration_ms, duration_api_ms, num_turns and total_cost_usd in stream-json variants. The recorded survey confirms result usage, including cache splits, but the small repo fixture includes only duration_ms. Exact result-extra field coverage is not measured here. Keep these observations with a declared grain and source. Never sum result totals with per-message totals for the same work. `num_turns` is a provider counter whose relation to Nano conversation turns is not established by this evidence.

The old live item adapter did not promote message usage into the proposed Turn envelope. Historical per-message usage has a more complete explicit aggregation contract. Null, missing and zero differ. `iterations` and server-tool counters can add another native aggregation level; preserve them without double-counting.

### Terminal result

```json
{"type":"result","subtype":"success","is_error":false,"session_id":"session-example","duration_ms":20,"result":"Ready."}
```

```json
{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_by_user","errors":["Example interruption."]}
```

Result ends a Claude execution turn, not the long-lived process. Nano treats terminal_reason beginning `aborted` as interruption, then is_error as failure, otherwise completion. It selects the last assistant item as final only after successful completion. Result text is evidence; it must not duplicate an already assembled final message. Preserve the full errors array, subtype and native reason. The old error detail chose only result/subtype text, which can lose the useful error sentence.

An error result **before** init can mean spawn/resume failure. The missing-transcript error is one known case. An error after init belongs to the turn. A process exit without result is separate evidence; absence of a terminal cannot prove success. An interrupt acknowledgment is also insufficient: idle and active sessions can acknowledge identically.

Code: old `live_projection.go:3403` (the retired parser), [claude.go:890](../../claude.go), [claude_control.go:318](../../claude_control.go).

## Bidirectional control channel

Request framing:

```json
{"type":"control_request","request_id":"request-example","request":{"subtype":"initialize"}}
```

Success and failure framing:

```json
{"type":"control_response","response":{"subtype":"success","request_id":"request-example","response":{"models":[],"commands":[]}}}
```

```json
{"type":"control_response","response":{"subtype":"error","request_id":"request-example","error":"Example error."}}
```

Requests and responses interleave freely with data. The same envelope works in both directions. A response's request_id lives inside response, not at the root. Scope ids by process/session and request direction. Nano-generated ids include a process nonce to keep stale responses from resolving new requests.

The new readers retain permission and elicitation requests as interactions.
Settings exchanges remain observations. A matched response has `result_of`
evidence pointing to its request; exact replay preserves the settled decision.
An interaction's outcome describes that control decision, not tool execution.
Absent request direction uses the known request subtype's direction. Replies
without direction cannot choose between two matching requester namespaces.
Unmatched replies remain observations with diagnostics. The control fixtures
also keep uncertain hook, MCP and cancellation schemas explicitly unverified.
Pending-array hydration remains a coverage gap; retaining its native detail
does not claim that behavior works.

| Request subtype | Direction / trigger | Payload and response | Old parser (db88af8) |
|---|---|---|---|
| `initialize` | Client→CLI on start/attach | Request subtype; response models, commands; optional pending_permission_requests and pending_user_dialog_requests | Old handler. Pending arrays were absent even with an open permission during a 2.1.220 stdio probe |
| `interrupt` | Client→CLI stop request | Success acknowledgment; optional still_queued receipt | Old handler; ack alone does not prove turn interruption |
| `set_model` | Client→CLI setting | model string; default resets spawn default; success/error response | Old handler; applies to future turns |
| `set_permission_mode` | Client→CLI setting | Mode payload convention; exact current schema not verified here | Name found by 2.1.201 survey; no current outbound method in claude_control.go |
| `can_use_tool` | CLI→client permission/question | tool_name, input; optional tool_use_id and other permission context | Old permission/AskUserQuestion router |
| `elicitation` | CLI→client MCP input | Prompt/schema/server detail; response action accept/decline/cancel, optional content | Old router and response encoder; full request variants not captured here |
| `request_user_dialog` | CLI→client dialog | dialog_kind and opaque kind-specific payload | Old code declines; response status=cancelled |
| `hook_callback` | Control client hook callback | Callback identity/input; exact shape unavailable | Binary-survey name only; constructed unknown/system case |
| `mcp_message` | MCP control traffic | Server/message payload; exact shape unavailable | Binary-survey name only; constructed unknown/system case |
| `control_cancel_request` | Cancellation | request id association presumed; envelope unverified here | Survey named it alongside subtypes, but exact root-vs-subtype framing was not captured. Keep unverified |

The inline examples demonstrate known frames. The hook/MCP/cancel fixtures deliberately mark uncertain payloads. They cannot support an “all wire shapes mastered” claim. Unknown future controls must remain inspectable even when operational handling declines them.

### Questions and permissions

AskUserQuestion arrives through can_use_tool with tool_name=AskUserQuestion, not a unique question subtype. Its input has questions with question text, header, options and multiSelect. Each answer is keyed by the question's exact text. A multi-select value can be a label array; current code supports it, but a real multi-select capture was not established here.

```json
{"type":"control_response","response":{"subtype":"success","request_id":"question-example","response":{"behavior":"allow","updatedInput":{"questions":[{"question":"Choose a color.","multiSelect":false}],"answers":{"Choose a color.":"Blue"}}}}}
```

Permission allow echoes input as updatedInput. Deny returns behavior=deny plus message. Skipping a question allows the tool with an empty answers object; it is not a denied tool. An AskUserQuestion tool call, control interaction, and file toolUseResult answer are correlated evidence for one interaction. The control request id and tool call id must remain separate aliases. File-only evidence cannot reconstruct the exact permission exchange or its decision actor.

The accepted model keeps the interaction invocation and its tool result as
separate facets. The composite fixture therefore yields two linked items,
instead of the study's single combined item. A control request can augment a
uniquely matching earlier question invocation; its reply preserves the original
content. Single-block file answers retain exact answer keys and arrays and
link only when the scoped call is unambiguous. Controls alone do not establish
a conversation turn. Later native calls and mixed result rows remain coverage
gaps.

Code: [claude_control.go:480](../../claude_control.go), [interactions.go:259](../../interactions.go). Fixture: [question/control/file answer](../../internal/transcriptparse/claude/testdata/research/question-control-and-file-answer/manifest.md).

## Shim envelopes, replay and process state

Disk spool entry:

```json
{"seq":1,"ts":"2026-01-01T00:00:00.123456789Z","dir":"recv","line":"{\"type\":\"result\",\"subtype\":\"success\"}"}
```

WebSocket notification:

```json
{"jsonrpc":"2.0","method":"session/line","params":{"shimSessionId":"shim-example","seq":1,"ts":"2026-01-01T00:00:00.123456789Z","dir":"recv","line":"{\"type\":\"result\",\"subtype\":\"success\"}"}}
```

`seq` starts at one and covers both directions within one shim session. `ts` is shim observation time formatted RFC3339Nano; it is **not a Claude native timestamp**. Older shim events can omit it. Preserve first-observed time and replay-received time separately. A replayed event must not be redated to the current attachment.

| Shim operation / notification | Fields | Meaning |
|---|---|---|
| `session/start` | bin, args, cwd → shimSessionId | Creates transport-owned child; not a conversation message |
| `session/attach` | shimSessionId or nativeSessionId, fromSeq | Reattach from exclusive sequence watermark |
| Attach result | shimSessionId, nativeSessionId, state, turnActive, lastSeq | Process/turn snapshot, not a native provider turn id |
| `session/input` | shimSessionId, line | Send unchanged native frame; successful write precedes spool acceptance |
| `session/close` | shimSessionId | Process lifecycle request |
| `session/list` | sessions | Transport inventory |
| `session/line` | shimSessionId, seq, ts, dir, line | One spooled native observation |
| `session/replayDone` | shimSessionId, lastSeq | Finite replay boundary |
| `session/exited` | shimSessionId, exitCode | Child process exit, not automatically a successful turn |
| JSON-RPC error | code, message | Transport/request failure; codes include -32001 attached, -32002 not found, -32003 exited |

Payload definitions: [cmd/claude-shim/server.go:92](../../cmd/claude-shim/server.go). Spool shape: [cmd/claude-shim/spool.go:43](../../cmd/claude-shim/spool.go). Timestamp assignment: [cmd/claude-shim/session.go:292](../../cmd/claude-shim/session.go).

On reattach, receive requests are held until replay ends. An already recorded response proves an old dialog was answered. Only unmatched requests are rehydrated, merged with any pending arrays returned by initialize. Replaying the same seq is not another semantic event. Transport replay certainty depends on the complete spool and single attached client. A partial spool cannot justify the same conclusion.

The source explicitly records that documented initialize pending arrays did not fire with an open stdio permission request. The spool fallback is therefore central, not optional polish. [claude_control.go:115](../../claude_control.go), [claude_control.go:405](../../claude_control.go).

`session/line` ingestion suppresses already-seen seq values and passes the shim's original timestamp into delivery. [claude_shim_transport.go:424](../../claude_shim_transport.go). A shim process can be running while its turn is idle; keep process state and turnActive separate. [cmd/claude-shim/session.go:309](../../cmd/claude-shim/session.go).

The shim scanner allows 64 MiB lines, while the historical parser defaults to 32 MiB. A line fitting the wire can exceed the file parser budget. Oversize and malformed transport failures need diagnostics with their own layer, not invented Claude error events.

## Direct adapter verification

Six in-memory probes ran against the inspected live adapter using the fixtures here. They confirmed the snapshot-only, orphan-result, multiple-user-block and initial-text losses described above. They also confirmed that failed tool output remains attached to a generated call labelled completed, illustrating why generation and execution need separate fields. The normal constructed split stream assembled the expected three semantic items. The [research archive](evidence/README.md) retains the results in `claude/current-live-probe-summary.json`. No provider process or deployed service was exercised.

## File/wire differences and limits

Native files preserve row UUIDs, parent links, local timestamps, compaction-preserved segments, origin flags, file backups, attachments and worker sidecars. Most have no demonstrated one-for-one stream-json frame. Wire preserves deltas, control exchanges, terminal results, init configuration and transport observations that files may not retain.

A complete file and wire capture of the same conversation was not available in the named evidence. The paired split fixture is constructed and explicitly labelled. The survey reports a real wire capture but does not retain its full payloads in the named document. Reproducing every versioned wire field remains unproven.

Unknown native events should become bounded unknown items plus diagnostics. Nonsemantic transport frames can remain observations linked to items/turns instead of fabricating conversation turns. Raw wire retention remains useful evidence, but it does not meet standardized-stream mastery by itself.

## Added paired evidence (2026-09-09)

The [genuine paired assistant excerpt](../../internal/transcriptparse/claude/testdata/captured/paired-assistant-excerpt/manifest.md)
adds matching native file and shim-spool records from an existing conversation.
It covers two split assistant blocks, streaming snapshots, usage revisions and
a successful native result. It is an excerpt, not a complete conversation.
The original research limitation above still applies to full captures.

This capture uses `request_id` on wire assistant envelopes and `requestId` in
the file. Both retain the same API request alias. Its result carries
`terminal_reason: completed`, `stop_reason: end_turn` and `subtype: success`.
The completed terminal finalizes the answer; file-only stop evidence remains
a derived candidate. Early wire snapshots have output usage 1; the final
response snapshot has 52. The separate result model bucket has 63 output
tokens and never replaces or adds to the response total.

`CLAUDE-OBSERVATION-2` retains orphan deltas and stop/usage payloads as unresolved
observations with diagnostics. There is no invented message or turn owner.
`CLAUDE-UNKNOWN-1` keeps unknown block types and their native envelope roles.
The research server-result example is explicitly unobserved, so the reader
retains its call ID in native detail without asserting execution or linkage.

`CLAUDE-CONTROL-3` separates RPC success from an interaction decision. A
permission response needs native `behavior`; an elicitation needs native
`action`. Empty or unrecognized decisions stay unknown with a diagnostic.
They still retain their request/reply relationship.

`CLAUDE-QUESTION-2` joins a uniquely matching earlier question control with a
later generated native call. The first item position and prior decision remain;
earlier observations keep their original unknown scope. `CLAUDE-CONTROL-4`
retains initialization pending arrays as bounded detail and diagnoses nonempty
or malformed arrays instead of hydrating an unverified nested request schema.
