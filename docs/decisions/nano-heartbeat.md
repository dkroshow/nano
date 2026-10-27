# Nano heartbeat

Date: 2026-09-12
Status: implemented behind a default-off flag; supersedes the 2026-08-26
reply-token and writable-script design

## Decision

Heartbeat is a wall-clock wake feature built as a client of
`internal/interact`. It does not inspect transcripts, provider sessions, or
private operation transitions. It reads the public hosted lifecycle state and
submits a system-origin prompt with `requireIdle`. Busy and awaiting-human
targets skip; the atomic submit check prevents interruption or queueing after a
read race.

The scheduler has a one-minute cadence floor and no catch-up. A due minute is
claimed once by attachment and epoch minute. Ten consecutive wakes without a
new human message or explicit resume raise the effective cadence floor to two
minutes; twenty raise it to five. A human message or resume resets the count.
A global rolling-hour cap bounds admitted wakes across all attachments.

Pause and resume are explicit API mutations. Both require a reason and actor,
and any hosted agent may manage any heartbeat. Every change is audited. A gone
target self-pauses with a reason. No reply text changes heartbeat state.

A paused attachment does not receive timer-only prompts. If its pre-ping
reports an arrival, it receives one prompt carrying the pause reason, what
arrived, and a request to restate its outstanding wait.

## Prompt and pre-ping boundary

The wake body is exactly the attachment prompt or the pre-ping replacement.
Nano appends one `[from nano heartbeat]` footer line. There is no cadence
guidance or pause token in the prompt.

Pre-ping source is repository-owned. The operator edits templates through reviewed Git
changes. An attachment selects a template and Nano stores an immutable copy for
that attachment. The API lists templates and copies but cannot accept raw
JavaScript, attach another copy by id, or append revisions.

Migration retains old script source as audit evidence but replaces each
executable legacy reference with a private repository-template copy. An
affected attachment stays paused and suppresses pre-ping execution until an
explicit resume.

The retained `inbox_match` template peeks at the public Nano inbox without
marking messages seen. Heartbeat
schema version 2 preserves retirement audit and pause state. Its three closed
condition columns remain unused for rollback pending a later operator decision.

Processors receive bounded JSON and return one strict `wake`, `skip`, or `fail`
object. Wake may replace the prompt and carry a bounded dedupe key. Failure
pauses the attachment and sends at most one Nano-written diagnostic. The log
stores bounded decision facts, never raw output or prompt text.

## Usage and retention

Every admitted wake begins with usage state `pending`. Heartbeat follows the
public completed-turn feed by operation id. It stores the provider's input,
cached-input, and output token counters, or marks the wake `unavailable` when
the provider reports none. Attachment responses sum reported counters and show
pending and unavailable wake counts. Heartbeat does not estimate money.

Decision-only skip rows expire after 30 days. Delivery and failure rows remain,
as do attachment configuration, lifecycle audit, and immutable template copies.
There is no catch-up after downtime.

The exact API and operational examples live in
[`docs/features/heartbeat.md`](../features/heartbeat.md).
