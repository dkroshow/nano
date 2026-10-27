# Targeted catalog updates over SSE

`GET /api/v1/thread-index/events` notifies browsers of catalog and runtime
changes. SSE is a change signal; HTTP remains the source for catalog rows.
Runtime deltas do not advance the catalog version. The stream has a heartbeat
and reconnect repair so a missed event cannot leave a stale browser forever.

## Catalog row repair

The version-only rule caused one active conversation to reload the complete
Thread Explorer on every catalog change. A 30-day window with more than 1,000
threads required five HTTP pages. The browser exposed each page as it arrived,
so sidebar counts visibly collapsed and grew during ordinary message flow.

Catalog events now distinguish two cases:

- An ordinary update names the affected Nano thread IDs. The browser fetches
  those rows and patches them into its current complete window.
- A structural change sets `refresh: true`. New, removed, archived, reparented,
  or otherwise uncertain rows use this path. Every reconnect also starts with
  a complete repair, which preserves the missed-event recovery guarantee.

The one-slot hub unions pending thread IDs when it conflates events. If the
bounded set overflows, it escalates to a complete repair. This keeps the stream
as a change signal rather than a second catalog. HTTP remains the row and
window data authority.

Complete window loads now build off-screen and replace the prior dataset once
all pages arrive. A failed refresh leaves the prior complete window visible.
This prevents counts and filters from observing a partial catalog.
