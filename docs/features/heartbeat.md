# Heartbeat

Heartbeat is Nano's optional wall-clock wake service. It is off unless Nano
starts with `-heartbeat`, and it sends nothing until a heartbeat is attached to
a hosted thread with recorded actor and authority evidence.

The scheduler, store, pre-ping runner, and embedded template live in
`internal/features/heartbeat`. Root files only adapt startup, HTTP, and thread
lifecycle events.

## Scheduling and safety

Cadence is measured in whole minutes, with a floor of one minute. Nano checks
only the current wall-clock minute and never catches up for time missed while
it was stopped. Each due-minute decision is idempotent.

Before a wake, heartbeat reads the target through `internal/interact`. Busy
threads and threads waiting on the operator are skipped. Prompt submission also
sets `requireIdle`, so a race cannot queue or interrupt a running turn. A gone
target makes the attachment pause itself with an audited reason.

The wake body is exactly the attachment prompt or the pre-ping replacement.
Nano then appends one `[from nano heartbeat]` footer line. Heartbeat never reads
a transcript and recognizes no reply-text pause token.

Pause and resume are explicit API actions. Both require an actor and a reason;
any hosted agent may manage any attachment. Every mutation is audited. A paused
attachment stays quiet unless its pre-ping reports that something arrived. That
wake includes the pause reason, what arrived, and an instruction to restate the
outstanding wait.

## Backoff and global budget

After ten consecutive wakes without a newer human message or explicit resume,
the effective cadence has a two-minute floor. After twenty, it has a five-minute
floor. A human message or resume resets the count. A configured cadence slower
than those floors remains unchanged.

`-heartbeat-max-wakes-per-hour` bounds all admitted wakes in one rolling hour.
The default is 60. The operator may change the startup setting for the fleet.

## Pre-ping templates

Pre-ping scripts are read-only through the API. Source is kept in repository
templates and may be edited only by Operator through the reviewed Git path. An
attachment request selects a `templateId`; Nano stores an immutable copy for
that attachment. Raw source, shared script references, and API revisions are
not accepted.

On upgrade, a legacy custom script is retained as audit evidence but can no
longer execute. Its attachment receives a private copy of `inbox_match`, stays
paused with its existing reason, and reports `prePingSuspended:true`. The
template does not run until an explicit resume clears that suspension.

The retained `inbox_match` template peeks at unread Nano mail without marking it
seen. It wakes on a configured sender and/or reference, or on any unread
message when neither is configured. It pages through at most 5,000 unread
messages and fails with `inbox_scan_limit` past that; a bad setting fails with
`settings_invalid`. Existing attachment copies keep their original source, so
an attachment gets this behavior only after it is updated with a fresh
`templateId` copy.

A pre-ping may return `wake`, `skip`, or `fail`. A wake may replace the prompt.
A failure pauses the attachment and sends at most one Nano-written diagnostic.
The audit record keeps bounded result facts, never script output or prompt text.

## Usage and retention

Each admitted wake starts with usage `pending`. When its terminal feed row
arrives, heartbeat records provider-reported input, cached-input, and output
tokens. If the provider reports no usage, the wake shows `usage unavailable`.
Attachment reads sum the reported counters and show pending and unavailable
wake counts. No monetary estimate is invented.

Decision-only skip rows are retained for 30 days. Delivered wakes and failures
remain after that window. Attachments, configuration changes, pause/resume
reasons, and immutable script copies remain durable in `heartbeat.sqlite`.
Before each stage 6 schema upgrade, Nano writes one timestamped SQLite backup
beside that file. Schema version 2 retires affected closed-condition
attachments with an audit event. Its three old condition columns remain unused
for rollback; their removal requires a later operator decision. A legacy script
rewrite also records its prior script id and revision in the attachment audit
event.

## API outline

- `GET|POST /api/v1/heartbeat/attachments`
- `GET|PATCH|DELETE /api/v1/heartbeat/attachments/{attachmentId}`
- `POST /api/v1/heartbeat/attachments/{attachmentId}/pause`
- `POST /api/v1/heartbeat/attachments/{attachmentId}/resume`
- `GET /api/v1/heartbeat/script-templates`
- `GET /api/v1/heartbeat/scripts[/{scriptId}]`
- `GET /api/v1/heartbeat/log`

The OpenAPI document is the exact request and response contract.
