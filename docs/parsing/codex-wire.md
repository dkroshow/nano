# Codex live wire format

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

Research date: 2026-09-07. Code revision `db88af8b44ed712a99283bf5fd147ea072700b00`. No connection was made to the app-server.
The checked-in schema has 87 client requests, 68 server notifications and 10 server requests. Its exact producer build is not recorded in these schema files; do not equate it with the running daemon. The real capture is documented as CLI 0.142.5 on 2026-07-05. Current disk headers reach 0.153.3.

## Framing and scope

Nano talks to the [supervised Codex app-server daemon](../decisions/supervised-app-server.md)
on local port 7691. Wire captures record the protocol logger's `{ts,dir,msg}`
wrappers; the checked-in [paired capture](../../internal/transcriptparse/codex/testdata/research/paired-first-turn/input.jsonl)
is an example input for `transcript-dump --source wire`.

Codex uses JSON-RPC-shaped objects without requiring `jsonrpc: "2.0"`: request `{id,method,params}`, notification `{method,params}`, success `{id,result}`, failure `{id,error:{code,message,data?}}`. IDs can be strings or integers; preserve the original type. Stdio uses one JSON object per newline; WebSocket transports messages, not rollout lines. Transport control/ping/close frames are not conversation items. Capture wrappers `{ts,dir,msg}` are recorder metadata, not provider fields (internal/protocol/client.go:1).

Handshake: initialize request/result, unsolicited status may arrive, initialized client notification, thread/start or thread/resume, turn/start acceptance, then notifications. `turn/start` acceptance is not completion. Client-side requests and server-side requests use separate identity namespaces even if numeric IDs coincide. Requests may remain pending while unrelated thread events flow.

Thread notifications route through params.threadId, legacy conversationId, or thread.id. Events without those fields do not enter a session wire buffer through this route ([codex.go](../../internal/sessions/codex.go)). Connection/account/process-scoped events need a separate observation scope. A socket close is delivery uncertainty, not proof the supervised model turn ended.

## Lifecycle, replay and snapshots

Normal item flow is started → indexed deltas → completed snapshot. Final text can differ from concatenated deltas; completed plan text is explicitly authoritative in the schema. SummaryIndex/contentIndex are separate reasoning part addresses. Terminal command output delta uses plain `delta`; only command/exec and process output streams use `deltaBase64`. Never infer encoding from decodability.

Turn.startedAt/completedAt are Unix seconds. Item startedAtMs/completedAtMs are milliseconds on the notification envelope. Native timestamps can be missing. Capture ts is observed time. `turn.itemsView` distinguishes full/summary/notLoaded; an empty notLoaded array does not prove an empty turn. ThreadItem items may also appear in resume/read/fork responses.

`thread/resume` repairs subscription and returns persisted turns. Nano selects five terminal turns plus active tail, then merges live notifications that raced the reply ([codex_resume_projection.go](../../internal/sessions/codex_resume_projection.go)). This is a snapshot, not provider replayed deltas. The schema has no universal replay sequence/token. Nano's SSE revisions/epochs are its own transport, not Codex-native IDs. There is no separate thread/subscribe arm in this checked-in client schema.

## Notification coverage

All 68 notification names are below. “No dedicated handler” means no semantic Go handler was found; thread-scoped raw wire can still be inspected. Direct item adapter coverage is seven names. Separate runtime, settings, refresh and interaction paths are counted separately. Each name has a constructed fixture.

