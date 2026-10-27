# Annotated wire transcript — first app-server turn

Synthetic example matching the codex-cli **0.142.5** wire shape
(`gpt-5.5`, service tier `priority`). Raw JSONL: [first-turn.jsonl](first-turn.jsonl)
(34 messages, `{"ts","dir","msg"}` per line). Prompt: *"Reply with exactly:
SAMPLE_READY"* — deliberately tool-free, so this is the minimal turn
lifecycle; command/patch/MCP items will add to it.

## Framing

Newline-delimited JSON over stdio. Codex's JSON-RPC variant has **no
`"jsonrpc":"2.0"` field**: requests are `{id, method, params}`, notifications
`{method, params}`, responses `{id, result|error}`. Client request ids can be
int or string.

## Message sequence

```
SEND initialize                    → RECV response (userAgent, codexHome, platform)
RECV remoteControl/status/changed    (unsolicited server state, pre-handshake-complete)
SEND initialized                     (notification — completes handshake, MCP-style)
SEND thread/start                  → RECV response (thread + effective config)
RECV thread/started
RECV mcpServer/startupStatus/updated ×2   (codex_apps, openaiDeveloperDocs: starting)
SEND turn/start                    → RECV response (turn id, status inProgress)
RECV thread/status/changed           (idle → active)
RECV turn/started
RECV mcpServer/startupStatus/updated ×2   (→ ready)
RECV hook/started / hook/completed        (user-prompt-submit hook, ~/.codex/hooks)
RECV item/started + item/completed        (userMessage — our own input echoed as an item)
RECV item/started + item/completed        (reasoning — empty summary/content by default)
RECV item/started                         (agentMessage, phase "final_answer")
RECV item/agentMessage/delta ×5           (streaming text)
RECV item/completed                       (agentMessage, full text)
RECV thread/tokenUsage/updated            (totals + modelContextWindow)
RECV account/rateLimits/updated
RECV hook/started / hook/completed        (stop hook)
RECV thread/status/changed                (active → idle)
RECV turn/completed                       (status, startedAt/completedAt, durationMs)
```

## Findings that shape the inspector

1. **The rollout path comes to us.** `thread/start`'s response (and
   `thread/started`) carries `thread.path` —
   `~/.codex/sessions/2026/07/05/rollout-…jsonl`. The Disk panel needs zero
   matching heuristics: tail exactly that file. (Contrast legacy-client's
   window→rollout matcher.)
2. **Responses Input panel is mostly in the `thread/start` response**:
   `instructionSources` (`~/.codex/AGENTS.md`, repo `AGENTS.md`), effective
   `model` / `reasoningEffort` / `serviceTier`, `approvalPolicy`,
   `approvalsReviewer`, `sandbox` (`{type: readOnly, networkAccess: false}`),
   `activePermissionProfile`, `multiAgentMode`, `runtimeWorkspaceRoots`.
   Deeper content (tool registry, skills) has dedicated calls: `skills/list`,
   `mcpServerStatus/list`, `model/list`, `config/read`, `thread/read`.
3. **Context gauge is free**: `thread/tokenUsage/updated` gives per-turn and
   total token splits (input/cached/output/reasoning) plus
   `modelContextWindow` (258400 here) — no rollout parsing needed live.
4. **Everything is an item** with `item/started` → deltas → `item/completed`:
   even the user message and reasoning are items. Item ids are provider ids
   (`msg_…`, `rs_…`). The event-feed panel can be a uniform item-card stream.
5. **Turns are first-class**: `turn/start` responds immediately (status
   `inProgress`); completion arrives as `turn/completed` with timing
   (`durationMs: 2940`). `thread/status/changed` brackets activity.
6. **Hooks are visible on the wire** (`hook/started`/`hook/completed` with run
   ids like `user-prompt-submit:0:/home/user/.codex/hooks/…`) — an inspector
   panel we get for free that even the rollout doesn't show as clearly.
7. **Session provenance quirk**: app-server threads record `source: "vscode"`
   in thread metadata — worth knowing when correlating with `state_5.sqlite`.
8. **Server→client requests exist but didn't fire** (approvals, tool user
   input, auth refresh) — expected under `approvalPolicy: "never"` +
   `sandbox: "read-only"`. Inspector must still answer them or a turn wedges.
9. **Unsolicited pre-handshake traffic**: `remoteControl/status/changed`
   arrived before we sent `initialized`; the client must tolerate
   notifications at any time.
