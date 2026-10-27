
## What

An agent appends a pin to its own hosted thread to mark a conversational point
it wants to return to. The API is under `/api/v1/hosted-threads/{threadRef}/pins`:

- `POST` appends `{text, commentary?, itemId?, creator?, creatorThreadRef?, clientActionId?}`.
  Without `itemId` the pin anchors the thread's latest indexed user or agent
  message ("pin this"). With `itemId` it anchors that exact message ("pin your
  earlier analysis"); `GET .../pins/candidates` lists recent messages with ids.
  A future UI message action passes the selected message's id the same way.
- `GET` lists newest first with a `before` cursor. Agents read backward until
  a pin names the topic.
- `GET .../pins/{pinId}/context?turns=N` (default 10, max 50) returns the
  anchored message plus the N whole turns before it, the anchor's own turn cut
  at the anchor, stable turn and item ids, and `hasEarlierHistory`.
- `GET /api/v1/pins/monitor` reports accumulation, list scan depth, read rank,
  context reads, never-read pins, and broken anchors.

Storage is `~/.nanod/thread-pins.sqlite` (flag `-thread-pins-db`). A pin
row holds the hosted thread id, the anchor (provider, native thread id,
canonical `nit_` item id, turn id, role, time, 280-byte preview), text,
commentary, creator, and creation time. The context URL is generated at read
time from the anchor; no absolute URL is stored.

## Why

- **Informal bookmarks.** Pins mark a conversation without task state. There is no
  approval, status, resolution, expiry, pruning, or lint. Append-only keeps
  the agent workflow to one call.
- **Sibling SQLite, not `history.sqlite`.** The catalog is delete-and-rebuild
  derived data; pins are agent-written truth, the same class as stars.
  Pins avoid `nano_threads.sqlite` so they do not collide with schema work there.
- **Canonical item ids.** `nit_` ids hash provider, native thread id, and the
  item's source key, so they survive a catalog rebuild. Older
  projection-scoped ids resolve through `conversation_item_aliases`.
- **Hosted-thread scope.** A pin list covers pins written under the hosted
  id plus pins anchored in any native id that hosted thread has claimed
  (`hosted_thread_native_ids`). Provider-native replacement or reattachment
  therefore never strands a pin.
- **No hidden context.** Pins are never injected into a turn. An agent sees
  them only through its own API calls, which stay in its native transcript.
- **Broken anchors are reported, not repaired.** If a rebuild no longer
  contains the anchored message, context returns 404 `pin_anchor_missing`
  with the unchanged pin in `error.details.pin`, and the monitor lists it.
- **Observational monitor.** Read counters live in separate tables
  (`thread_pin_context_reads`, `thread_pin_list_reads`). The monitor never
  writes pins. Real usage should guide any later refinement.

## Limits

- The context byte budget is the transcript budget (256 KiB). Over budget,
  full bodies fall back to previews, then the oldest turns drop
  (`trimmedForSize`), then earlier messages in the anchor turn.
- "Latest" means latest *indexed* message. The projection normally trails the
  live stream by seconds.
- Back up the pins database separately from the derived history catalog.