| Notification | Parameters (* required; other fields optional) | Old parser (db88af8) |
|---|---|---|
| [`error`](../../internal/transcriptparse/codex/testdata/research/wire-error/input.jsonl) | `error`*: TurnError, `threadId`*: string, `turnId`*: string, `willRetry`*: boolean | Live error text; live_projection.go:1551 |
| [`thread/started`](../../internal/transcriptparse/codex/testdata/research/wire-thread-started/input.jsonl) | `thread`*: Thread | Catalog/runtime routing; codex.go:650 |
| [`thread/status/changed`](../../internal/transcriptparse/codex/testdata/research/wire-thread-status-changed/input.jsonl) | `status`*: ThreadStatus, `threadId`*: string | Live status; live_projection.go:1544 |
| [`thread/archived`](../../internal/transcriptparse/codex/testdata/research/wire-thread-archived/input.jsonl) | `threadId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/deleted`](../../internal/transcriptparse/codex/testdata/research/wire-thread-deleted/input.jsonl) | `threadId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/unarchived`](../../internal/transcriptparse/codex/testdata/research/wire-thread-unarchived/input.jsonl) | `threadId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/closed`](../../internal/transcriptparse/codex/testdata/research/wire-thread-closed/input.jsonl) | `threadId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`skills/changed`](../../internal/transcriptparse/codex/testdata/research/wire-skills-changed/input.jsonl) | (empty; see nested schema) | Refresh scheduling only; live_projection.go:1331 |
| [`thread/name/updated`](../../internal/transcriptparse/codex/testdata/research/wire-thread-name-updated/input.jsonl) | `threadId`*: string, `threadName`: string/null | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/goal/updated`](../../internal/transcriptparse/codex/testdata/research/wire-thread-goal-updated/input.jsonl) | `goal`*: ThreadGoal, `threadId`*: string, `turnId`: string/null | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/goal/cleared`](../../internal/transcriptparse/codex/testdata/research/wire-thread-goal-cleared/input.jsonl) | `threadId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/settings/updated`](../../internal/transcriptparse/codex/testdata/research/wire-thread-settings-updated/input.jsonl) | `threadId`*: string, `threadSettings`*: ThreadSettings | Settings evidence; interactions.go:520 |
| [`thread/tokenUsage/updated`](../../internal/transcriptparse/codex/testdata/research/wire-thread-tokenusage-updated/input.jsonl) | `threadId`*: string, `tokenUsage`*: ThreadTokenUsage, `turnId`*: string | Thread-level latest usage, not live per-turn sum; live_projection.go:1549 |
| [`turn/started`](../../internal/transcriptparse/codex/testdata/research/wire-turn-started/input.jsonl) | `threadId`*: string, `turn`*: Turn | Runtime opens turn; runtime.go:365 |
| [`hook/started`](../../internal/transcriptparse/codex/testdata/research/wire-hook-started/input.jsonl) | `run`*: HookRunSummary, `threadId`*: string, `turnId`: string/null | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`turn/completed`](../../internal/transcriptparse/codex/testdata/research/wire-turn-completed/input.jsonl) | `threadId`*: string, `turn`*: Turn | Runtime completion + live turn; live_projection.go:1558 |
| [`hook/completed`](../../internal/transcriptparse/codex/testdata/research/wire-hook-completed/input.jsonl) | `run`*: HookRunSummary, `threadId`*: string, `turnId`: string/null | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`turn/diff/updated`](../../internal/transcriptparse/codex/testdata/research/wire-turn-diff-updated/input.jsonl) | `diff`*: string, `threadId`*: string, `turnId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`turn/plan/updated`](../../internal/transcriptparse/codex/testdata/research/wire-turn-plan-updated/input.jsonl) | `explanation`: string/null, `plan`*: array<TurnPlanStep>, `threadId`*: string, `turnId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`item/started`](../../internal/transcriptparse/codex/testdata/research/wire-item-started/input.jsonl) | `item`*: ThreadItem, `startedAtMs`*: integer, `threadId`*: string, `turnId`*: string | Direct item projection; live_projection.go:241 |
| [`item/autoApprovalReview/started`](../../internal/transcriptparse/codex/testdata/research/wire-item-autoapprovalreview-started/input.jsonl) | `action`*: GuardianApprovalReviewAction, `review`*: GuardianApprovalReview, `reviewId`*: string, `startedAtMs`*: integer, `targetItemId`: string/null, `threadId`*: string, `turnId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`item/autoApprovalReview/completed`](../../internal/transcriptparse/codex/testdata/research/wire-item-autoapprovalreview-completed/input.jsonl) | `action`*: GuardianApprovalReviewAction, `completedAtMs`*: integer, `decisionSource`*: AutoReviewDecisionSource, `review`*: GuardianApprovalReview, `reviewId`*: string, `startedAtMs`*: integer, `targetItemId`: string/null, `threadId`*: string, `turnId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`item/completed`](../../internal/transcriptparse/codex/testdata/research/wire-item-completed/input.jsonl) | `completedAtMs`*: integer, `item`*: ThreadItem, `threadId`*: string, `turnId`*: string | Direct item projection; live_projection.go:241 |
| [`item/agentMessage/delta`](../../internal/transcriptparse/codex/testdata/research/wire-item-agentmessage-delta/input.jsonl) | `delta`*: string, `itemId`*: string, `threadId`*: string, `turnId`*: string | Direct item projection; live_projection.go:241 |
| [`item/plan/delta`](../../internal/transcriptparse/codex/testdata/research/wire-item-plan-delta/input.jsonl) | `delta`*: string, `itemId`*: string, `threadId`*: string, `turnId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`command/exec/outputDelta`](../../internal/transcriptparse/codex/testdata/research/wire-command-exec-outputdelta/input.jsonl) | `capReached`*: boolean, `deltaBase64`*: string, `processId`*: string, `stream`*: CommandExecOutputStream | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`process/outputDelta`](../../internal/transcriptparse/codex/testdata/research/wire-process-outputdelta/input.jsonl) | `capReached`*: boolean, `deltaBase64`*: string, `processHandle`*: string, `stream`*: ProcessOutputStream | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`process/exited`](../../internal/transcriptparse/codex/testdata/research/wire-process-exited/input.jsonl) | `exitCode`*: integer, `processHandle`*: string, `stderr`*: string, `stderrCapReached`*: boolean, `stdout`*: string, `stdoutCapReached`*: boolean | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`item/commandExecution/outputDelta`](../../internal/transcriptparse/codex/testdata/research/wire-item-commandexecution-outputdelta/input.jsonl) | `delta`*: string, `itemId`*: string, `threadId`*: string, `turnId`*: string | Direct item projection; live_projection.go:241 |
| [`item/commandExecution/terminalInteraction`](../../internal/transcriptparse/codex/testdata/research/wire-item-commandexecution-terminalinteraction/input.jsonl) | `itemId`*: string, `processId`*: string, `stdin`*: string, `threadId`*: string, `turnId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`item/fileChange/outputDelta`](../../internal/transcriptparse/codex/testdata/research/wire-item-filechange-outputdelta/input.jsonl) | `delta`*: string, `itemId`*: string, `threadId`*: string, `turnId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`item/fileChange/patchUpdated`](../../internal/transcriptparse/codex/testdata/research/wire-item-filechange-patchupdated/input.jsonl) | `changes`*: array<FileUpdateChange>, `itemId`*: string, `threadId`*: string, `turnId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`serverRequest/resolved`](../../internal/transcriptparse/codex/testdata/research/wire-serverrequest-resolved/input.jsonl) | `requestId`*: RequestId, `threadId`*: string | Interaction resolution; interactions.go:516 |
| [`item/mcpToolCall/progress`](../../internal/transcriptparse/codex/testdata/research/wire-item-mcptoolcall-progress/input.jsonl) | `itemId`*: string, `message`*: string, `threadId`*: string, `turnId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`mcpServer/oauthLogin/completed`](../../internal/transcriptparse/codex/testdata/research/wire-mcpserver-oauthlogin-completed/input.jsonl) | `error`: string/null, `name`*: string, `success`*: boolean | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`mcpServer/startupStatus/updated`](../../internal/transcriptparse/codex/testdata/research/wire-mcpserver-startupstatus-updated/input.jsonl) | `error`: string/null, `name`*: string, `status`*: McpServerStartupState, `threadId`: string/null | Refresh scheduling only; live_projection.go:1331 |
| [`account/updated`](../../internal/transcriptparse/codex/testdata/research/wire-account-updated/input.jsonl) | `authMode`: AuthMode / null, `planType`: PlanType / null | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`account/rateLimits/updated`](../../internal/transcriptparse/codex/testdata/research/wire-account-ratelimits-updated/input.jsonl) | `rateLimits`*: RateLimitSnapshot | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`app/list/updated`](../../internal/transcriptparse/codex/testdata/research/wire-app-list-updated/input.jsonl) | `data`*: array<AppInfo> | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`remoteControl/status/changed`](../../internal/transcriptparse/codex/testdata/research/wire-remotecontrol-status-changed/input.jsonl) | `environmentId`: string/null, `installationId`*: string, `serverName`*: string, `status`*: RemoteControlConnectionStatus | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`externalAgentConfig/import/progress`](../../internal/transcriptparse/codex/testdata/research/wire-externalagentconfig-import-progress/input.jsonl) | `importId`*: string, `itemTypeResults`*: array<ExternalAgentConfigImportTypeResult> | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`externalAgentConfig/import/completed`](../../internal/transcriptparse/codex/testdata/research/wire-externalagentconfig-import-completed/input.jsonl) | `importId`*: string, `itemTypeResults`*: array<ExternalAgentConfigImportTypeResult> | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`fs/changed`](../../internal/transcriptparse/codex/testdata/research/wire-fs-changed/input.jsonl) | `changedPaths`*: array<AbsolutePathBuf>, `watchId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`item/reasoning/summaryTextDelta`](../../internal/transcriptparse/codex/testdata/research/wire-item-reasoning-summarytextdelta/input.jsonl) | `delta`*: string, `itemId`*: string, `summaryIndex`*: integer, `threadId`*: string, `turnId`*: string | Direct item projection; live_projection.go:241 |
| [`item/reasoning/summaryPartAdded`](../../internal/transcriptparse/codex/testdata/research/wire-item-reasoning-summarypartadded/input.jsonl) | `itemId`*: string, `summaryIndex`*: integer, `threadId`*: string, `turnId`*: string | Direct item projection; live_projection.go:241 |
| [`item/reasoning/textDelta`](../../internal/transcriptparse/codex/testdata/research/wire-item-reasoning-textdelta/input.jsonl) | `contentIndex`*: integer, `delta`*: string, `itemId`*: string, `threadId`*: string, `turnId`*: string | Direct item projection; live_projection.go:241 |
| [`thread/compacted`](../../internal/transcriptparse/codex/testdata/research/wire-thread-compacted/input.jsonl) | `threadId`*: string, `turnId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`model/rerouted`](../../internal/transcriptparse/codex/testdata/research/wire-model-rerouted/input.jsonl) | `fromModel`*: string, `reason`*: ModelRerouteReason, `threadId`*: string, `toModel`*: string, `turnId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`model/verification`](../../internal/transcriptparse/codex/testdata/research/wire-model-verification/input.jsonl) | `threadId`*: string, `turnId`*: string, `verifications`*: array<ModelVerification> | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`turn/moderationMetadata`](../../internal/transcriptparse/codex/testdata/research/wire-turn-moderationmetadata/input.jsonl) | `metadata`*: any JSON, `threadId`*: string, `turnId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`model/safetyBuffering/updated`](../../internal/transcriptparse/codex/testdata/research/wire-model-safetybuffering-updated/input.jsonl) | `fasterModel`: string/null, `model`*: string, `reasons`*: array<string>, `showBufferingUi`*: boolean, `threadId`*: string, `turnId`*: string, `useCases`*: array<string> | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`warning`](../../internal/transcriptparse/codex/testdata/research/wire-warning/input.jsonl) | `message`*: string, `threadId`: string/null | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`guardianWarning`](../../internal/transcriptparse/codex/testdata/research/wire-guardianwarning/input.jsonl) | `message`*: string, `threadId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`deprecationNotice`](../../internal/transcriptparse/codex/testdata/research/wire-deprecationnotice/input.jsonl) | `details`: string/null, `summary`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`configWarning`](../../internal/transcriptparse/codex/testdata/research/wire-configwarning/input.jsonl) | `details`: string/null, `path`: string/null, `range`: TextRange / null, `summary`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`fuzzyFileSearch/sessionUpdated`](../../internal/transcriptparse/codex/testdata/research/wire-fuzzyfilesearch-sessionupdated/input.jsonl) | `files`*: array<FuzzyFileSearchResult>, `query`*: string, `sessionId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`fuzzyFileSearch/sessionCompleted`](../../internal/transcriptparse/codex/testdata/research/wire-fuzzyfilesearch-sessioncompleted/input.jsonl) | `sessionId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/realtime/started`](../../internal/transcriptparse/codex/testdata/research/wire-thread-realtime-started/input.jsonl) | `realtimeSessionId`: string/null, `threadId`*: string, `version`*: RealtimeConversationVersion | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/realtime/itemAdded`](../../internal/transcriptparse/codex/testdata/research/wire-thread-realtime-itemadded/input.jsonl) | `item`*: any JSON, `threadId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/realtime/transcript/delta`](../../internal/transcriptparse/codex/testdata/research/wire-thread-realtime-transcript-delta/input.jsonl) | `delta`*: string, `role`*: string, `threadId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/realtime/transcript/done`](../../internal/transcriptparse/codex/testdata/research/wire-thread-realtime-transcript-done/input.jsonl) | `role`*: string, `text`*: string, `threadId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/realtime/outputAudio/delta`](../../internal/transcriptparse/codex/testdata/research/wire-thread-realtime-outputaudio-delta/input.jsonl) | `audio`*: ThreadRealtimeAudioChunk, `threadId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/realtime/sdp`](../../internal/transcriptparse/codex/testdata/research/wire-thread-realtime-sdp/input.jsonl) | `sdp`*: string, `threadId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/realtime/error`](../../internal/transcriptparse/codex/testdata/research/wire-thread-realtime-error/input.jsonl) | `message`*: string, `threadId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`thread/realtime/closed`](../../internal/transcriptparse/codex/testdata/research/wire-thread-realtime-closed/input.jsonl) | `reason`: string/null, `threadId`*: string | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`windows/worldWritableWarning`](../../internal/transcriptparse/codex/testdata/research/wire-windows-worldwritablewarning/input.jsonl) | `extraCount`*: integer, `failedScan`*: boolean, `samplePaths`*: array<string> | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`windowsSandbox/setupCompleted`](../../internal/transcriptparse/codex/testdata/research/wire-windowssandbox-setupcompleted/input.jsonl) | `error`: string/null, `mode`*: WindowsSandboxSetupMode, `success`*: boolean | No dedicated semantic handler found; raw routing only if thread-scoped |
| [`account/login/completed`](../../internal/transcriptparse/codex/testdata/research/wire-account-login-completed/input.jsonl) | `error`: string/null, `loginId`: string/null, `success`*: boolean | No dedicated semantic handler found; raw routing only if thread-scoped |

