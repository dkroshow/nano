# Operator mail surface

Status: accepted. See [Mail](../features/mail.md) for storage and API details.

## Decision

The operator gets a UI over the durable agent inbox, and a bounded participant role.
The inbox remains the agents' mailbox: recipients alone read-mark and
disposition their mail. The UI therefore always reads with `peek=true` — a
browser view never marks mail seen on an agent's behalf.

The operator can:

- **See everything.** Per-thread inbox in the thread pop-up; a cross-thread
  listing (`GET /api/v1/agent-messages`, read-only, newest first, aggregate
  counts) behind a Mail button in the sidebar header.
- **Send mail** as a first-class `operator` sender (`operator:true` instead of
  `senderThreadRef`; sender kind/origin `operator`, name "Operator"). Dispositions
  of operator mail produce **no** feedback notice — the operator watches status
  in the UI and has no hosted inbox.
- **Edit only their own messages, only while unread**
  (`POST /api/v1/messages/{id}/edit`). Once the recipient has read a message it
  is frozen: dispositions and audits refer to the words the agent saw, and the
  correction path is a follow-up message. Every edit is preserved as an
  `edited` event carrying the previous content. No deletion, ever.

## UI shape

The sidebar badge shows unread/total. Yellow indicates unread mail or mail
that was seen but not dispositioned; otherwise the badge is dimmed.
The thread popup includes its inbox and compose box. The sidebar Mail button
opens the cross-thread list with status filtering.

## Freshness

Rides the existing pipeline. Catalog rows carry a read-time `mail` counts
overlay (like Assignment/Starred — never stored, never part of
catalog_version), and mail mutations emit a `mail` named event on the
thread-index SSE stream carrying each changed thread's complete counts plus
the global summary. Deltas are last-value-wins and not replayed; a stream
reconnect clears live counts and the window reload re-stamps them.

## Storage

The feature owns `mail.sqlite`; see [Mail](../features/mail.md).
