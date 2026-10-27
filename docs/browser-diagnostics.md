# Browser diagnostics

Nano keeps a bounded, content-free browser flight recorder for stale panes,
lost optimistic prompts, and projection-stream failures that a hard refresh
would otherwise erase.

## Thread-open timing

`GET /api/v1/browser-diagnostics?kind=thread_open` returns one content-free
record per visible thread open. `actionId` identifies the open; `trigger` is
`sidebar`, `tab`, or `load`. A load starts at browser navigation, including URL
resolution. Sidebar and tab opens start before staging the selected pane.

`firstRowsMs` measures the first visible message rows; `newestRowsMs` measures
the last eligible row of that view's ready initial tail. Bounded previews count.
`warm: true` marks a pane that first showed the thread's warm rows (the warm
stream's saved rows of a thread not on screen); its `newestRowsMs` is still
its own first tail.
Two animation frames allow a paint opportunity, with viewport and document
visibility checked again; these are browser paint estimates, not pixel capture.

`outcome` is `ready`, `empty`, `error`, `preserved` (an older reading position),
`abandoned` (replaced, hidden or closed), or `timeout` after 30 seconds. Missing
measurements are omitted, never replaced by zero. `itemCount` and
`visibleItemCount` count mounted and viewport-visible rows. Observers stop when
the open ends; streaming updates do not create another record.

For completed initial HTTP history reads matching the displayed thread and
profile, `historyMs` is the maximum Resource Timing
`responseEnd - requestStart`; `historyBytes` sums `encodedBodySize`. Older
paging, other profiles, body/detail reads and unrelated requests are excluded.
File-only SSE tails have no completed resource entry, so these fields are
absent. They do not describe server execution time. Server-only route timing
is in `GET /api/v1/monitor` under `requests`.

## Read a recent trace

```sh
curl -s 'http://localhost:7690/api/v1/browser-diagnostics?limit=500' | jq
```

Filter by the native provider thread id when the operator names the affected
thread:

```sh
curl -s 'http://localhost:7690/api/v1/browser-diagnostics?limit=500&threadId=<native-id>' | jq
```

`tabId` and exact `kind` are also accepted query filters. Results are returned
in chronological order.

The useful incident chain is normally:

1. `navigation.request` → `navigation.commit`
2. `pane.render` for the selected thread/session/store
3. `projection.connect` → `projection.open`
4. `projection.message` cursor progress, or the last cursor before a stall
5. `projection.error` / `projection.snapshot.*` / a replacement connection
6. `send.echo` → `send.post.*` → `send.confirm` for submission incidents
7. `send.visibility` (`pending` then `confirmed`) proves whether the exact
   action-bearing prompt row was found and anchored inside the transcript
   viewport; `missing` or `hidden` distinguishes presentation failure from a
   delivery failure
8. `transcript.validation` compares the ordered durable, live, and pending rows
   the component intended to render with the rows its browser actually
   mounted. Its counts distinguish missing, extra, duplicate raw render keys,
   duplicate semantic message identities, blank, wrong-type, and out-of-order
   rows without recording transcript content.
9. `js.error` records content-free classifications such as `duplicate_key`
   for global errors and unhandled promise rejections; error messages and
   stacks never leave the browser.
10. `sidebar.layout` measures the compact rail's mounted geometry after each
    meaningful DOM, scroll, or size change. It records row, movement, reorder,
    insertion, removal, working, star, detached, scroll, and viewport counts.
    `sidebar.row.move` records up to the 24 largest changes per layout sequence:
    immutable row identity, before/after index and content-top position, pixel
    movement, visibility, lifecycle status, and the ordering band that placed
    the row. These events distinguish data reordering from scroll restoration
    or row-height changes without retaining titles or other content.

## Agent validation of the visible transcript and sidebar

Every visible transcript pane and the left sidebar automatically compare their
intended row sequence with the mounted browser DOM and publish content-free
snapshots:

```sh
curl -s 'http://127.0.0.1:7690/api/v1/browser-validations?hostedThreadId=<nht_id>' | jq
```

