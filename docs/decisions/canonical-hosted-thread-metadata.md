
## Decision

Nano's durable control-plane database is `~/.nanod/nano_threads.sqlite`.
Every hosted Codex, Claude, or Grok thread has one row keyed by its immutable
`hostedThreadId`. The same database owns hosted execution sessions, turn
operations, current and historical provider-native identities, attachment
generations, immutable metadata evidence, and the materialized current
metadata projection.

The database also owns the append-only permission audit trail. Each permission
request and resolution is tied to its hosted thread, current Nano attachment,
provider request identity, and permission mode. It stores a safe structural
summary and request hash rather than command text or the raw request body.

For each metadata field, current state keeps desired operator intent separate
from provider-confirmed effective state. Both sides point to the immutable
event that established them. A pending desired value may be displayed as
pending; missing evidence stays unknown and is never converted to a product
default. Provider evidence can settle a request only when Nano can connect it
to that request or prove that it is fresh for the current attachment
generation.

Provider evidence identity includes its attachment generation, protocol seam,
semantic value, freshness class, and request causation. A replayed observation
and a live observation of the same value are distinct evidence occurrences;
an exact retry within either class is idempotent. This prevents a provider's
replay/live handoff from being misclassified as corrupt metadata.

All hosted-thread API responses, session summaries, the bottom information
bar, live controls, and hosted catalog overlays read this projection. The
browser may cache it by the hosted revision, but it does not reconstruct
posture from transcript data, attachment fields, or local defaults.

## Store boundaries

- `nano_threads.sqlite` is non-rebuildable control state. It is separate from the
  rebuildable provider-history database, `history.sqlite`.
- Permission request and resolution evidence stays in `nano_threads.sqlite`. It
  shares Nano's serialized SQLite writer instead of adding another database or
  a direct writer from each agent runtime. A bounded read-only WAL pool serves
  browser-facing projections. This keeps a local operator decision such as
  Codex `/fast` independent from full session-list reads while preserving one
  control-plane writer and its existing ordering.
- Legacy stores are migration inputs rather than runtime writers.
  Hosted sessions, turn operations, and metadata live only in `nano_threads.sqlite`.
- `attachments.json` retains provider transport and reconnection data only.
  Working directory, model, and Codex service tier are removed after import.
- `thread-names.json` remains the name truth selected by the 2026-07-21
  decision. Canonical metadata joins its value and actual provenance at read
  time rather than copying it into the event log.

Normal close and detach retain the hosted record and evidence. Explicit
hosted-thread destroy retains its documented destructive meaning and cascades
the control rows.

## Migration and rollback

The posture and execution JSON migrations completed in August 2026 and were
retired during the store migration. Attachment transport metadata retains its separate
bounded migration. Canonical metadata failure remains fail-closed for hosted
mutations and explicit in API/UI reads. If a newer observation cannot commit
but the canonical record remains readable, Nano continues showing the last
committed values with a degraded warning and disables posture mutations.

## Operational contract

The event log has no automatic truncation. Semantic provider duplicates are
suppressed; operator decisions remain audit events. Startup diagnostics report
bounded event inventory, and a field reaching 256 events emits a runaway-write
warning without deleting evidence.

Public readers use Nano's versioned HTTP API. The current record is available
at `/api/v1/hosted-threads/{threadRef}/metadata`; immutable evidence is bounded,
filterable, and cursor-paginated under `/metadata/events`; revision hints are
available under `/metadata/changes`. Permission audit evidence is independently
cursor-paginated under `/permission-events`. Browsers and agents never open the
SQLite file directly.