File and line numbers in the old-parser column refer to the deleted parser;
see the retired parser.

## Server requests and replies

A request is an interaction or tool execution, not ordinary assistant prose. Approvals retain their method-specific answer vocabulary. `serverRequest/resolved` closes a pending request; it does not prove acceptance. Automatic timeout or another client can resolve it.

Nano dispatches all ten schema arms in interactions.go:142. Seven are decision dialogs. Dynamic tool calls have a separate executor. Auth refresh and attestation return -32601 because Nano cannot supply those credentials; do not fixture credentials.

| Server request | Fields (* required) | Meaning / expected response |
|---|---|---|
| [`item/commandExecution/requestApproval`](../../internal/transcriptparse/codex/testdata/research/request-item-commandexecution-requestapproval/input.jsonl) | `threadId`*, `approvalId`, `turnId`*, `command`, `commandActions`, `cwd`, `environmentId`, `itemId`*, `networkApprovalContext`, `proposedExecpolicyAmendment`, `proposedNetworkPolicyAmendments`, `reason`, `startedAtMs`* | Command decision, exec/network policy amendments; response decision union. |
| [`item/fileChange/requestApproval`](../../internal/transcriptparse/codex/testdata/research/request-item-filechange-requestapproval/input.jsonl) | `grantRoot`, `itemId`*, `reason`, `startedAtMs`*, `threadId`*, `turnId`* | File change decision, optional grantRoot. |
| [`item/tool/requestUserInput`](../../internal/transcriptparse/codex/testdata/research/request-item-tool-requestuserinput/input.jsonl) | `autoResolutionMs`, `itemId`*, `questions`*, `threadId`*, `turnId`* | Questions/options, optional autoResolutionMs; response answers keyed by question ID. |
| [`mcpServer/elicitation/request`](../../internal/transcriptparse/codex/testdata/research/request-mcpserver-elicitation-request/input.jsonl) | `serverName`*, `threadId`*, `turnId` | Form or URL elicitation (flattened oneOf); response action/content or metadata. |
| [`item/permissions/requestApproval`](../../internal/transcriptparse/codex/testdata/research/request-item-permissions-requestapproval/input.jsonl) | `cwd`*, `environmentId`, `itemId`*, `permissions`*, `reason`, `startedAtMs`*, `threadId`*, `turnId`* | Permissions grant and scope; not the ordinary command decision enum. |
| [`item/tool/call`](../../internal/transcriptparse/codex/testdata/research/request-item-tool-call/input.jsonl) | `arguments`*, `callId`*, `namespace`, `threadId`*, `tool`*, `turnId`* | Client executes tool(namespace, arguments); response success/contentItems. |
| [`account/chatgptAuthTokens/refresh`](../../internal/transcriptparse/codex/testdata/research/request-account-chatgptauthtokens-refresh/input.jsonl) | `previousAccountId`, `reason`* | Connection auth request; response credential contract. Fixture uses refusal only. |
| [`attestation/generate`](../../internal/transcriptparse/codex/testdata/research/request-attestation-generate/input.jsonl) | see schema unions | Connection attestation; response token contract. Fixture uses refusal only. |
| [`applyPatchApproval`](../../internal/transcriptparse/codex/testdata/research/request-applypatchapproval/input.jsonl) | `callId`*, `conversationId`*, `fileChanges`*, `grantRoot`, `reason` | Legacy fileChanges/callId/conversationId; response review decision. |
| [`execCommandApproval`](../../internal/transcriptparse/codex/testdata/research/request-execcommandapproval/input.jsonl) | `approvalId`, `callId`*, `command`*, `conversationId`*, `cwd`*, `parsedCmd`*, `reason` | Legacy argv/parsedCmd/conversationId; response review decision. |