Visible panes refresh their snapshot every 15 seconds even when the transcript
has not changed. Nano reports `fresh: true` only when `pageVisible` was true and
the snapshot arrived within 45 seconds. Agents should require `fresh: true` and check
`checkedAt`, `build`, `tabId`, and `paneId` before treating the result as
evidence for a new fix. `status: "match"` means only that browser's
ordered durable, live, and pending rows match the component inputs. It does
not compare those inputs with saved SQLite history. A mismatch
returns both `expectedRows` and `mountedRows`; a missing result means no visible
browser pane has reported the requested thread since the current Nano boot.

The response has `validations` for the latest checkpoint per browser surface,
`history` for meaningful changes, and `duplicateIncidents` for durable duplicate
observations. Heartbeats replace the checkpoint but do not append history.
History keeps at most 20 changes per pane and surface, prefers retaining
mismatches, and the entire validation store is capped at 4 MiB in memory.
Duplicate incidents are separate durable records in `nano_threads.sqlite`; they
survive Nano restart. Filters include `hostedThreadId`, native `threadId`,
`paneId`, `surface` (`transcript` or `sidebar`), and `status`. The older
`/api/v1/transcript-validations` route remains a compatibility alias. This is
the primary troubleshooting surface for agents.

Nano derives duplicate sets again on receipt instead of trusting browser
counts. It hashes the sorted duplicate raw render keys and semantic message IDs
before persistence. A stable incident fingerprint combines those class
fingerprints with the thread identity; build, pane, and observation time remain
evidence but do not split one incident into repeated alerts. No raw row identity
or message text is persisted in the incident or its inbox cue.

For optional human inspection, turn on **Validate** in a thread's transcript
bar (or its mobile thread menu). The setting persists in that browser and
shows:

- **Render matches** when the intended and mounted row sequences agree.
- **Render mismatch** with category counts when they differ.
- **Compare rows**, an expandable local list of row types, ownership source,
  and shortened identities under “Should show” and “Browser shows.”

The comparison uses the same durable-history, live-window, and pending-prompt
inputs as the real transcript component; it is not a second transcript
implementation. Row identity, type, order, and whether content is blank are
published, but prompt text, assistant text, reasoning, tool output, and file
content are not.

## On-demand saved transcript audit

Use **Check transcript** for an independent comparison with a pinned, bounded
SQLite window. Only user and assistant messages are checked. Tools and reasoning
are outside the audit scope. The result says **Displayed window** because a partial mounted range is
not proof about the whole thread.

An agent first reads `/api/v1/browser-validations` to choose the exact current
`tabId`, `pageId`, and `paneId`, then creates and polls an audit:

```sh
curl -s -X POST http://127.0.0.1:7690/api/v1/transcript-audits \
  -H 'content-type: application/json' \
  -d '{"clientActionId":"audit-example-1","threadRef":"<provider-native-or-catalog-id>","tabId":"<tab>","pageId":"<page>","paneId":"<pane>","scopes":["messages"]}'

curl -s http://127.0.0.1:7690/api/v1/transcript-audits/<audit-id>
```

The create call pins the saved projection and snapshot head. A content-free SSE
request asks only the named tab and pane to capture mounted content. The pane
reads the pinned reference with `Cache-Control: private, no-store`, normalizes
rendered Markdown, line endings, trailing whitespace, and DOM-only template
spacing, then publishes identities, hashes, lengths, counts, and bounded
differences. Blank lines and intra-line code whitespace remain substantive.
Transcript bodies are neither logged nor retained in the audit store.

The pane also declares both mounted-window boundaries and any interior seam
that its own paging or 40-row live window did not mount. It names exact omitted
live rows when it has them; an unloaded live page supplies a bounded sequence
range only while the store still reports that page missing. Sequence gaps alone
are never omission evidence. The conversation component mounts through its
actual tail. Its top is the transcript start, explicitly older unmounted rows,
or unknown during a pending/error load, including the All-view history twin.
The reference request is open-ended only at a declared start or tail. An
unknown boundary cannot produce `match`.

