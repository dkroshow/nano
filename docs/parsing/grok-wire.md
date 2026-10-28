# Grok Build: live ACP wire

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

Nano talks newline-delimited JSON-RPC 2.0 to a short-lived `grok agent --leader stdio` client. The supervised native leader owns the session. No provider process was started for this audit. Wire evidence comes from repository code, captured fixtures and the integration decision. The sampled files supply additional update payloads, but do not prove every payload's live method spelling.

Repository inspected: `db88af8b44ed712a99283bf5fd147ea072700b00`, 2026-09-07. The verified compatibility contract names Grok Build 1.0.3. Earlier 0.2.118 evidence has weaker replay signalling. Current native corpus build versions are unknown. This is a bounded inventory, not a claim to know future ACP extensions.

## Frames and update dialects

| Frame | Fields | Meaning and file equivalent |
|---|---|---|
| Notification | `jsonrpc`, `method`, `params`; no request id | Update or control observation. Only update envelopes normally appear in `updates.jsonl`. |
| Server request | `jsonrpc`, `id`, `method`, `params` | Leader asks the client for permission or input. Request id is connection-scoped, not a conversation item id. Not preserved as such in native updates. |
| Result | `jsonrpc`, same `id`, `result` | Reply to a known request. Must join the request ledger to learn method and session. Not a standalone native update. |
| Error | `jsonrpc`, same `id`, `error:{code,message,data?}` | Request failure. Raw code/data need bounded retention. Transport loss is separate. |
| Capture wrapper | `seq`, `dir`, `msg` | Nano fixture/event-feed wrapper, **not provider JSON-RPC**. Unwrap `msg`; retain direction and capture sequence as provenance. |

The pure decoder accepts `session/update`, `_x.ai/session/update`, and `_x.ai/session_notification` ([shared decoder](../../internal/transcriptparse/grok/decode.go)). File extension rows in the sample use `_x.ai/session/update`. Live phase0 captures use `_x.ai/session_notification`. Both carry `{sessionId,update,_meta}`. Treat them as dialect aliases, not distinct conversation events when other native evidence identifies the same event.

Wire notifications usually omit the durable top-level integer-seconds `timestamp`. They retain original `params._meta.agentTimestampMs`. File rows add the seconds timestamp. The shared payload is therefore saved wire-shaped evidence, **not literally the entire wire stream**. The repository's parent wire capture additionally includes queue and prompt-complete notifications absent from its parent updates excerpt.

The 17,169-row file sample counts in [the Grok file reference](grok-file.md) count file observations, not live frequency. The repository phase0 wire excerpt has nine received wrappers: two queues, two user messages, one thought, spawn, progress, finish and prompt-complete. These are intentionally selected excerpts, not representative rates.

## Startup, delivery and terminal controls

| Method / response | Direction and fields | Meaning, handling and coverage |
|---|---|---|
| `initialize` | Client request: protocol version 1 and empty client capabilities. Provider result has capability/model metadata. | `_meta.modelState.currentModelId`, `availableModels` and reasoning efforts drive model discovery. No transcript-file equivalent. Model entries accept several fallback id/effort field spellings in code. |
| `session/new` | Client request: cwd, `mcpServers:[]`, optional `_meta.reasoningEffort`, `autoMode`, `yoloMode`. Provider result supplies session id and selected configuration. | Session identity uses `sessionId`, falling back to `info.id`. Actual selected effort comes from `_meta["x.ai/sessionConfig"].options` with `category:mode`, id and selected flag. Missing/multiple/conflicting selections fail readiness. Summary later records metadata but is not this acceptance signal. |
| `session/load` | Client request: cwd, MCP list, native session id. Result may contain `_meta["x.ai/runningPromptId"]`. | Authoritative running prompt at reconnect. Replayed unfinished content does not substitute. Missing running id settles an orphaned operation without inventing native completion time. |
| `session/prompt` | Client request: session id, `prompt:[{type:text,text}]`. Provider result: `stopReason`, `_meta.promptId`. | Result can bind native delivery id and provide terminal evidence. It is connection-bound and can be lost during detach. Not stored as a result frame in updates. |
| `_x.ai/queue/changed` | Provider notification: session id, `runningPromptId`, `runningText`, `entries:[{id,kind,text}]`. | Exact submitted text plus attached session id binds the operation. Synthetic continuation ids cannot bind it. Replayed text alone must not bind a fresh submission. Raw control, not a message. |
| `_x.ai/session/prompt_complete` | Provider notification: `promptId`, `stopReason`, sometimes session id. | Alternate terminal observation without token usage. Missing session id is accepted only for the exact already-bound prompt. No identical file row found. |
| `turn_completed` update | Provider notification: native prompt id, stop reason, optional usage. | Durable terminal counterpart. The file uses `_x.ai/session/update`; live can use `_x.ai/session_notification`. See terminal details below. |
| `session/cancel` | Client notification: session id. | Request to interrupt; not evidence of completed interruption. Completion comes from provider evidence. No equivalent persisted request frame was found. |

