# Claude reader extraction

`content.go` extracts the current history parser's content, usage, question
answer and tool-result helpers at `115207e`. `ParseFile` and `ParseWire` now
use those helpers with shared block assembly. The original parser remains in
service until the bounded differential and cut-over gate.

The executable reader tests cover the named research cases
`interleaved-message-ids`, `wire-initial-content-at-start`,
`tool-call-completeness-and-failure`, `bare-string-message`, and the
constructed `paired-split-file` / `paired-split-wire` pair. The latter is a
constructed comparison, not a genuine dual capture. A later genuine
[paired assistant excerpt](testdata/captured/paired-assistant-excerpt/manifest.md)
adds two file rows and fourteen shim records from one observed API message.
Every byte split of the
bare-string fixture restores through JSON to the same state and leaves the
prior state unchanged. Repeated file UUIDs add replay evidence without new
items or turns in that fixture.

The additional constructed `parent-tool-scope` fixture reuses a message ID
and block index across parent/child traffic, including checkpoint restoration.
The research `worker-file-and-parent-scope` case is also exercised within a
parent stream. Neither child completion nor child usage affects the parent.

Named changes from the current parsers:

- CLAUDE-PAIR-1: the genuine paired assistant excerpt preserves wire
  `request_id` as the same API request alias as file `requestId`. Native
  `terminal_reason: completed` settles successful completion. File stop reasons
  remain derived candidates; wire result model buckets stay separate from
  converged response usage. The pair excludes prompt/control metadata and
  does not claim complete conversation coverage.

- CLAUDE-SCOPE-1: native parent tool-call identity scopes nested wire messages,
  blocks, usage and results independently. A child without a native session ID
  keeps that uncertainty; no native session/turn ID is invented. Worker
  sidechain rows preserve their parent session and worker identity.
- CLAUDE-SCOPE-2: the first explicit root session ID on a Claude source can
  bind its preceding source-local turn. The paired wire fixture now needs no
  caller-supplied session ID. Items keep their IDs and source positions; original
  observations keep unknown scope. Replays and usage retain the established
  local turn identity. Known session conflicts, nested calls and worker rows
  cannot supply this binding proof. Each revised item is emitted separately.
- CLAUDE-BLOCK-1: split and interleaved API messages keep distinct ordered
  blocks, with native message IDs and native or derived block-index aliases.
  Stream block starts retain initial text; later snapshots replace the same
  block. API messages do not each become a human turn.
- CLAUDE-SCOPE-3: remembered root session identity belongs to one connection
  within the source incarnation. Nested/worker traffic cannot replace it, and
  explicit caller native scope takes precedence over a remembered default.
  A new connection's unbound content remains unbound.
- CLAUDE-PART-1: the imported mixed-content-and-images fixture keeps native
  image/document blocks as typed user content. Malformed array entries and
  empty strings retain their positions; invalid values are inspectable with
  diagnostics. URL media stays a reference. Bare string source paths point at
  the native field. Whitespace-only stream deltas remain content. Confirming
  snapshots update retained part detail without appending snapshot text.
- CLAUDE-TOOL-1: streamed JSON arguments exclude the empty start placeholder.
  A block stop completes generation only. A failed tool result is a separate
  item linked by `result_of`; it does not fail the parent turn.
- CLAUDE-TOOL-2: command arguments expose command display text; supported
  Edit/Write inputs expose proposed file changes, and plan-bearing mode tools
  expose plans. The first part retains the complete native argument stream,
  including whitespace and options. Display text never feeds later argument
  deltas. Plans and edits gain no approval/applied outcome from generation.
  Separate results can link to these specialized invocation facets.
- CLAUDE-END-1: `end_turn` provides a derived candidate. A successful native
  result promotes finality; adverse results retract candidates. Duration
  metadata does not prove successful completion or erase failure.
- CLAUDE-END-2: all three native interruption-marker fixtures settle the existing
  turn without creating a human prompt. Synthetic API error stubs cannot become
  answer candidates. A bare API-message `api_error` remains failure evidence
  without settling the conversation. Named refusal retractions retain content
  and exclude it from later final selection. Fresh tool activity reopens a
  derived stop; later derived evidence cannot replace a native terminal.
- CLAUDE-ORIGIN-1: native task notifications and coordinator messages retain
  envelope/origin evidence as system items. They do not create empty turns.
  Explicit human origin still opens a human prompt turn.
- CLAUDE-ORIGIN-2 (reader v36): a slash command an SDK sender sent is saved
  as Claude's expansion of it (`<command-message>`, `<command-name>/X`,
  `<command-args>A`) on a row Claude marks `turnOrigin: "sdk"`. That row is
  the sender's prompt, read as the command it sent, `/X A`, so it settles like
  any prompt. A command typed in Claude's terminal carries no such mark and
  stays a system item. The upgrade re-reads only files with content that
  opens with a command tag.