## ThreadItem union

These 18 item tags are not interchangeable with snake_case response_item tags or newer file PascalCase completion tags. All fields are retained below; nested definitions and enums live in `codex/wire-inventory.json` ([research archive](evidence/README.md)).

| Native type | Fields (* required) | Standard kind |
|---|---|---|
| `userMessage` | `clientId`: string/null, `content`*: array<UserInput>, `id`*: string, `type`*: userMessage | message |
| `hookPrompt` | `fragments`*: array<HookPromptFragment>, `id`*: string, `type`*: hookPrompt | system |
| `agentMessage` | `id`*: string, `memoryCitation`: MemoryCitation / null, `phase`: MessagePhase / null, `text`*: string, `type`*: agentMessage | message |
| `plan` | `id`*: string, `text`*: string, `type`*: plan | plan |
| `reasoning` | `content`: array<string>, `id`*: string, `summary`: array<string>, `type`*: reasoning | reasoning |
| `commandExecution` | `aggregatedOutput`: string/null, `command`*: string, `commandActions`*: array<CommandAction>, `cwd`*: LegacyAppPathString, `durationMs`: integer/null, `exitCode`: integer/null, `id`*: string, `processId`: string/null, `source`: CommandExecutionSource, `status`*: CommandExecutionStatus, `type`*: commandExecution | command |
| `fileChange` | `changes`*: array<FileUpdateChange>, `id`*: string, `status`*: PatchApplyStatus, `type`*: fileChange | file_change |
| `mcpToolCall` | `appContext`: McpToolCallAppContext / null, `arguments`*: any JSON, `durationMs`: integer/null, `error`: McpToolCallError / null, `id`*: string, `mcpAppResourceUri`: string/null, `pluginId`: string/null, `result`: McpToolCallResult / null, `server`*: string, `status`*: McpToolCallStatus, `tool`*: string, `type`*: mcpToolCall | tool_call |
| `dynamicToolCall` | `arguments`*: any JSON, `contentItems`: array/null, `durationMs`: integer/null, `id`*: string, `namespace`: string/null, `status`*: DynamicToolCallStatus, `success`: boolean/null, `tool`*: string, `type`*: dynamicToolCall | tool_call |
| `collabAgentToolCall` | `agentsStates`*: object, `id`*: string, `model`: string/null, `prompt`: string/null, `reasoningEffort`: ReasoningEffort / null, `receiverThreadIds`*: array<string>, `senderThreadId`*: string, `status`*: CollabAgentToolCallStatus, `tool`*: CollabAgentTool, `type`*: collabAgentToolCall | tool_call |
| `subAgentActivity` | `agentPath`*: string, `agentThreadId`*: string, `id`*: string, `kind`*: SubAgentActivityKind, `type`*: subAgentActivity | system |
| `webSearch` | `action`: WebSearchAction / null, `id`*: string, `query`*: string, `type`*: webSearch | tool_call |
| `imageView` | `id`*: string, `path`*: AbsolutePathBuf, `type`*: imageView | tool_call |
| `sleep` | `durationMs`*: integer, `id`*: string, `type`*: sleep | system |
| `imageGeneration` | `id`*: string, `result`*: string, `revisedPrompt`: string/null, `savedPath`: AbsolutePathBuf / null, `status`*: string, `type`*: imageGeneration | tool_call |
| `enteredReviewMode` | `id`*: string, `review`*: string, `type`*: enteredReviewMode | system |
| `exitedReviewMode` | `id`*: string, `review`*: string, `type`*: exitedReviewMode | system |
| `contextCompaction` | `id`*: string, `type`*: contextCompaction | compaction |

