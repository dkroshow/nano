# Completion subscriptions

Subscriptions tell a parent when a child thread finishes a turn. They replace
terminal notifications.

Dispatch and durable cursor reads live in `internal/features/subscriptions`.
Root startup supplies the canonical database and interact client.

Choose the channels when you create the child:

```json
{
  "provider": "codex",
  "invokerThreadRef": "<your thread reference>",
  "subscriptions": {
    "inbox": "send",
    "prompt": "none"
  }
}
```

Each choice is independent and accepts `send` or `none`. Omitted choices use
the values above. Inbox mail is durable and is the least intrusive option, so
it is the default. It does not start a turn. Prompt delivery is opt-in. It uses
ordinary system-origin prompt admission with a stable id, queues behind a
running turn, and never interrupts one.

Change an existing child's settings with
`PUT /api/v1/hosted-threads/{threadRef}/subscriptions` and a body such as
`{"inbox":"none","prompt":"none"}`. Both choices must be explicit. The
settings persist and apply to subsequent completions without a restart.
Disabled children still advance their completion cursor, so turning notices
back on does not replay the quiet period.
In the browser, open the child's thread details and toggle **Inbox notices**
or **Prompt notices**. Notices already admitted to the parent remain queued.

The dispatcher reads only the public completed-turn feed. That feed contains
completed, failed, and interrupted outcomes plus provider token counters when
available. Root server glue does not use the private operation-transition log
as a second subscription source.

Both delivery forms carry the outcome and a link to the normal turn-operation
result when the turn has one. They do not copy the child's response into the
delivery. Nano persists one cursor per child. Existing children start at their
latest completed turn during the upgrade, so old results are not replayed.
New children start with an empty cursor, and stable delivery ids plus the
durable cursor prevent repeat delivery after a process restart.

Nothing polls. The dispatcher reads a child's feed after the child's finished
turn is saved, when the child or its parent becomes live in this process, and
once for every child at startup. A failed read or delivery tries that child
again after 10 seconds. Exited and detached children or parents are skipped.
Each child's notices go out on one worker at a time, and no lock is held
across a delivery: a parent that is slow to accept one (a thread still
replaying after a restart) delays only the notices addressed to it.

Every live child with an invoker participates in the existing event-driven
completion and startup sweeps, including children with both channels off.
A detached child whose old disabled completions have not been swept can replay
them if notices are enabled before it becomes live. Already-delivered and
already-queued notices are preserved.
