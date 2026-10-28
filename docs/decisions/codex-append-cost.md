# Codex transcript appends cost what they add

Status: **active**
Created: 2026-10-28

## What

Saving a Codex transcript append costs about what the appended lines add,
not what the running turn holds:

- **The reader keeps only what can still change.** In a turn that has not
  ended, a file checkpoint keeps the items a later record can revise: calls,
  serialization-twin candidates, the newest item of each type, anything an
  update, process stream or request names, and every item a native
  `item_completed` record made or can join (user and agent messages and
  reasoning, whichever copy came first). Codex completes such an item again
  under its id, a growing reasoning record up to about a thousand records
  later. Response rows never repeat an id, and a call's output follows its
  call, so other items belong to the consumer once emitted.
- **Usage samples are entries, not part of the turn.** The reader keeps each
  turn's usage observations as separate continuation entries (`usage`,
  `usageCounts`), so a token count adds one entry. Emitted turns still carry
  the whole list. The saved turn row names its samples by interned value
  (`uv`), reusing the saved ids when the list only grew; rows saved earlier
  with the inline list (`u`) are still read. Writing a turn reads its saved
  usage list only where it writes that list back.
- **A running turn's continuation stays in memory.** After a save commits,
  the history store keeps that thread's decoded Codex continuation while a
  turn is running. The next append resumes from it, and saves only the
  entries the reader reports changed (`State.Changed`) plus the small fields,
  compared with what was saved. The saved rows stay the source of truth:
  every append saves its own changes, the copy is replaced only after the
  transaction commits, and a projection, offset or commit mismatch drops it,
  so the next append reads the saved rows as before. A resumed save applies
  only to the rows its copy saved (the same read offset), and an append that
  saves nothing leaves the kept copy as it was. A restart starts from the
  saved rows.
- **The layout is versioned.** Codex checkpoint version 23 holds usage in
  its own entries; this reader still resumes a version 22 checkpoint and
  moves its samples. The previous reader accepts only 22, so after a
  rollback it rebuilds a thread saved as 23 instead of reading turns without
  their samples. Turn rows saved with `uv` read without their sample list
  in an older binary; its next save of that thread rebuilds it.

## Bounds

A thread's copy is kept only while its turn is running and only up to 96 MB
of encoded entries; all copies together up to 256 MB, dropping the least
recently used first; a copy idle for 10 minutes is dropped at the next save.
Past a bound the thread reads its saved rows, as before.

## Why

A worker's turn of about a thousand tool calls held 97 MB of continuation.
Each append read it back, decoded it, encoded it again and compared every
entry, and rewrote the turn's 2.3 MB usage list: about 10 s and 8.5 MB per
append, 15 to 22 times a minute, so Nano ran at 100 to 175% CPU and 2 GB
while such threads worked. Measured on that transcript's last 300 lines as
20 appends: about 0.4 s and 0.25 MB per append.

## Evidence it changes nothing stored

- On 199 real rollouts, including the 40 largest, the reader's emitted items,
  turns and diagnostics equal the previous reader's, and appending in chunks
  equals one pass.
- `TestTranscriptResidentAppendsMatchReopenedAppends`: appends resumed in
  memory save exactly what appends after a reopen save.
- `TestCodexOpenTurnKeepsNativeItemsForRecompletion` and
  `TestCodexOpenTurnCheckpointDropsSettledResults` pin the retention rule.