UserInput has five alternatives: text (text/text_elements), image (url/detail), localImage (path/detail), skill (name/path), mention (name/path). Image/audio output, MCP content and realtime audio need typed parts, not text flattening.

## Client control requests

These 87 requests are the full checked-in control surface. Replies correlate by request ID, not turn ID. Thread lifecycle and read replies can contain snapshots; most other replies are connection/configuration/resource observations. Presence in this inventory does not authorize calling any method. The research called none.

| Client request | Parameters |
|---|---|
| `initialize` | `capabilities`, `clientInfo` |
| `thread/start` | `approvalPolicy`, `approvalsReviewer`, `baseInstructions`, `config`, `cwd`, `developerInstructions`, `threadSource`, `sandbox`, `ephemeral`, `personality`, `serviceName`, `model`, `modelProvider`, `serviceTier`, `sessionStartSource` |
| `thread/resume` | `approvalPolicy`, `approvalsReviewer`, `baseInstructions`, `config`, `cwd`, `developerInstructions`, `serviceTier`, `threadId`, `personality`, `model`, `modelProvider`, `sandbox` |
| `thread/fork` | `approvalPolicy`, `approvalsReviewer`, `baseInstructions`, `config`, `cwd`, `developerInstructions`, `ephemeral`, `serviceTier`, `model`, `modelProvider`, `threadSource`, `sandbox`, `threadId` |
| `thread/archive` | `threadId` |
| `thread/delete` | `threadId` |
| `thread/unsubscribe` | `threadId` |
| `thread/name/set` | `name`, `threadId` |
| `thread/goal/set` | `objective`, `status`, `threadId`, `tokenBudget` |
| `thread/goal/get` | `threadId` |
| `thread/goal/clear` | `threadId` |
| `thread/metadata/update` | `gitInfo`, `threadId` |
| `thread/unarchive` | `threadId` |
| `thread/compact/start` | `threadId` |
| `thread/shellCommand` | `command`, `threadId` |
| `thread/approveGuardianDeniedAction` | `event`, `threadId` |
| `thread/rollback` | `numTurns`, `threadId` |
| `thread/list` | `archived`, `cursor`, `cwd`, `limit`, `modelProviders`, `useStateDbOnly`, `searchTerm`, `sortDirection`, `sortKey`, `sourceKinds` |
| `thread/loaded/list` | `cursor`, `limit` |
| `thread/read` | `includeTurns`, `threadId` |
| `thread/inject_items` | `items`, `threadId` |
| `skills/list` | `cwds`, `forceReload` |
| `skills/extraRoots/set` | `extraRoots` |
| `hooks/list` | `cwds` |
| `marketplace/add` | `refName`, `source`, `sparsePaths` |
| `marketplace/remove` | `marketplaceName` |
| `marketplace/upgrade` | `marketplaceName` |
| `plugin/list` | `cwds`, `marketplaceKinds` |
| `plugin/installed` | `cwds`, `installSuggestionPluginNames` |
| `plugin/read` | `marketplacePath`, `pluginName`, `remoteMarketplaceName` |
| `plugin/skill/read` | `remoteMarketplaceName`, `remotePluginId`, `skillName` |
| `plugin/share/save` | `discoverability`, `pluginPath`, `remotePluginId`, `shareTargets` |
| `plugin/share/updateTargets` | `discoverability`, `remotePluginId`, `shareTargets` |
| `plugin/share/list` |  |
| `plugin/share/checkout` | `remotePluginId` |
| `plugin/share/delete` | `remotePluginId` |
| `app/list` | `cursor`, `forceRefetch`, `limit`, `threadId` |
| `fs/readFile` | `path` |
| `fs/writeFile` | `dataBase64`, `path` |
| `fs/createDirectory` | `path`, `recursive` |
| `fs/getMetadata` | `path` |
| `fs/readDirectory` | `path` |
| `fs/remove` | `force`, `path`, `recursive` |
| `fs/copy` | `destinationPath`, `recursive`, `sourcePath` |
| `fs/watch` | `path`, `watchId` |
| `fs/unwatch` | `watchId` |
| `skills/config/write` | `enabled`, `name`, `path` |
| `plugin/install` | `marketplacePath`, `pluginName`, `remoteMarketplaceName` |
| `plugin/uninstall` | `pluginId` |
| `turn/start` | `sandboxPolicy`, `approvalPolicy`, `approvalsReviewer`, `clientUserMessageId`, `threadId`, `cwd`, `effort`, `serviceTier`, `input`, `model`, `personality`, `outputSchema`, `summary` |
| `turn/steer` | `threadId`, `clientUserMessageId`, `expectedTurnId`, `input` |
| `turn/interrupt` | `threadId`, `turnId` |
| `review/start` | `delivery`, `target`, `threadId` |
| `model/list` | `cursor`, `includeHidden`, `limit` |
| `modelProvider/capabilities/read` |  |
| `experimentalFeature/list` | `cursor`, `limit`, `threadId` |
| `permissionProfile/list` | `cursor`, `cwd`, `limit` |
| `experimentalFeature/enablement/set` | `enablement` |
| `mcpServer/oauth/login` | `name`, `scopes`, `timeoutSecs` |
| `config/mcpServer/reload` |  |
| `mcpServerStatus/list` | `cursor`, `detail`, `limit`, `threadId` |
| `mcpServer/resource/read` | `server`, `threadId`, `uri` |
| `mcpServer/tool/call` | `_meta`, `arguments`, `server`, `threadId`, `tool` |
| `windowsSandbox/setupStart` | `cwd`, `mode` |
| `windowsSandbox/readiness` |  |
| `account/login/start` |  |
| `account/login/cancel` | `loginId` |
| `account/logout` |  |
| `account/rateLimits/read` |  |
| `account/rateLimitResetCredit/consume` | `idempotencyKey` |
| `account/usage/read` |  |
| `account/workspaceMessages/read` |  |
| `account/sendAddCreditsNudgeEmail` | `creditType` |
| `feedback/upload` | `classification`, `extraLogFiles`, `includeLogs`, `reason`, `tags`, `threadId` |
| `command/exec` | `command`, `cwd`, `disableOutputCap`, `disableTimeout`, `env`, `outputBytesCap`, `tty`, `processId`, `sandboxPolicy`, `size`, `streamStdin`, `streamStdoutStderr`, `timeoutMs` |
| `command/exec/write` | `closeStdin`, `deltaBase64`, `processId` |
| `command/exec/terminate` | `processId` |
| `command/exec/resize` | `processId`, `size` |
| `config/read` | `cwd`, `includeLayers` |
| `externalAgentConfig/detect` | `cwds`, `includeHome` |
| `externalAgentConfig/import` | `migrationItems`, `source` |
| `externalAgentConfig/import/readHistories` |  |
| `config/value/write` | `expectedVersion`, `filePath`, `keyPath`, `mergeStrategy`, `value` |
| `config/batchWrite` | `edits`, `expectedVersion`, `filePath`, `reloadUserConfig` |
| `configRequirements/read` |  |
| `account/read` | `refreshToken` |
| `fuzzyFileSearch` | `cancellationToken`, `query`, `roots` |

