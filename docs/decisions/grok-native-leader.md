# Grok uses its native leader

Nano connects to Grok Build through ACP JSON-RPC 2.0. Each live Nano session
owns a small `grok agent --leader stdio` proxy. The native Grok leader owns the
actual Grok sessions and prompts.

This is the restart boundary. Stopping or rebuilding Nano closes the proxy,
but it does not stop the leader. A new Nano process reconnects and calls
`session/load`. Grok replays the session, continues an in-flight prompt, and
re-issues a permission request that was waiting when Nano disconnected.

## Why this topology

Read-only probes against Grok 0.2.118 verified four cases:

- Direct stdio died with its client and lost the unfinished turn.
- The native leader completed a turn after its stdio client died.
- A new client loaded that session mid-turn, received replay plus live output,
  and observed normal terminal evidence.
- A parked permission request survived the disconnect and could be answered
  after `session/load`.

A Nano-owned bridge would duplicate working Grok behavior and add another
protocol and persistence layer. It is not part of this design.

## Fixed V1 choices

- The leader uses its short default socket, `~/.grok/leader.sock`. macOS Unix
  sockets fail when a custom path is too long.
- The leader sandbox is `off`. Grok applies sandboxing to the whole leader, so
  a shared leader cannot honestly offer a per-session sandbox selector.
- Nano advertises an empty ACP client-capabilities object. Advertising terminal
  or filesystem capabilities delegates those effects to Nano, which does not
  implement them.
- Grok keeps its normal permission system. On `session/new`, Nano can select
  the documented per-session `auto` or `bypassPermissions` posture through
  ACP `_meta.autoMode` / `_meta.yoloMode`; omission inherits Grok's normal
  configuration. Nano renders any resulting `session/request_permission`
  through the existing pending-interaction API.
- The native leader resolves reasoning effort when it handles `session/new`.
  Nano therefore sends the requested value as ACP `_meta.reasoningEffort`,
  then reads the selected `mode` from `_meta["x.ai/sessionConfig"]`. A new
  session does not become ready if that provider-selected mode is missing or
  differs from the request. The selected mode is recorded as canonical
  provider-effective metadata; the model catalog default and `summary.json`
  are not substitutes for this startup signal.
- The launchd leader disables auto-update. A Grok upgrade requires a deliberate
  leader restart when no hosted Grok turns are active.
- Every Nano-owned `agent stdio` client also sets
  `GROK_DISABLE_AUTOUPDATER=1`. This includes the model-catalog probe. A short
  client must never replace the shared binary or restart the leader as a side
  effect of connecting.
- Provider-wide model and billing probes use the configured Nano cwd while it
  exists, then fall back to the user's home directory if a transient release
  checkout disappears. These probes do not create or load a session. Real
  session starts never fall back from their requested cwd.
- Grok 1.0.3 stamps replay notifications with `_meta.isReplay` and returns the
  actual in-flight prompt as `_meta["x.ai/runningPromptId"]` from
  `session/load`. Replay is historical projection input only; it never asserts
  present-tense activity, even when it arrives after the load response. The
  response alone restores a running turn. An absent running prompt clears and
  settles old unfinished execution records.
- Grok replay keeps the provider's `agentTimestampMs` on normalized live rows.
  Receipt time is only a fallback for updates that carry no provider time.
- Grok rewrites summary activity fields for reconnect hooks and housekeeping.
  Those fields remain metadata evidence, but they do not define sidebar
  recency. Once a transcript exists, the last primary user or agent message is
  the thread's operator-facing updated time.

## Persistence and history

Grok 0.2.118 stores sessions below
`~/.grok/sessions/<encoded-cwd>/<session-id>/`. `summary.json` supplies session
metadata and `updates.jsonl` supplies ACP-shaped transcript evidence. Older
sessions without `updates.jsonl` remain discoverable as metadata-only rows.
Discovery uses `updates.jsonl` as the single source when present so transcript
append checkpoints use its byte offsets; the metadata adapter reads the
sibling summary snapshot. Summary-only sessions checkpoint an empty transcript
at the summary size until an update stream appears. When that first update
stream appears, Nano migrates the derived source row in place so the thread ID
stays stable and the new physical generation rebuilds its transcript without a
false duplicate-identity conflict.

Nano never reads `auth.json`. Provider-owned `~/.grok` files are view-only.

