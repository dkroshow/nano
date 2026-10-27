# Nano owns every pending prompt, its actions and their answers

Date: 2026-10-14

Status: accepted. Nano owns prompt delivery state and the available actions.

## Decision

The prompt store is the only state machine for a prompt. Its states are the
only vocabulary anyone reads: `queued`, `delivering`, `accepted`,
`uncertain`, `confirmed`, `refused`, `parked`, `expired`.

- **One stream, one shape.** The live stream's prompt entries, the journal
  list `GET /api/sessions/{id}/turn-delivery` and the single read
  `GET /api/sessions/{id}/turn-delivery/{clientActionId}` all carry the stored
  state, `allowedActions` and `label`, computed by `promptPresentation` from
  the same record. The journal has no second status word or reason code; an
  unknown id is 404. The submission receipt of `POST /turn` keeps its fields
  (it answers one submission; the browser reads only its HTTP status).
- **One rule for the actions.** `promptActionRefusal(provider, prompt,
  action)` returns nothing when the person may act, or the reason they may
  not. `promptPresentation` offers exactly the actions it allows, and the
  routes ask it again under the send lock, so a button and its answer cannot
  disagree:

  | State | Discard | Resend |
  | --- | --- | --- |
  | queued | yes (cancelled) | no: it goes when the running turn ends |
  | delivering | no: Nano is handing it over now | no |
  | accepted, confirmed | no: the provider has it | no |
  | uncertain, parked | yes (expired) | yes |
  | refused, expired | done already (200) | no: it has ended |

  A refusal is 409 with code `prompt_action_refused`, the reason as the
  message (the pane shows it verbatim) and `{clientActionId, state}` in the
  details. Discard of a prompt that already ended, or that Nano never had,
  succeeds: the end the person asked for holds.
- **Resend is one server request.** `POST .../turn-delivery/{id}/resend`
  retires the prompt and queues its exact provider text as a new prompt in
  one store transaction (`prompts.Store.Resend`, the same operation Nano's own
  lost-input recovery uses), then starts it as a fresh send would. The new
  action id is derived from the old one, so a repeated Resend answers with
  the same prompt and sends nothing more. An agent's prompt keeps its origin.
- **The warning is the label.** A prompt whose delivery passed Nano's
  deadline is `uncertain` with the label "Codex hasn't answered in 30 s;
  resending may deliver twice" (provider name per thread). Resend is allowed;
  the browser asks no question of its own. Before that, a prompt the provider
  has not taken 5 s after delivery claimed it is labeled "Codex hasn't taken
  it yet; sending again will deliver twice", still with no actions: a silent
  row invited the same words sent again. The claim time is the journal row's
  `updated_at`, so a reload computes the same label; the live stream presents
  the prompt again when that time passes, with no store write.
- **The browser renders.** It shows the server's rows, labels and buttons,
  sends Resend and Discard to Nano, and shows Nano's refusal. The only prompt
  it holds is a send Nano never acknowledged (`nano-unsent:*`), managed by
  one reducer (`unsentStep`); a refusal from Nano proves Nano has that action.
  That send retries itself until Nano answers
  (the unsent-prompt reducer). From Enter until the
  stream lists its action, the pane shows that send as a delivering row
  with the pending dot (its echo), so the prompt never leaves the screen
  while the network carries it. It ends on the server's word alone: the
  stream's row for the action takes its place, or the stream names the
  prompt saved (every live-text update names the prompts a read found
  saved, including ones it never showed live, and a tail names every prompt
  whose saved row is in its own page, matched by exact key to a row id, so
  a saved row and the end of its copy arrive in one message). A tail that
  neither lists nor names a send Nano took, whose row is older than the
  tail's page, leads to one read of that prompt's record: `confirmed`,
  `refused` or `expired` ends it. A tail that cannot read which prompts its
  rows settle fails the pane rather than go out without that word.

## Claude: a prompt lost after acceptance

Codex and Grok prompts that a turn ended without are held for the person
from file evidence: the saved turn they were bound to has ended. A Claude
prompt cannot be decided that way. Claude takes a prompt when Nano writes it
to its input, queues it itself, and saves it as its own row or, mid-turn, as a
`queued_command` attachment; its saved turns share no id with its live turns,
and nothing saved says Claude went idle with input unread. The remaining
signal would be a timer after a live turn end, but saved rows trail the live
stream (about 50 s seen, more when history writes are slow), so a timer would
hold prompts Claude did take and offer Resend under "turn ended without it":
the double delivery this decision removes.

Claude does merge queued prompts. Prompts that wait in its queue together
reach the model as one turn and are saved as one user row, a content block per
prompt: the row's `uuid` is one prompt's stamped id and `mergedSendUuids`
lists the others'. Each listed id is an exact identity of that row, so those
prompts settle with it like the first. Seen once, on 2026-10-18 (Claude
2.1.300): two prompts stayed `accepted` until the reader read the list.

A loss was seen on 2026-10-22: a reboot killed Claude mid-turn
while one prompt waited in its queue, and the prompt read `accepted` for good
(`testdata/claude-process-death`). The ordered proof decides it. Claude takes
the prompts it is handed in order, so when a prompt handed to the same session
later is in a saved row and this one is in none (not as a row's `uuid`, in its
`mergedSendUuids`, or as a `queued_command`'s `source_uuid`), Claude never
took it. The settlement pass holds it as `uncertain` (`turn_ended`) for the
person (`claudeTakenAfter`). The hand order is the shim's spool position in
the prompt's turn id. A lost prompt that no later prompt follows stays
`accepted` until one is sent.

## Consequences

- No browser clock ages, parks or confirms a prompt, and no browser rule
  decides Resend or Discard.
- Discard of an accepted or saved prompt is refused; before, it expired the
  record (accepted) or answered as if done (saved). The agent cancel path
  (`DELETE /api/v1/turn-operations/{id}`) applies the same rule.