Code: session request [grok.go](../../internal/sessions/grok.go); identity/model [grok.go](../../internal/sessions/grok.go); model normalization [grok.go](../../internal/sessions/grok.go); resume [grok.go](../../internal/sessions/grok.go); exact queue matching [grok.go](../../internal/sessions/grok.go); terminal decoding [grok.go](../../internal/sessions/grok.go); prompt outcome [grok.go](../../internal/sessions/grok.go).

Model selection accepts `_meta.modelState.currentModelId`, `models.currentModelId`, or `_meta["x.ai/sessionDetail"].currentModelId`. These are native observations with different source fields. Do not describe a model-catalog default as the session's effective mode.

## Update payload inventory

The update-body fields and scrubbed samples are in [the Grok file reference](grok-file.md), its appendix, and the capture fixtures. They apply to the common decoder on both paths.

| Update family | Old parser (db88af8) |
|---|---|
| user/message/thought chunks | Single typed content block. Live concatenates assistant/reasoning chunks of the same adjacent kind and prompt. An intervening tool or ordinary item breaks the segment; suppressed hooks/hidden/progress records do not. User prompt id can arrive later and bind an open user item. |
| tool call / update | One logical live tool card per call id. Revisions can carry arguments, text/image content, diffs, locations, tool metadata, raw output, and missing status. Current extraction reads only nested text and raw input. Earlier nonempty text output can survive a later revision with no text. Output revisions are not proven deltas. |
| plan | Actual corpus uses entry snapshots, not a text block. Shared classification recognizes it, but live payload assembly discards entries. A new plan snapshot is not another successful tool execution. |
| current mode | Observed on disk; unclassified live projection if received. Preserve currentModeId as a setting observation, not finality. |
| retry state | Observed on disk; unclassified if received. Attempt/max/reason indicate retry state, not terminal failure. |
| task backgrounded / completed | Observed on disk; unclassified if received. Task id, tool call id, output file and owner session form task identity. A completed task may wake a continuation; do not close an unrelated assistant prompt. |
| hook execution | Recognized and omitted from live cards. Hook status/error describes the hook only. |
| subagent spawn/progress/finish | Parent coordination, separate from detailed child conversation. Spawn/finish yield compact cards; progress is raw-only. |
| unknown `subagent_*` | Known prefix routes to raw-only diagnostic; currently no inspectable normalized unknown item. |
| other unknown update | Shared decoder retains raw/payload. Live retains a minimal event-type card; historical detail is richer. |
| malformed / oversized | Shared decoder returns an error. Live emits a hashed projection-error card for admitted input. Unrelated methods return zero updates. Unknown methods therefore need preservation outside this projector. |

The retired live adapter is available through the retired parser. Current native assembly lives in [the module](../../internal/transcriptparse/grok/assemble.go).

## Replay and multiplexed sessions

`params._meta.isReplay:true` marks replayed notifications in the 1.0.3 contract. Replay can arrive before **or after** the load response. It is historical input, not present activity. The load response alone establishes the running prompt. A replayed event retains original native time; receipt time must remain an additional observation.

Nano checks attached `params.sessionId` before projection, runtime, delivery, operations, and reverse requests. Child rows stay in the raw feed. Session-scoped updates without a session id cannot affect parent state. `prompt_complete` is the narrowly-bound exception. Synthetic `subagent-completed-*` terminals cannot settle operator operations. Visible synthetic assistant chunks are marked settled by the live adapter even when their terminal is filtered. A synthetic reasoning chunk may have different settlement behavior; it deserves a fixture before making broad claims.

Filtering belongs to a **view of the stream**, not loss of the child's identity. A standardized stream needs session scope on every item. It also needs origin separate from role: parent coordination can contain assistant text, and injected control can have user role.

The source file decoder has no equivalent attached-session predicate in `emitGrokUpdate`; it relies on file ownership. Unexpected foreign-session rows in a native file need a diagnostic rather than silent reassignment.

[Ingress and replay: grok.go](../../internal/sessions/grok.go), [session predicate: grok.go](../../internal/sessions/grok.go), [current live conversion](../../internal/app/live_parse_grok.go).

## Reverse requests and answers

These are server-to-client JSON-RPC **requests**, not notifications. They have no directly equivalent native update record, although tool activity, model history, plan state or telemetry can reflect the effect. The provider can re-park an unanswered approval and replay it after `session/load`. Request identity across connections is not proven stable; keep connection request id separate from native tool id.