`initialized` is a client notification outside the 87 request arms. RPC responses use the corresponding method's response schema; arbitrary errors can occur for any request. Empty/not-loaded snapshots and method errors must not mint turns.

## File counterparts and gaps

Messages, reasoning, commands, patches, compaction, goals/settings, subagent activity, web/MCP tools, and usage have file counterparts, often with different tags and grouping. File token_usage_record adds response/root-turn identities and cache-write counts absent from this live usage schema. File compacted has replacement_history/window lineage absent from contextCompaction items. File world_state and inter_agent_communication_metadata have no dedicated notification arm in this schema.

Wire-only surfaces include streaming deltas, pending permission/questions, resolution notifications, hook runs, realtime audio/transcript/SDP, terminal stdin, progress notifications, process streams, model routing/verification/moderation, account/rate-limit/auth traffic, plugin/app/config/fs/search state, and transport errors. Some can leave indirect tool or context evidence on disk; absence of a dedicated row is not proof they never happened. Ephemeral threads can have a wire history and no rollout.

A real paired conversation is in [paired-first-turn](../../internal/transcriptparse/codex/testdata/research/paired-first-turn/manifest.md). It verifies structural correspondence for a tool-free turn only. It does not validate tools, approvals, realtime or newer record versions.

## Response and elicitation union details