`match`, `mismatch`, and `inconclusive` are distinct. Missing or wrong panes,
hidden/collapsed content, stale or wrong builds, unreliable identity anchors,
row or byte caps, a changed saved projection, and an unindexed live tail cannot pass.
A stable mismatch can still list live-tail or hidden-content exclusions.
Repeated equal text is not a duplicate when its stable saved identities differ.
Difference totals cover every finding even though only the first 20 details are
returned. Exclusions aggregate by scope and reason before the server's bounded
result validation. Rows below a row- or byte-capped reference start are
unverified, not extra: same-projection durable rows and confirmed live rows are
excluded there. A known different projection remains a contradiction, and
every identity, order, or content defect inside the returned window remains a
mismatch. A missing saved suffix may be paired with an explicitly awaiting,
contiguous live suffix only one-for-one when its role and rendered presentation
agree. The store watermark helps identify that transient suffix, but the
contiguous shape, cardinality, and presentation checks are what bound the
uncertainty. The result stays `inconclusive`, and stable omissions elsewhere
remain errors.

The completion payload reports the pane's own thread reference, independently
of the canonical catalog id in the audit request. Nano resolves that reference
again and requires the same catalog thread. The supported pane references are
the catalog id and provider-native id, matching the catalog resolver. Hosted and
Nano session ids are not accepted as transcript-audit thread references.

Assistant reference text passes through the same Markdown renderer and
sanitizer as the visible pane before presentation comparison. The browser read
still comes from the mounted DOM, and identity and ordering are independent of
that shared path. This audit therefore proves that saved normalized input,
through the shared renderer, matches what the pane mounted. It cannot detect a
renderer or sanitizer defect that changes both sides in exactly the same way.

Compare the last browser `epoch` and `revision` with the current hosted-thread
conversation snapshot. `pane.render` identifies whether the visible pane was
actually bound to the same store whose stream advanced.

## Feedback notes

Each live thread's view bar (the thread menu on a phone) has a **Feedback**
button beside **Repair transcript**; a read-only historical pane shows it at its
top left. The operator types a note and presses Save, and the pane confirms
"Feedback saved at <time>" in local time.

Save records a `feedback.save` event, then sends the tab's whole retained ring
through the ordinary `POST /api/v1/browser-diagnostics` path, including events
Nano already acknowledged: Nano's shared ring may have evicted them, and its
store skips any it still holds. If Nano restarts between batches, the whole
ring is sent again under the new boot, and the note names that boot: Nano
refuses a note whose boot it no longer has (`evidence_lost`). If any batch
fails, the note is not saved; the pane says so and keeps the text for a retry. Nano then copies that tab's events
for the thread, plus its thread-less page events, in the tab's order, and keeps
them with the note in `feedback.sqlite` (`-feedback-db`), so a later restart
cannot erase them. The note also records the thread (hosted id, native id,
display name, provider), the serving build stamp and bundle and the bundle the
tab loaded, the view, and the pane state: working or idle, pending prompt
count, and the last saved row id. The note is the only free text; the copied
events keep the content-free schema below.

```sh
curl -s 'http://127.0.0.1:7690/api/v1/feedback?limit=20' | jq        # newest first, without events
curl -s 'http://127.0.0.1:7690/api/v1/feedback?threadId=<id>' | jq   # hosted or native id
curl -s 'http://127.0.0.1:7690/api/v1/feedback/<feedback-id>' | jq   # one note with its events
```

Saving a note notifies no agent: no prompt and no inbox mail. Agents read
notes through the API above.

## Retention and restart behavior

- Each tab keeps its newest 600 events in `sessionStorage`. A normal or hard
  refresh preserves that ring.
- Nano keeps the newest 10,000 uploaded events in memory.
- Every server boot has a new diagnostic `bootId`. When a tab observes a new
  boot id, it replays its retained ring, so a Nano restart does not erase the
  browser evidence.
- Uploads are batched, deduplicated by `eventId`, and retry after connection
  loss.
- Duplicate incidents and their inbox relay watermark live in `nano_threads.sqlite`,
  independent of the in-memory validation and diagnostic rings.
- Transcript audit requests and content-free results live only in a bounded
  in-memory store. Pending requests expire after 30 seconds.

## Privacy boundary

The schema accepts identities, state flags, cursor numbers, operation names,
HTTP status, and the loaded bundle hash. It has no fields for prompt text,
assistant responses, reasoning, tool output, command output, paths, or file
contents. Both the browser serializer and Go handler enforce this narrow
schema.