| Request | Native parameters | Response dialect |
|---|---|---|
| `session/request_permission` | sessionId; `toolCall:{toolCallId,title,rawInput,_meta}`; `options:[{optionId,kind,name}]` | `{"outcome":{"outcome":"selected","optionId":"allow-once"}}`; cancellation is `{"outcome":{"outcome":"cancelled"}}`. Native option ids win over guessed names. |
| `_x.ai/exit_plan_mode` | sessionId, toolCallId, planContent | `{"outcome":"approved"}` or `{"outcome":"abandoned"}`. Other outcomes mean revise, optionally with feedback. Nano sends `revised` on decline/cancel. |
| `_x.ai/ask_user_question` | sessionId, toolCallId, questions with text/options/multiSelect, mode | `{"outcome":"accepted","answers":{"Choose a color":["Blue"]}}`. Keys are full question text, not native question ids. Empty answers mean unanswered. |

The question parser is strict; the plan response parser is fail-soft. Refusing these method names was previously interpreted as client disconnect and canceled the prompt. This is verified repository integration evidence, not a new live probe. Duplicate identical question text is a representational ambiguity: create derived question-index ids while retaining the original answer key.

Nano rejects missing session ids with `-32602` and unsupported reverse methods with `-32601`. An unrouted child's request is declined without parking a parent interaction. Bypass mode can auto-approve a native permission after recording audit evidence. The answer's author/origin is therefore not necessarily human.

[Request handling: grok.go](../../internal/sessions/grok.go), [response handling: grok.go](../../internal/sessions/grok.go), [verified reverse-request contract](../../docs/decisions/grok-native-leader.md).

## Usage and errors

Turn usage lives in `turn_completed.usage`, already at native-prompt grain. Preserve both total counters and `modelUsage` observations, plus `usageIsIncomplete`. Terminal cancellation category lives at `params._meta.cancellationCategory`, not in the update body. No usage is supplied by `prompt_complete` in Nano's decoder. Multiple terminal observations must reconcile one prompt, not add usage repeatedly.

`_x.ai/billing` is a separate account-level request/response extension. Nano uses a short initialized client without creating a session. Modern responses have optional `config`, `subscription_tier` or `subscriptionTier`, `currentPeriod:{type,start,end}`, and `creditUsagePercent`. Numeric fields may be direct numbers or `{val:number}` wrappers. Empty wrapper means zero; an omitted percentage with a valid modern period also means zero. Null/malformed numbers are errors. Legacy responses use `used` and `monthlyLimit`; resets accept several field aliases. A no-limit response is neither a turn counter nor a zero usage assertion. Native financial fields are intentionally excluded from Nano's usage view.

`-32601` is the billing compatibility guard. JSON-RPC prompt errors establish failure when tied to a native prompt. EOF, disconnect and timeout can leave execution unknown because the leader survives. Do not turn a client transport error into provider failure. Exact arbitrary provider error codes and error-data variants are open coverage, not a closed enumerated contract.

[Billing decoder: usage_limits_grok.go:95](../../internal/app/usage_limits_grok.go), [numeric variants: usage_limits_grok.go:236](../../internal/app/usage_limits_grok.go).

## Retention and open coverage

Nano redacts `rawOutput`, signature, env, hostname, agent identity and account presentation fields before its feed/spool receives them. The ACP client itself sees the original frame. Consequently live normalized data can be less rich than native history, even with the same projector. Redaction is not a promise that all remaining prose is public-safe. Research fixtures use stronger placeholder scrubbing.

No live compaction notification, dedicated guardian shape, general elicitation method beyond the three reverse requests, or full binary/version matrix was verified. ACP filesystem and terminal reverse capabilities are not advertised by Nano. They must not be represented as supported just because ACP as a protocol can name them. These are explicit limits on mastery.

[Redaction: grok.go](../../internal/sessions/grok.go). [Fixture catalog](../../internal/transcriptparse/grok/testdata/research/README.md) includes real scrubbed payloads, repository pairs, and constructed replay, terminal, interaction, damage and identity cases. Constructed cases show proposed semantics; they do not prove that a particular provider build emitted every optional field.

### Terminal authority refinement (2026-09-09)

A `stopReason` field alone does not identify a prompt response. Only a matched
`session/prompt` RPC reply, without an RPC error and in the response direction,
can supply terminal evidence. Unmatched, load and reverse-control replies keep
the field as metadata with `unproven_rpc_terminal`. This includes the isolated
result in `handshake-queue-responses-errors`; its following native prompt-complete
notification remains separate evidence.