MCP elicitation params flatten three branches: form and openai/form use message/mode/requestedSchema; url uses elicitationId/message/mode/url. All also require serverName and threadId; turnId can be null. These branches remain explicit in `codex/wire-inventory.json` ([research archive](evidence/README.md)).

Command approval decisions are accept, acceptForSession, decline, cancel, acceptWithExecpolicyAmendment, or applyNetworkPolicyAmendment. File approval uses the first four. Permissions reply uses permissions, scope (turn/session), and optional strictAutoReview. Question reply uses answers keyed by question ID. Elicitation reply uses action (accept/decline/cancel), optional content and _meta. Dynamic tool reply uses success and contentItems. Legacy approval uses approved, approved_for_session, denied, abort and amendment object forms. The response schemas and eight request/reply fixture pairs are included. Auth/attestation payloads are intentionally absent; a -32601 error envelope is sufficient for their Nano refusal path.

Realtime transcript/done maps to a message with its native role. Realtime transcript/audio deltas need thread/role or stream identity rather than a fabricated itemId. Auto-approval review events map to guardian interaction evidence, and model/rerouted also updates Turn.modelObservations. These refined mappings are reflected in expected fixture outputs.

## Existing live fixture limitation

The repository's [testdata/live_projection/codex.jsonl](../../internal/app/testdata/live_projection/codex.jsonl) is a deliberately reduced adapter fixture, not a genuine capture. It omits required current-schema routing/timing fields. Its command delta uses a base64 `chunk` field (line 8), while the current native notification schema requires a plain string `delta`. Passing that fixture does not validate native output encoding. The real first-turn capture and newly schema-checked examples provide different evidence.

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

A `thread/started` observation carries the nested thread ID and parent thread ID.
It creates no turn and supplies no default identity for later frames. One wire
connection can carry many threads; a later frame without native or caller thread
scope remains unbound. `TestCodexWireThreadMetadataDoesNotBindMultiplexedTraffic`
checks that boundary.

## Item decoder coverage (2026-09-09)

`TestCodexWireItemTaxonomyRetainsNativeFacets` checks all 18 schema item variants
listed above. MCP, dynamic and collaboration tools retain native tool names.
Image paths remain references; image-generation result strings remain opaque
inline results with no inferred encoding. Review text and hook fragments remain
system content. Child activity remains evidence about a child within the parent
item scope; it does not become a new parent prompt.

Generation completion and execution outcome remain separate. A constructed
`item/completed` specimen with `status: inProgress` has complete generation and
a running native outcome. Unknown item variants retain the full bounded native
payload and emit `unknown_item_type`; they do not silently claim a known type.

## Addressed item updates (2026-09-09)

Plan deltas append native text, including whitespace. A confirming snapshot
replaces both display text and its text part. File-change output appends to a
tool-output part, while patch revisions replace the changes part. MCP progress
and terminal input keep separate parts; neither replaces invocation text or
proves completion. Patch/progress/input observations have `progress_of` links
to their addressed item. Their exact native payload remains in each observation.

When the excerpt lacks a start, the reader retains an incomplete addressed
item and emits `item_update_without_start`. It does not infer missing initial
content or execution success. This is the implemented model shape in place of
the research draft's unresolved observation-only deltas and empty system items.

The usage reader also retains the outer `modelContextWindow` capacity and
native counter objects. Repeated last/total tuples contribute once and are
diagnosed; cumulative decreases remain evidence without negative contributions
or an invented reset epoch. Comparisons are scoped to the source, connection
and native thread.

## Server request checkpoint (2026-09-09)

All ten named server-request schemas now have executable assertions. Approvals
and questions become interactions with complete request generation and a pending
response. Dynamic tool calls keep their call ID, tool name and native arguments.
These facts do not approve execution or complete a turn. Native `startedAtMs`
remains a separate start clock. An explicit native turn ID can own a local turn;
a request without one remains outside a fabricated turn.

RPC IDs retain scalar type in `codex.serverRequestId` aliases. They are not
native item IDs. Repeated pending requests with the same scoped ID and payload
retain replay evidence; changed payloads keep the first request and a conflict
diagnostic. Authentication and attestation requests remain connection metadata
with opaque material excluded. Scoped response correlation and completed-ID
reuse are implemented as described below.

## Request response evidence (2026-09-09)

Eight imported request/response pairs now have executable assertions. Approval
and question replies retain decisions on their interactions. A dynamic-tool
reply creates a separate result facet linked to the generated invocation.
Generation leaves execution unknown; a returned success/failure describes the
result. Neither control decisions nor tool results settle their parent turn.
Null or invalid `success` values never become a reported failure.

A reply matches its typed ID in the opposite requester namespace on the same
source and connection. Conflicting explicit thread/turn scope is rejected.
Directionless collisions remain ambiguous. Exact repeated replies retain
replay evidence without another result; conflicting replies leave the accepted
decision unchanged. Once a request has a response, reusing its ID starts a new
request, even with identical parameters.