Grok 1.0.3 stores each delegated worker as its own session directory, which
can sit below a different encoded working-directory root. The parent keeps
the durable relationship in
`<parent>/subagents/<child-session-id>/meta.json`, with both native session
ids. Nano uses that sidecar only to classify and join independently discovered
native sessions; it does not create a Nano child attachment or hierarchy file.
The parent ACP connection can multiplex child traffic. Nano preserves those
rows in the raw Event Feed, but only the attached parent session may change
the parent transcript, runtime, delivery state, hosted operation, or parked
permission. Parent-scoped `subagent_spawned`, `subagent_progress`, and
`subagent_finished` records provide compact coordination evidence; each
child's detailed transcript remains its own native `updates.jsonl` projection.

## Experimental compatibility matrix

The public Grok provider is experimental. Model discovery and startup are
verified against Grok Build `1.0.40`; the broader replay, interaction, and
lineage rows retain their `1.0.3` evidence. A later Grok upgrade requires a
focused compatibility pass before broadening this matrix.

| Capability | Status | Current limit |
|---|---|---|
| Start, prompt, and resume | Supported | One active prompt per Nano attachment |
| Nano restart during a prompt | Supported | `session/load` must report `x.ai/runningPromptId`; an absent id clears an orphaned replay |
| Native tool permissions | Supported | New sessions accept `auto` or `bypassPermissions`; other modes remain file/CLI scoped |
| Interrupt | Supported | Uses ACP `session/cancel` |
| Model and effort discovery | Supported (1.0.40) | Grok 4.7 and Grok 4.7 Fast: `xhigh`, `high`, `medium`, `low`; Grok 4.6 and 4.5 remain available; read from ACP initialization metadata |
| Live messages, reasoning, and tools | Supported | Unknown or oversized updates remain visible as unclassified evidence |
| Explorer metadata and transcript | Supported | `summary.json` plus `updates.jsonl`; provider files stay read-only |
| Sandbox | Fixed `off` | Grok applies sandboxing at leader scope, not per session |
| Billing / usage | Supported | `_x.ai/billing` is an unstable ACP extension; Nano opens a short-lived leader client only, and JSON-RPC `-32601` is the upgrade guard. The literal may not appear in the binary. |
| Plan approval (`exit_plan_mode`) | Supported | Parked as a `toolPermission` interaction; see reverse-request contract below |
| Agent questions (`ask_user_question`) | Supported | Parked as a `userInput` interaction; answers keyed by question text |
| Plan updates | Unverified | No plan rows appeared in captured 0.2.118 sessions |
| Subagent lineage and coordination | Supported | Parent `subagents/<child>/meta.json` joins separate native child sessions; typed parent coordination records do not copy child transcript bodies |
| `grok inspect --json` resolved context | Deferred | Under the Hood shows ACP and durable evidence only |
| General elicitation beyond permissions | Deferred | No verified native method in the tested build |

## Mid-turn reverse requests (`_x.ai/exit_plan_mode`, `_x.ai/ask_user_question`)

Verified against leader `1.0.3` on 2026-08-13 by live probing (there is no
documentation; the shapes below were confirmed by driving a real leader and
reading the tool results). Both are JSON-RPC requests the leader sends to its
ACP client mid-turn and then waits on. **Refusing either by name — Nano's
behavior before 2026-08-13 — is read as "client disconnected mid-approval":
the leader cancels the whole turn** (`cancellation_category:
permission_cancelled`) and tells the model the client dropped. That killed a
hosted session on every `exit_plan_mode`. Nano now parks both as pending
interactions.

`_x.ai/exit_plan_mode` — request `{sessionId, toolCallId, planContent}`.
Response is `ExitPlanModeExtResponse`:

- `{"outcome": "approved"}` — exit plan mode, start implementing.
- `{"outcome": "abandoned"}` — abandon the plan, plan mode off.
- Anything else — including a body the leader cannot parse — silently means
  "revise": plan mode stays active and the optional `feedback` string is
  echoed to the model as the user's revision notes. This fail-soft is why a
  wrong response shape never errors; only live probing distinguishes the
  accepted vocabulary.

`_x.ai/ask_user_question` — request `{sessionId, toolCallId, questions:
[{question, options: [{label, description}], multiSelect}], mode}`. Response
is parsed strictly (unlike exit_plan_mode) and must be
`{"outcome": "accepted", "answers": {"<question text>": ["<label>", …]}}` —
answers are keyed by the question's own text, the same dialect as Claude's
AskUserQuestion. Empty `answers` reads as the user not answering.

On a client disconnect with the request unanswered, the leader re-parks the
approval and replays it when a client reattaches (`session/load`), so an
unanswered approval survives a Nano restart.
