---
type: decision
category: architecture
summary: Nano must use thread/resume to re-subscribe a new app-server connection after restart; this is connection repair, not a model turn, and the current full-history response should seed only the bounded live tail
source: verified
confidence: 10
evidence_ref: Codex app-server manual and protocol schema on 2026-08-04; codex.go runCodexHandshake/codexResumeParams; supervised-app-server.md
applies_to: nano
status: active
superseded_by: null
task_id: null
created: 2026-08-04
---

# Codex re-attach is a per-connection subscription

## Finding

The supervised Codex app-server and a Nano process have separate lifetimes.
App-server can keep a thread and its active turn alive across a Nano restart,
but Nano's WebSocket and every subscription owned by that connection are gone.
A replacement WebSocket begins with no thread subscriptions.

The current app-server protocol exposes one way for the replacement connection
to join an existing thread's event stream: `thread/resume`. There is no
`thread/subscribe` method or transport-level connection-resumption token.
`thread/read`, `thread/turns/list`, and `thread/loaded/list` are passive reads;
none subscribes the calling connection to future turn or item notifications.

Nano therefore sends exactly one `thread/resume {threadId}` for each attached
Codex thread it restores after restart. It deliberately supplies no cwd, model,
sandbox, personality, or other override. The one exception is the thread's
service tier: without it, Codex resumes the thread at its own config's tier. This is logical re-attachment, not a
new model turn: Nano sends no `turn/start`, and `thread/resume` alone does not
advance `thread.updatedAt`. Routine Nano restarts therefore do not spend model
tokens for the conversation.

A newly created zero-turn thread is the one timing exception. Codex has not
written its rollout yet, so the boot resume can return `no rollout found` even
though the supervised daemon still owns the in-memory thread. Nano keeps that
thread routed and accepts its first `turn/start`, but acceptance alone does not
subscribe the restarted WebSocket. After recording the returned provider turn
identity, Nano must retry `thread/resume` until the rollout is available. Only
a successful shared resume transition may clear zero-turn recovery. This is
still connection repair rather than a second model turn, and a recovery error
cannot turn the already accepted prompt into a false refusal.

## Current protocol cost

The current installed protocol populates `thread.turns` in a successful
`thread/resume` response. Long threads can consequently transfer megabytes from
app-server to Nano even when the thread is already running and the only
essential operation is subscription repair. That is local serialization,
memory, and loopback traffic; it is not model-token usage.

Older Nano research notes mention experimental `excludeTurns` and
`initialTurnsPage` resume parameters. They are absent from the current
`ThreadResumeParams` schema and current app-server manual. Agents must not rely
on or send those parameters without a fresh capability check against the
installed app-server.

## Decision

Keep the existing `thread/resume` re-attachment. Do not replace it with
`thread/read`, a second resume, lazy history polling, or a Nano-owned Codex
connection broker.

On a successful resume, Nano uses the response it already receives to hydrate
the normalized live projection with the newest five terminal turns plus any
active turn. It then discards all older returned turns immediately. The oldest
retained live turn is whole: stripping the response while seaming SQLite in
the middle of a turn can leave a missing prefix.

This bounds retained Nano memory and repairs the live/durable seam. It does not
eliminate the transient app-server-to-Nano response, because the current
protocol does not offer subscription without history.

The resume response and live notifications may cross on the WebSocket. Nano
therefore treats the response as a baseline prefix and ordered-merges any
already-observed notifications over it before publishing one coherent
projection snapshot.

## Browser restart reconciliation

A Nano restart creates a new projection epoch and normally a new Nano session
id. Neither means that the native conversation changed. For the lifetime of a
browser tab, continuity is keyed by `(provider, nativeThreadId)`, not by the
Nano session id or by projection revision. Revision numbers are comparable
only within one epoch.

When the replacement epoch arrives for the same native thread, the browser
ordered-merges it into the longer tail already in page memory. Stable
`clientActionId` and native item ids join overlaps; provider fields update the
matched row; missed rows are inserted between overlap anchors; and older or
lossier browser-only rows remain visible. This is synchronization, not a
wholesale transcript replacement. The replacement epoch's runtime, turn, and
interaction state is authoritative and replaces the old epoch's state.

The send journal supplies explicit tombstones for refused or failed synthetic
prompts. Absence from a bounded or lossy resume payload is not deletion.
Changing native thread identity still hard-replaces the transcript. A future
explicit rollback/reset operation must carry its own hard-reset signal rather
than overloading a new epoch or missing rows.

Do not make idle threads lazy-attach as the primary optimization. That trades
boot traffic for click and send latency, conflicting with Nano's requirement
that opening an attached thread be immediate and reliable.

## Preferred protocol evolution

The clean app-server solution is either:

1. `thread/subscribe {threadId}` with no history payload; or
2. `thread/resume` with an advertised `excludeTurns` or bounded initial-turns
   option.

With either capability, Nano can request only the newest five completed turns
plus the active turn, using `thread/turns/list` when a separate history call is
needed. Any adoption must be capability-gated so older installed Codex binaries
retain today's safe resume behavior.

A persistent Codex connection broker would also avoid repeat resumes by owning
the app-server WebSocket across Nano restarts, but it would duplicate much of
the Claude shim: another daemon, subscription registry, sequence journal,
replay protocol, and failure boundary. Do not build it solely to remove this
loopback history transfer.

## Interacts

- `supervised-app-server.md`: the daemon preserves the thread runtime; this
  decision explains why a restarted Nano must still re-subscribe to it.
