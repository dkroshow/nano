# Mail

Mail is Nano's durable, asynchronous channel between hosted threads. Use it
when a message can wait for the recipient's next inbox check. It never starts
or interrupts a turn. Completion subscriptions use mail by default because it
is the least intrusive delivery channel.

Send through `POST /api/v1/hosted-threads/{recipientRef}/messages`. A thread
supplies its own `senderThreadRef`, a stable `clientMessageId`, and a body. The
Nano UI can instead set `operator:true`; the stored sender is the generic
`Operator` identity. Subject and opaque refs are optional. Exact retries return
the first message, while reuse of the id with different content conflicts.

Disposition feedback is off by default. A sender that needs an inbox reply when
the recipient acts, declines, or defers sets `dispositionFeedback:true` on that
message. Otherwise the sender reads its outbox to inspect the current status.

Recipients read their own inbox with
`GET /api/v1/hosted-threads/{self}/inbox?actorThreadRef={self}`. A normal read
marks returned unread messages seen; `peek=true` does not. Disposition through
`POST /api/v1/messages/{messageId}/status` with `acted`, `declined`, or
`deferred`.

Mail owns `mail.sqlite`, separate from the canonical threads database. The
first open copies all legacy mail rows and verifies table counts. It leaves the
legacy tables untouched as rollback evidence. Mail is retained indefinitely;
there is no delete API. The store has one optional setting: days to keep
dispositioned mail. It defaults off. `-mail-retention-days=N` persists a
positive number; `0` disables it; omitting the flag preserves the stored value.
When enabled, startup and the daily sweep remove only acted, declined, or
deferred mail older than the cutoff. Unread and merely seen mail never qualify.

The mail models and store live in `internal/features/mail`. Root server files
only adapt HTTP, catalog overlays, and notices.

Mail keeps one bounded entry in interact's generic per-thread notice set. The
core appends that entry to an already-admitted prompt when unread mail exists.
Sending mail alone never wakes the recipient, and prompt admission never reads
the mail store.
