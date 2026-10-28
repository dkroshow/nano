# Restart tracking

Every Nano process start records how long its restart took, per thread and in
total, so restart speed is measured rather than guessed. The code is
`restart_tracking.go`; the hooks sit where each step happens.

## Where it is

- `<data-dir>/restarts.jsonl` (default `~/.nanod/restarts.jsonl`), next to
  `deploys.jsonl`: one JSON line per process start. The join key with a
  deploy record is `stamp`.
- `GET /api/v1/restarts?limit=N` (default 5, max 50) returns the newest first,
  including this process's own while its boot is still settling
  (`complete: false`). Add `threads=1` for each thread's steps.
- `GET /api/v1/monitor` carries the newest 5 as `restarts`, without
  threads. Both read the end of the file only, through the same tail reader
  as the alert ledger.

The line is written when the boot loop is done, every thread is live, failed
or resting, and every live thread has its `attached` mark (at most 30 seconds
later: a Claude respawn goes live before it); with resting threads, a minute
after the last attach settles (below).
It is also written, as it stands, 15 minutes after start (`not_live_at_cap`)
or when Nano stops before that (`interrupted: true`). A crash during boot
loses that boot's record. If the line cannot be written, the API keeps
serving the record from memory and the log says why.

The record is content-free: ids, fixed codes and millisecond offsets from
`started_at` (the process start). No prompt text, titles, paths or error
text.

## Per restart

| Field | Meaning |
|---|---|
| `restart_id` | the boot id that browser diagnostics also use |
| `http_open_ms` | the port answers |
| `boot_loop_ms` | the boot loop has handed every queued thread to its provider |
| `visible_live_ms` | every sidebar-visible thread is live; null while one is pending or if one failed |
| `visible_settled_ms` | every sidebar-visible thread is live or failed |
| `all_live_ms`, `all_settled_ms` | the same for every queued thread |
| `threads_total = live + failed + pending + resting`, `visible` | counts |
| `by_provider.<p>.ms.<stage>` | n, p50, p95 and max of each stage below |

Sidebar-visible means every starred row, plus the first 20 top-level rows by
recency among the rows the active sidebar shows once the restart is done
(rows it shows already, and stored threads, which show once attached).
Browsers also group and filter rows locally, so this approximates one screen.

The boot loop queues the sidebar-visible threads, then hidden threads with
work not yet settled: a hosted operation, a queued prompt, or a provider turn
that started and has no outcome. Every other stored thread is left `resting`
and comes back when something needs it: a sidebar row or pane that shows it
(the browser's `prioritize`, which covers what the estimate misses, such as an
expanded subagent), or an agent's turn, operation or read that needs it live.
A resting thread counts as settled; one picked for an
attach before the line is written is pending again until it is live or
failed. While threads rest, the line waits until no attach has settled for a
minute, so the attaches browsers ask for right after the restart count.

## Per thread (`threads=1`)

`tier` (`starred`, `top_level`, `nested`, `unseen`) and `order` give the
thread's planned boot priority from the sidebar. Boot runs one queue per
provider, so `order` is the actual order within a provider, except for a
thread a request reached first: an agent's turn or read, or the operator
opening it (the browser's `POST /api/v1/hosted-threads/{threadRef}/prioritize`).
Providers run side by side.

Offsets, each set at most once: `picked_ms` (the boot loop or a request
reaches it), `lock_ms` (resume lock acquired), `registered_ms` (Nano's record
rebuilt from its stores and the session shell registered), `attached_ms` (the
agent reconnected: Claude shim attach or respawn, Codex `thread/resume`, Grok
session load), `live_ms` (the session's connection first became ready, which for a
re-attached thread waits until the catalog has caught up its saved file, so
`attached_ms` to `live_ms` is that wait) and `failed_ms` with
`reason` (the first failure). `state` is `pending`, `resting`, `live` or `failed`: the
last of live and failed wins, and the counts follow it, so a thread that went
live and then failed counts as failed. Records written before 2026-10-09 also
carry `restore_started_ms`, `restore_done_ms` and `restore_outcome`, from a
boot step that no longer exists.

Stages: `lock_wait` (picked to lock), `record` (lock to registered),
`reconnect` (registered to attached, including any wait for a provider
handshake slot), `registered_to_live` and
`to_live` (start to live).

Reason codes: `no_longer_stored`, `control_unavailable`, `spawn_error`,
`handshake_failed`, `closed`, `reconnect_failed`, `reconnect_parked`,
`exited`, `detached`, `not_live_at_cap`, `interrupted`.
