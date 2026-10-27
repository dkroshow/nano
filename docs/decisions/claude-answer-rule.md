# One rule decides which Claude answer belongs to which prompt

Date: 2026-10-21

Status: accepted. Replaces the answering-execution rule of the Claude member
step, the live-final check on a Claude operation's saved answer, and the
`claude-result-bindings` maintenance endpoint.

## Decision

A Claude prompt's answer and outcome are those of the Claude turn that
consumed it, as Claude's saved transcript records it.

- **A saved turn is Claude's own:** the rows that carry one `promptId`
  (reader v35). That includes a turn Claude started itself, for its own task
  notification or a slash command, and a prompt it took mid-turn as a plain
  row.
  - An interruption marker or API error stub settles the turn it ends, even
    though Claude writes the next turn's `promptId` on it.
  - A row without a `promptId` joins the active turn. All 67 such user rows
    in 5,278 files are local-command artifacts or tool results.
  - Wire rows keep the shim execution's turn.
  - A slash command a sender sent is saved as Claude's expansion of it; that
    row is the sender's prompt, read as the command it sent (reader v36), so
    its answer is its own turn's like any prompt's.
- **A prompt is consumed where a saved user row carries its submission id:**
  the row's `uuid`, an entry of `mergedSendUuids`, or the `source_uuid` of a
  `queued_command` Claude absorbed mid-turn.
- **Prompts one turn consumed share its answer**, whichever execution
  Claude's live result named.
- **Only a completed turn has an answer**, its final message. An
  interrupted turn's outcome is interrupted, and a truncated or unended turn
  decides nothing yet.
- **A root turn a later root turn follows has ended.** Claude starts a turn
  only after the last one ends. So a saved root turn that recorded no end,
  with a later root turn after it, was cut off when its process stopped (a
  reboot, a crash), and its outcome is interrupted. A worker's turns, saved in
  the root file as sidechain rows while the root turn runs, do not count. The
  saved-row read applies this
  (`history.ReadSavedUserRows`), so it needs no new reader and no re-read.
  Claude's own task notifications, saved one row each and answered together
  in the last one's turn, read the same way; they carry no Nano prompt.
- **A prompt Claude never took is the person's to resend or discard.** Claude
  takes the prompts it is handed in order. When a prompt handed to the same
  session later is consumed and this one is in no saved row, Claude never
  took it, so it is held for the person
  ([server-owned-prompt-actions.md](server-owned-prompt-actions.md)).
- **Only a projection the current reader built decides an answer.** A Claude
  saved-row read refuses an older reader's projection; the upgrade backfill
  re-reads the file, and its commit retries.

One function, `claudeAnswers.answer` (`claude_answer.go`), applies the rule
for every consumer:
- an operation's saved answer, including the late read of one that failed
  as `result_not_saved`;
- the member step that finishes owed prompts, attached or not: a thread with
  no session is finished from the catalog commit;
- the completed-turn feed.

## Why

Three fixes in a week each patched one way Nano tied a Claude answer to a
prompt: merged prompts, results bound to the last-written prompt, and the double answers that appeared after the prior implementation.
- **The root cause:** Nano drew Claude's saved turns by the person's prompt
  rows. It folded every turn Claude began itself into the previous one, and
  split a turn at a prompt taken mid-turn.
- **What made it stick:** two downstream rules then refused to decide. The
  member step required exactly one live result per saved turn, and an
  operation's saved answer had to equal its live final. Operations failed
  ('result_not_saved') or stayed running, and prompts owed their completion
  for good.

## Kept, with reasons

- **The live result** (claude_shim_transport.go) still records timing and
  usage on its execution and starts the saved-answer wait. It decides no
  answer.
- **Usage is the provider's count.** A turn the rule finishes records zero
  for a counter it has none for. The provider's own result replaces those
  placeholder zeros once, and the row then holds the provider's counts. Any
  count already recorded is kept.
- **Terminal decisions stand:** a live failure or interruption, the person's
  Stop, close or Discard. The rule reopens only 'result_not_saved', which is a
  wait, not a decision. The rule completes it with the answer it already read,
  with no further saved-result wait, and its owed prompt clears only once
  that completion commits.
- **Execution ownership:** an execution's row is written only when exactly
  one prompt claims that execution. A terminal row keeps its stored facts;
  public readers take the answer through the rule.

## The feed

`GET /api/v1/hosted-threads/{ref}/completed-turns` behaves as follows for
each Claude row:
- **A row whose execution answered a Nano prompt** publishes the rule's final
  message and outcome, and names the saved turn (`nativeThreadId`,
  `savedTurnId`). `turnId` stays the execution's identity and the cursor.
- **A completed row the rule has not decided yet** ends the page and holds
  the cursor. No stored final stands in for it.
- **A failed or interrupted row** publishes its decision with no answer until
  the rule decides one.
- **An execution two prompts claim** publishes no answer.
- **Sweeps:** every catalog commit of a Claude thread sweeps its owners'
  subscriptions, after the owed pass, and the dispatcher coalesces repeats.
  So an answer the commit decides, or a turn the owed pass finished without a
  notice of its own, reaches its subscribers.
- **A turn with no Nano prompt** publishes as stored, as every other
  provider's does.

## Consequences

- A Claude answer can wait up to the saved transcript's lag (about a minute)
  after the live result. Until each thread's file is re-read at v35, its
  answers wait.
- Old records come out right through the re-read itself; no maintenance
  endpoint or hand edit is involved. Each 'result_not_saved' operation, and
  each still-running one with an owed prompt, that the rule decides completes
  and notifies its sender.
- A cut-off turn is decided only once a later turn follows it, and a lost
  prompt only once a later prompt is taken. On a thread left idle since its
  process stopped, both stay `accepted` until it is used again.
