# A send Nano never answered retries itself

Date: 2026-10-15

Status: accepted. Builds on
[server-owned-prompt-actions.md](server-owned-prompt-actions.md).

## Decision

Operator, before a flight on in-flight wifi: "after the user submits, the browser
keeps retrying until it goes through".

- **Submit does not ask the network first.** The composer hands its text
  over whenever the session accepts prompts; its text is stored
  (`nano-unsent:<id>`) before anything else. It posts at once unless the
  connection indicator already knows Nano cannot be reached, or an older send
  to the thread has no answer yet: then it is held, stored with a `held`
  mark and unposted ('waiting for the connection', 'waiting for the earlier
  message'). The mark leaves the record before any POST for it does, so a
  held send provably never reached Nano.
- **One at a time, oldest first** (amended 2026-10-15). A thread's
  unacknowledged sends go in the order of their stored `createdAt`: a later
  one posts only when every older one is acknowledged, ended or refused,
  even while the older one's POST is out. A send the person discarded blocks
  no one. Only the front send has a retry timer.
- **Discard is the control on a stuck send**. Operator: "I don't
  need to edit a stuck prompt. Just being able to discard it is fine". A
  stuck send offers Discard, and Resend where it is the front send Nano may
  have; there is no editing. (An edit-before-it-goes feature shipped briefly
  in 4fcd10a5 and was removed.) Discard returns the text to the composer,
  where it can be changed and sent as a new message.
- **Every automatic post reads the stored record first.** Another tab may
  hold the same send: if its record is gone or stopped, this tab stands
  down.
- **A slow send is not cancelled.** On plane Wi-Fi a request took about
  9.5 s just to reach Nano, and a 15 s abort cancelled sends that were
  still arriving. A send's POST now stays open: after 15 s its row says
  "Still sending", and only a network error or 60 s ends the wait and
  counts as no answer (amended 2026-10-20).
- **Only a send Nano never answered retries.** That is the one prompt the
  browser holds. Its copy's reducer (`unsentStep`) carries `retry` and
  `attempts`; while `retry` holds, the store re-posts the thread's front
  send (the only one with a timer) under its own action id after 2, 4, 8 and
  16 seconds, then every 30 seconds; its row counts down to the next try. Nano's journal
  answers an action id once however often it arrives, so a retry delivers at
  most once.
- **Any answer from Nano ends it.** A 2xx/202 or a refusal answers the POST;
  a read of the action that returns a record in any state makes it Nano's. A
  refusal ends the action for this browser, so a POST that was already on the
  wire and then gets no answer cannot show or post it again. A
  read answering 404 means Nano is reachable without it, so the POST is tried
  again. Prompts Nano holds (accepted, uncertain, parked, queued) are never
  re-posted by the browser: an uncertain prompt may still reach the provider
  late.
- **A front's answer is no answer.** A 502, 503 or 504 whose body is not
  Nano's error envelope (`{"error": {"code": …, "message": …}}`, which every
  error Nano writes carries) came from Tailscale Serve or a dev proxy answering for
  a Nano it could not reach (a deploy restart, say): after the one registry
  refresh and re-post it is retried like a lost answer
  (`send.post.gateway`). The same statuses with Nano's envelope, such as its
  503 `restarting`, are Nano's refusal (`send.post.refused`).
- **Discard stops it.** Ending a copy marks the send stopped, in its stored
  record too, even when Nano could not confirm the Discard: neither a late
  lost answer nor a reload or session handoff turns retry on again. Only the
  person's own Resend posts it again (its fresh record clears the mark). The
  mark is stored before Nano is asked; a browser that cannot store it does not
  discard and says so, as a Send it cannot store is not sent.
- **Waking.** The connection indicator (`web/src/connection.svelte.js`)
  reports when Nano answers again after it could not; the front send posts
  at once instead of waiting out its backoff, and the rest follow in order.

## Consequences

- An offline Send no longer shakes the composer: the text moves below it,
  survives a reload, and says what it waits for ("waiting for the
  connection", "waiting for the earlier message", "retrying in 6s (attempt
  3)"), with Discard, and Resend (post now) on the front send. One line
  above the rows, "N not delivered", counts the same list and opens or
  closes it (closed by default at two or more).
- Standing limit: when Nano ends an id it never had (a Discard of a send
  whose answer was lost), it refuses that id's late arrival only for
  the life of its process (`discardedUnknown`). A POST already on the wire
  that reaches Nano only after a restart following the Discard could still
  land. A durable record of such ends is a follow-up (data change).
- The connection indicator is app state, not prompt state. Any answer from
  Nano counts as contact (stream events, responses); it probes only while
  the page is visible, at most every 5 s: `GET /api/v1/build` after 5 s with
  no other answer or when its latency figure is 30 s old, and the static
  64 KiB `GET /api/v1/connection-test` at most every 2 minutes. Its
  "Last success: Ns ago" ticks on a display clock that asks Nano nothing.