Authentication, attestation and unrecognized client-response payloads retain
only their defined safe fields. Further response-union variants still need
their own decoders;
this correlation layer does not claim those payload semantics.

`serverRequest/resolved` now closes the scoped pending request without
inventing approval. When no answer was captured, its decision remains unknown.
A later actual response can supply the decision; a previously known answer
remains unchanged. Repeated resolution notices keep replay evidence. An orphan
notice retains its native reference with `unresolved_rpc_resolution` and creates
no request or turn. Request IDs can be reused after this explicit resolution.

### Snapshot refinement (2026-09-09)

Thread start, read, resume and fork replies decode nested native turns and items.
Each observation names its array position in its source path; source-local item
positions remain fixed by first observation. Native turn seconds and duration
stay on the turn. Missing item clocks remain null. An empty `notLoaded` array
means partial coverage, while an empty thread turn array creates no turn.
Snapshots never establish an active turn for later unaddressed wire traffic.

A received reply without its request can still contain a recognizable thread
snapshot. The reader retains that evidence with `unresolved_snapshot_response`;
it does not claim a request method. Unknown generic replies still exclude opaque
response fields. Matched RPC replay does not apply a snapshot a second time.

A snapshot has no universal offset proving its order against received deltas.
When an existing item's content differs, the reader keeps its assembled text,
adds the snapshot as a metadata observation with its native text and payload,
and reports `snapshot_delta_overlap_unproven`. In the A/B research specimen it
retains both A and B, without claiming AB. Conflicting known turn statuses stay
as observations with `snapshot_turn_status_unproven`. An unknown outcome may
be filled by native snapshot evidence. These rules are CODEX-SNAPSHOT-1; the
research's AB expectation explicitly requires nonoverlap proof absent there.

### Notification evidence refinement (2026-09-09)

Thirty imported connection and thread metadata fixtures now retain their full
native notification payload. These include settings, goals, hook runs, account
and rate-limit updates, filesystem watches, search results, model rerouting and
moderation facts. They are observations at their explicit native scope; a caller
turn default does not attach them to an unrelated conversation turn. Hook or
login completion never completes a conversation. Goal and rate-limit counters
do not become model-token usage. These transport-only observations replace the
research draft's empty system placeholders (CODEX-METADATA-2).

Error, warning, deprecation, guardian warning and compaction notifications retain
conversation-visible bodies. Their body completion says nothing about the parent
turn or action outcome. An error's native message and retry flag remain evidence;
only a native turn terminal may provide terminal authority. Unknown notification
names retain the bounded envelope with `unknown_method`, including names below a
known prefix such as `thread/`. Notification payload retention differs from RPC
reply decoding, whose method-specific limits exclude opaque credential fields.

### Turn aggregates and approval reviews (2026-09-09)

Turn-level plan and unified-diff notifications replace their own aggregate
facet, scoped by native thread and turn. The aggregate is separate from each
command or file-change item. Its source-local position stays fixed; it receives
no invented native item ID. Native plan step statuses remain in typed parts.

Approval reviews are separate interactions with a `codex.reviewId` alias.
Review start/end timestamps retain millisecond meaning. The native review
status determines the review outcome: approved, denied, timed out or aborted.
A completed notification whose review still says inProgress keeps a running
review outcome. Neither its target item reference nor its decision changes
execution evidence or completes the parent turn. Native action, decision source
and rationale remain in detail. These differences are CODEX-PROGRESS-2.

### Realtime refinement (2026-09-09)

Realtime transport startup, SDP and closure remain native-thread observations.
They cannot inherit a caller's conversation turn. Realtime errors retain visible
notice text without completing a conversation. A transcript done event retains
its complete text as a standalone body; known user/assistant roles map directly.
An unrecognized role stays a provider event with `unknown_realtime_role`.

Flat transcript deltas carry no part ID. Audio chunks can name an item, but that
alone does not prove a captured baseline or ordering within it. The reader keeps
these deltas as observations with `unresolved_realtime_part`; it does not append
them to a guessed message. A later done body does not claim the earlier delta.
The audio data and rate/channel fields remain native detail, with no encoding
inferred from a string. Arbitrary itemAdded payloads retain their JSON with
`unverified_realtime_item`, because the checked-in schema does not define them as
ThreadItem. These named differences are CODEX-REALTIME-1.

### Process output refinement (2026-09-09)

`process/outputDelta` and `command/exec/outputDelta` explicitly carry base64
bytes. The reader assembles bytes before decoding UTF-8, so a code point split
across chunks is preserved. Stdout and stderr remain separate parts, with exact
bytes retained as base64 detail. Binary terminal output remains inspectable
without replacement characters. The imported `example` data is invalid base64
and now produces `invalid_process_base64` rather than inferred plain text.

A process exit supplies its own exit outcome, independent of truncation flags.
Empty exit buffers do not erase streamed output. A conflicting nonempty buffer
produces `process_output_overlap_unproven`; malformed buffers remain explicit.
Process handles are scoped by connection and method family. They become native
aliases on output facets, never native item or conversation turn IDs. The reader
diagnoses unresolved invocation correlation; it does not invent a call or a
result_of relationship. These differences are CODEX-PROCESS-1. Process request
and response correlation, handle reuse after a proven new spawn, and retention
limits remain open.

`CODEX-PART-2` orders reasoning display by native part indices, with summaries
before raw content. Observations retain arrival order. Missing or invalid
indices remain native observations with `invalid_part_index`; they cannot
silently address part zero or create a guessed item or turn.