- CLAUDE-TURN-1 (reader v35): a saved file's turn is Claude's own, the rows
  that carry one `promptId`. A notice row with a new `promptId` (Claude's
  task notification, a slash command typed in its terminal) begins the turn
  Claude gave it and stays a system item; a plain prompt Claude took
  mid-turn, carrying the running `promptId`, stays in that turn. An
  interruption marker or API error stub settles the turn it ends first,
  whatever `promptId` it carries. A row without one, and every wire row,
  keeps the prompt rule; a replayed row never moves a turn.
- CLAUDE-USAGE-2: null, fractional, negative, string and overflowing counters
  stay unknown with diagnostics for invalid values. Native fields remain
  inspectable. The imported usage revision fixture proves two selected API
  messages with partial cache coverage; the result usage fixture retains
  per-model buckets without adding them to message totals.
- CLAUDE-REPLAY-1: scoped row UUID receipts compare native content and metadata
  with exact JSON numbers and field-order independence. Repeated receipts keep
  ordered replay observations but do not roll usage backward, repeat terminals
  or retarget active messages. Conflicting UUID payloads leave accepted facts
  unchanged and emit a diagnostic. This applies inside retained continuation;
  stream wrapper UUIDs remain excluded.
- CLAUDE-META-3: file and wire rows emit typed catalog facts from the existing
  metadata adapter: identity, lineage, model/configuration, title and preview.
  First-title and first-preview rules survive checkpoint restoration and the
  first native session binding. Native clocks, including fallback fields and
  numeric epochs, remain separate from capture clocks. `catalog.jsonl` is
  compared with the old adapter after every prefix in history tests.
  The `capture-row-started` and `capture-row-result` fixtures also prove that
  operational journal actor IDs never become conversation identities.
- CLAUDE-EVICTION-1: a new prompt retires earlier settled turns and their
  items, messages and replay receipts. Pending controls and unfinished turns
  remain. A settled turn kept for trailing records (the latest) retains only
  the items a later record can re-emit: its final answers, the calls a late
  result links to, and items a pending control or capture names; its other
  output belongs to the consumer's rows (`TestGrowingCheckpointsRetireSettledTurns`,
  one package above). The wire keeps a settled turn whole: a pane shows its
  live rows until the saved file catches up. File chunking does not change
  the eviction boundary. An unmarked
  identity after eviction produces a new local item key with the same native
  aliases; history resolves it against its persisted rows at cut-over.
  `checkpoint_test.go` and `replay_test.go` assert this reader boundary. The
  exact private-source bound and semantic-append test lives one package above
  in `corpus_checkpoint_test.go`.
- CLAUDE-USAGE-3: response usage revisions reference emitted, scoped evidence
  observations with their own clocks. A new human turn cannot inherit a final
  answer from a previous active API message.
- CLAUDE-TIME-1: absent wire timestamps remain null. Caller capture and receipt
  clocks stay separate. Each block and terminal clock keeps its own scope.
- CLAUDE-CAPTURE-1: shim spool rows and `session/line` envelopes decode the
  enclosed native record. Scoped shim sequences prove replay without becoming
  native event IDs. Replayed deltas, usage and terminals are not applied again;
  conflicting sequence payloads produce diagnostics. Shim timestamps remain
  capture clocks, and process exit/replay boundaries do not complete turns.
- CLAUDE-OBSERVATION-1: each stream observation ID has one scope and meaning.
  A message stop-reason observation is not also emitted as thread metadata.
- CLAUDE-CONTROL-1: native request IDs are scoped by session and requester.
  Permission and elicitation requests produce interactions; matching responses
  preserve decisions and `result_of` evidence without settling a turn or tool.
  Exact request/response replay adds receipts without reopening answers.
  Conflicts and ambiguous directionless replies remain diagnosed and unjoined.
- CLAUDE-CONTROL-2: settings and interrupt exchanges remain observations.
  Successful acknowledgements do not restamp a turn's model or prove its end.
  Survey-only hook/MCP/cancel schemas are explicitly unverified. Response-only
  fixtures retain unresolved observations instead of inventing a request item.
  These are named differences from the research draft's empty system items
  and hypothetical turn. Initialize pending arrays remain native detail;
  nonempty or malformed arrays carry the CLAUDE-CONTROL-4 diagnostic.
- CLAUDE-QUESTION-1: the composite question/control/file fixture retains one
  interaction invocation and a separate answer result linked by `result_of`.
  Exact question keys and multi-select arrays survive in native detail. A
  control can augment a uniquely matching generated AskUserQuestion invocation
  without changing its position or erasing content. Single-block file answers
  reuse that scope; control-only captures do not acquire an invented turn.
  Reused call IDs remain ambiguous. CLAUDE-QUESTION-2 covers late generated
  calls; CLAUDE-QUESTION-3 bounds mixed answer overlays.
- CLAUDE-METADATA-1: all 23 captured attachment subtypes retain their complete
  bounded native fields, source coordinates, UUID and session scope. Native
  titles, settings, backups, queue and rate-limit facts remain observations.
  Unknown metadata also retains its payload with a diagnostic. These facts do
  not create turns or the empty system items proposed by the research draft.