When prompt_complete lacks a session ID, the reader searches only exact prompt
bindings from the same source incarnation and connection. A binding on another
connection cannot identify the session. Unknown session IDs stay unresolved.
Outgoing prompt-complete frames have no provider terminal authority. Terminal
observations name `$.result` for RPC replies and `$.params` for notifications.
These differences are GROK-CONTROL-2.

### Load state refinement (2026-09-09)

A matched session/load response can establish the running native prompt for its
proven session and connection. Content replay before or after the reply remains
historical content. Without the request, a running-prompt field stays metadata
with `unproven_load_state`; nearby transcript rows cannot supply missing RPC
authority. The load tests add an explicit matching request before the two imported
load/replay specimens, whose original excerpts contain only replies.

A load response without a running prompt clears only the scoped reconnect
binding. It supplies no native interruption outcome or end timestamp. Settling
a local orphaned operation belongs to the caller's runtime policy. Exact repeated
load replies retain replay evidence without replacing a newer queue observation;
conflicting replies remain diagnosed. Load state supplies no submitted prompt
text, so it cannot bind an empty user chunk by string equality. These differences
are GROK-LOAD-1. The RPC lifetime rules below also apply to load requests.

### Queue binding refinement (2026-09-09)

A provider queue notification with no running prompt retires only that session's
text-binding hint. It cannot leave an old prompt attached to a later user chunk.
Queue binding requires an explicit native session and provider direction; caller
scope and outgoing queue-shaped frames cannot supply that authority. Invalid
running-prompt fields are diagnosed without clearing a previously proven binding.

A non-replayed native terminal retires a binding only when its session, connection
and prompt all match. Historical replay cannot retire current queue state, and a
terminal for an older prompt cannot clear a newer prompt's binding. Conversation
outcomes still come from native terminal evidence, never queue absence. These
differences are GROK-QUEUE-1.

### RPC lifetime refinement (2026-09-09)

RPC identities retain their native string or number type within the source,
connection and requester namespace. Other JSON shapes cannot enter the request
map. An exact pending request repeat retains replay evidence and the first
request's scope and source reference; a conflicting repeat is diagnosed. An ID
may name a fresh request after the previous response. Reverse requests in the
client-send direction are retained without being registered as provider requests.

Matched first responses have a typed result_of relationship to their requests.
Exact response replay adds replay_of evidence without repeating native completion
or load-state effects. Conflicting responses keep their safe metadata and a
diagnostic; they do not replace the first interpretation. Canonical JSON hashes
ignore field order and preserve numeric values without floating-point decoding.
These differences are GROK-RPC-2. Further method-specific response interpretation
remains open.

### Reverse interactions (2026-09-09)

Permission, plan and question requests each form an interaction item. A matched
reply adds its own part and observation to that item. The six rows in
`reverse-requests` therefore produce three interactions; the research draft's
three empty reply placeholders become reply observations. RPC IDs remain typed
aliases, distinct from native item IDs and the target tool-call ID.

A permission decision requires one declared option with the selected ID. Its
native kind determines approval or rejection; an option's name does not.
Missing, duplicate or unrecognized options leave the decision unknown. A revised
plan retains feedback and an unknown outcome. Accepted question replies complete
the interaction only when each distinct native question has an answer. Empty
answers and duplicate question keys remain diagnosed and unresolved.

An interaction's body can be complete while its outcome remains running or
unknown. Its decision cannot complete the target tool or the conversation turn.
Exact request and reply replay adds observations without duplicating parts.
These differences are GROK-INTERACTION-1.

GROK-PROVENANCE-1: nested control evidence retains the capture wrapper path.
Wrapped terminal replies point to `$.msg.result`; notifications point to
`$.msg.params`. Question parts identify their array index, plan parts identify
`planContent`, and reply parts identify `result` or `error`. All retain the
outer frame's byte range, hash and direction.

GROK-TIME-3: control requests, replies and other control observations retain
caller-supplied capture and observed clocks. Correlated replies keep the
interaction's scope and their own observation reference. These clocks never
become native creation times. A wrapper's `ts` remains separate capture evidence.

GROK-META-2: native current-mode and retry-state updates are known session
metadata. They do not borrow a caller turn or create transcript items. Every
update's `_meta` object also survives as a separate observation at its native
source path. This retains chunk IDs, timing fields and future extension values
without making replayed envelope metadata a second content delta. Unknown
notifications retain their full bounded native envelope with a diagnostic.

GROK-DIRECTION-1: client-sent update-shaped frames remain metadata with
`unexpected_update_direction`. They cannot supply provider content or terminal
authority. Received frames and durable native file updates keep their normal
interpretation.
