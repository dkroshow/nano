
## What

Operator-starred threads are stored in `~/.nanod/thread-stars.sqlite`
(overridable with `-thread-stars-db`), table
`thread_stars(provider, native_thread_id, starred_at)`. Listings overlay
`starred` at read time. `GET /api/v1/sidebar/threads` overlays the current
set so a hard refresh does not wait for a cache write, and starred threads
are rescued into the active sidebar even when they sit outside the
eligibility window. A successful star/unstar also nudges open browsers through
the existing runtime SSE (`refresh: true`) so every device talking to this
Nano process converges without a reload. There is one operator and one
SQLite file; stars are not per-browser.

## Why

- `history.sqlite` remains delete-and-rebuild derived data. Stars are
  operator truth, the same class as names. Putting them in the catalog
  file would lose pins on rebuild.
- Thread names already answered "later annotations (tags, notes, pins)
  join this file or get siblings." Stars take the sibling-SQLite fork
  because this feature asked for SQLite, not another JSONL.
- Key is `(provider, native_thread_id)`, never `nth_*`, matching names:
  catalog ids die with the DB.