- CLAUDE-JOURNAL-1: the captured task-journal `started`/`result` shapes stay at
  source/connection scope. A journal result never adds conversation completion
  evidence. Session cost fields remain native detail; their epoch-millisecond
  start clock is separate and never contributes per-turn token usage.
- CLAUDE-COMPACTION-1: summary text, native compaction boundaries and user
  compact-summary content retain compaction items without human prompt turns.
- CLAUDE-ENVELOPE-1: message envelope fields outside content blocks remain one
  scoped observation per row. Native prompt and API request IDs have separate
  aliases; neither becomes a native turn ID. Message effort and service tier
  remain turn facts. Wire API-message starts retain their model and scope.
  Completion observations retain the exact native stop reason behind the
  derived outcome; the envelope also retains stop-sequence details.
- CLAUDE-USAGE-1: partial message-start/delta usage is retained as response
  snapshot revisions. Repeated responses are selected once, with cache-read
  and cache-write counters separate. Result aggregates are retained separately.

The [conformance index](../../../docs/parsing/conformance.md) separates research
item views, detailed semantic assertions and the later corpus comparison.
CLAUDE-REPLAY-1 and CLAUDE-CAPTURE-1 require native UUID or capture-sequence proof;
equal text alone cannot prove replay. CLAUDE-CALL-1 keeps reused calls ambiguous.
Unknown nested content and survey-only controls remain bounded native detail
with diagnostics under CLAUDE-UNKNOWN-1 and CLAUDE-CONTROL-2/4. The parser does
not validate every opaque native field. Source memory and corpus parity remain
unmeasured; history conversion belongs to the separate cut-over.

`completion_test.go` exercises the imported interruption variants,
`synthetic-error-stop-sequence`, `partial-assistant-api-error-stop`,
`model-refusal-retraction`, the three `origin-*` cases, and constructed
provisional/native-terminal precedence sequences. Research draft system-only
error/refusal items become scoped observations where no content item is needed;
their evidence remains inspectable. This is an explicit model-shape difference.

`replay_test.go` covers the imported `duplicate-row-versus-equal-text` case
and constructed old-row usage rollback, metadata-only UUID conflict and repeated
terminal cases. Checkpoint serialization preserves receipt state. Native time
and capture/receipt clocks do not participate in the row payload fingerprint.

The six-field item-view layer now covers 119 single-input research cases in
`../research_views_test.go`, against original expected files and the reviewed
`testdata/item-views.json` differences. Two paired fixtures use the existing
paired scenario; `TestClaudeResearchSourceReplacementAndBudget` executes the
remaining mutation recipe. This layer supplements detailed semantics; it is
not a corpus differential.

Additional named differences:

- CLAUDE-CONTEXT-1: focused research views omitted surrounding native prompts
  or content when they asserted terminal, compaction or usage behavior. The
  reader includes that content in source order.
- CLAUDE-UNKNOWN-1: unknown blocks retain their native envelope role and bounded
  payload with diagnostics. The explicitly unobserved server-result specimen
  gains no speculative tool outcome or relationship.
- CLAUDE-OBSERVATION-2: malformed rows retain source records; unresolved stream
  events retain native detail and diagnostics, including orphan stop/usage
  deltas. Neither creates an invented item or turn.
- CLAUDE-REWRITE-1: replacing source bytes requires a new incarnation and fresh
  checkpoint. Parser item IDs and source positions change; the native
  `claude.rowUUID` alias survives. The module reports an incompatible checkpoint
  on misuse and row-too-large diagnostics under the recipe's 32-byte budget.
  It does not detect filesystem rewrites itself.

CLAUDE-CALL-1: `testdata/ambiguous-call-id` supplies two generated
invocations with one native call ID in one turn, followed by a result. Both
invocations survive; `ambiguous_call_id` and `unresolved_tool_result` explain
why the result cannot choose the last invocation. The checkpoint retains
that ambiguity instead of silently replacing the earlier binding.

CLAUDE-CONTROL-3: `testdata/unknown-control-decision` and the request-vocabulary
matrix require an actual native decision. A successful RPC envelope alone
leaves interaction outcome unknown with `unverified_control_decision`.
Permission behavior and elicitation action vocabularies cannot substitute for
one another. Request/reply relationships survive without granting permission
or completing execution or a conversation turn.

- CLAUDE-QUESTION-2: a uniquely matching earlier question control and a later
  generated native call retain one logical item, its first source position and
  its decision. Earlier unknown-scope observations remain unchanged. Reused or
  already bound call identities cannot supply this join. Checkpoint restoration
  preserves the same result and never mutates prior state.
- CLAUDE-QUESTION-3: row-level structured answers spanning multiple native result
  blocks remain in envelope detail with `ambiguous_question_answer_overlay`.
  Each result keeps its own native block text and any proven call relationship;
  the row overlay is not guessed onto multiple blocks.
- CLAUDE-CONTROL-4: initialization pending arrays remain bounded native detail.
  Nonempty or malformed arrays produce `unverified_pending_controls`; no pending
  request is hydrated from an unverified nested schema.
