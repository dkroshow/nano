---
type: decision
category: architecture
summary: A thread's updatedAt is the timestamp of its last message, not the last write to its file
source: user-stated
confidence: 9
evidence_ref: 2026-08-07 session (the operator) — a thread whose last message was 90 minutes old showed "3 minutes ago" in the rail; the newer rows were a harness task notification with its queue and attachment rows. the operator: "I want the updated at timer to be the timestamp of the most recent message and not this background activity."
applies_to: nano
status: active
superseded_by: null
task_id: null
created: 2026-08-07
---

## What

`ThreadProjection.UpdatedAt` advances only on conversation: a prompt a person
sent, or the agent's reply. Every other row still contributes to `CreatedAt`
and to the projection's activity span, but no longer decides how recent a
thread looks.

Per provider:

- **Claude** — `user` and `assistant` rows. A `user` row is excluded when it
  carries an `origin` (harness notifications such as
  `{"kind":"task-notification"}`), when it is flagged `isMeta` (injected
  context), or when its content holds only `tool_result` blocks. Attachments,
  queue operations, permission-mode, titles, and system rows never advance it.
- **Codex** — `response_item` messages with role user or assistant, and
  `event_msg` rows of type `user_message` / `agent_message`. Developer-role
  messages and the synthetic prompts `isCodexSyntheticPrompt` already
  recognizes are excluded, as are token counts, reasoning, function calls,
  turn context, and world state.
- **Grok** — unchanged. Its summary document states the thread's own updated
  time; there is no row stream to classify.

A thread with no messages at all falls back to its newest activity timestamp,
so an aborted session still sorts and displays.

## Why

The rail's relative time is read as "when did anything happen in this
conversation." Providers write far more than conversation into a session file,
and some of it arrives long after the last turn — a background task reporting
in, a resumed session's attachments, codex's token-count telemetry. Sorting
and labelling by file activity made idle threads look alive, which is exactly
the signal the rail exists to give.

The `updatedAt` field already had a second consumer that had worked around
this: active ordering deliberately does not sort by `updatedAt` because it
"changes on every streamed event". That
workaround stays correct and is now less load-bearing.

## Alternatives rejected

- **Keep `updatedAt` and add a separate `lastMessageAt`.** Two nearly
  identical timestamps invites every new surface to pick the wrong one. The
  displayed meaning should be the stored meaning.
- **Filter in the UI.** The catalog sorts and pages by `updatedAt` in SQL; a
  browser-side correction cannot fix ordering or cursors.
- **Treat tool results as messages.** They are mechanical echoes; the
  assistant row that requested the tool already marks that the turn is moving.

## Consequences

Metadata parser versions invalidate older projections so re-indexed threads use conversation timestamps.
