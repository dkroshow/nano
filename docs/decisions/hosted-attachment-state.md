## What

Every hosted thread Nano can resume has one row in `hosted_attachments`
inside `nano_threads.sqlite` (schema 25). The row holds the thread's resume
data (provider, native id, Claude binary and arguments, shim session id,
turn anchor, staged title) and one explicit state:

| State | Meaning | Set by |
| --- | --- | --- |
| `attached` | A durable session is live, or was when Nano stopped. | The session's handshake; a connection that comes back. |
| `resumable` | No session is live; Nano can bring the thread back. | A session that ends without a close; Nano's start, for every row still `attached` once it has read them. |
| `finished` | Closed for good. The row keeps `finished_at` and `finished_by`. | An explicit close, or a cleanup agent closing through `POST /api/v1/hosted-threads/{ref}/close` with `actorThreadRef`. |

Boot re-attach and the sidebar read only `attached` and `resumable` rows: the
store loads them with one query on a partial index, and the sidebar's catalog
read joins the same rows in SQL instead of sending their ids.
`GET /api/v1/hosted-thread-states` lists them for a cleanup agent.

## Why

- `attachments.json` only grew. Agents rarely close their children, so it
  reached about a thousand records, each restart tried to resume all of them,
  every attach rewrote the whole file, and a sidebar query built from it
  passed SQLite's expression limit and took the sidebar down.
- The hosted-thread record already lived in `nano_threads.sqlite`; keeping
  "can Nano bring this back" beside it leaves one place to ask.
- Nano never changes a state on a timer or by guessing that a thread is done.
  Deciding that an idle thread is finished is a cleanup agent's job; a
  finished thread comes back only through an explicit resume.
- Rows are never deleted. The one case Nano finishes a row itself is a Claude
  handle that provably cannot resume: a never-prompted child whose process
  is gone, or a predecessor that a non-durable successor replaced. Its
  `finished_by` is `nano`. Leaving it `resumable` would offer a resume that
  fails, or one that forks the conversation at the next boot.

## Migration

The schema-25 step imports `attachments.json` once, on a private copy of the
store like every schema step. A thread whose execution status was still
`ready` when Nano stopped becomes `attached`; every other record becomes
`resumable`; none becomes `finished`. The file is left on disk unchanged and
is never read or written again.
