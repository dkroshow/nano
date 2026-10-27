# Per-turn token usage — SQL and API contract

Status: **active**
Created: 2026-08-26

## What

Nano extracts provider-native per-turn token usage during transcript parse,
stores it on `conversation_turns`, and exposes it through:

- views `thread_turn_usage` and `thread_turn_usage_messages` (the SQL contract)
- `GET /api/v1/threads/{id}/usage` (per-turn rows joined to that turn's
  user and assistant messages)
- `GET /api/v1/usage/threads` (fleet per-thread totals)

Internal columns may change. Direct SQL readers must use the views.

## Envelope

Columns on the view: `input_tokens`, `output_tokens`, `reasoning_tokens`,
`cache_read_tokens`, `cache_write_tokens`. NULL means the provider did not
report that field. These are a shared *shape*, not equivalent units.

| Envelope | Claude | Codex | Grok |
| --- | --- | --- | --- |
| input | `input_tokens` (excludes cache) | `last_token_usage.input_tokens` (includes cached input) | `inputTokens` (includes cached reads) |
| cache_read | `cache_read_input_tokens` | `cached_input_tokens` (subset of input) | `cachedReadTokens` |
| cache_write | `cache_creation_input_tokens` | none (always NULL) | `cacheCreationTokens` |
| output | `output_tokens` | `output_tokens` | `outputTokens` |
| reasoning | `thinking_tokens` (⊂ output) | `reasoning_output_tokens` (⊂ output) | `reasoningTokens` (⊂ output) |

Do not subtract cache_read from Codex or Grok input. Do not add cache_read
to Claude input. Do not fold Claude cache-read and cache-write. Do not
derive a Claude total. Reasoning is not additive with output.

`native_json` holds provider extras (Claude 5m/1h cache split, Codex
cumulative total and context window, Grok modelCalls / apiDurationMs /
costUsdTicks). `native_total_tokens` is provider-reported only.

Per-turn `model`, `effort`, and `service_tier` sit on the same view.
Consumers must not join `config_json` for these fields.

| Column | Claude | Codex | Grok |
| --- | --- | --- | --- |
| model | last primary assistant `message.model` | `turn_context` `model` (incl. `collaboration_mode.settings`) | session `summary.json` `current_model_id`, copied onto every turn |
| effort | last primary assistant top-level `effort` | `turn_context` `effort` / `reasoning_effort` | session `summary.json` `reasoning_effort`, copied onto every turn |
| service_tier | unknown | explicit `thread_settings_applied.thread_settings.service_tier` (legacy direct event field is a fallback); otherwise `turn_context`. Standard is Codex's `default` wire value. | unknown |

For Codex, the settings notice creates or reuses the semantic turn. An
explicit tier on that notice wins over `turn_context`; later explicit notices
on the same open turn are last-wins. `turn_context` fills the tier only when
that turn has no explicit setting. A missing value remains unknown: it never
inherits a session-wide tier or mutates earlier turns.

Grok has no per-turn effort (and no per-turn model on `turn_completed`). A
rebuild restamps the current summary onto every historical turn. Mid-session
model or effort changes are not preserved.
The transcript module retains this summary as thread metadata. History copies
the separately parsed snapshot into the usage display columns in its source
transaction; the stored model turn does not claim historical settings from it.

`usage_contract = 1` means this grain. The view NULLs envelope columns
when `usage_contract` is not 1, so leftover v7 cumulative/folded numbers
cannot be mistaken for per-turn usage.

## Grain

- Claude: last observation per API `message.id`, then sum distinct ids in
  the Nano conversation turn. Parent-file `isSidechain` rows are not mixed
  into the parent envelope. A dedicated worker JSONL is its own thread.
- Codex: one canonical turn per provider `turn_id` (`task_started` /
  `turn_context`) until `task_complete` or `turn_aborted`. Dual
  `response_item` / `event_msg` user serializations of that id do not mint a
  second row. Envelope is the sum of `last_token_usage` events on that turn.
  `total_token_usage` is the session cumulative checksum, stored in
  `native_json`, not the envelope.
- Grok: `turn_completed.usage` (already per-turn). If
  `acceptTerminalNativeTurnID` rejects the terminal, usage is not recorded;
  hand-sums for verification must follow that rule.

## Backfill

Criterion 4 is every thread whose canonical source still exists. Missing or
errored sources are not rebuilt; their view envelopes stay NULL. A bounded
scheduler rebuilds parser-behind present sources. Catalog recovery does not
rebuild a file whose only change is a parser-version mismatch.
After each batch that rebuilt work, the scheduler waits twice that batch's
own work time, so the backfill works at most a third of the time; that bounds
its duty time, not the CPU it uses while working. Time a batch spent waiting
behind scans or for the catalog's projection lock does not count. A batch
reads and parses each source without that lock and takes it only to commit.
It commits only if no worker pass committed the source or its transcript
since the batch read them; otherwise it skips the source, which stays a
candidate while it is still behind. To hold it
without a deploy, create `parser-backfill.pause` next to `history.sqlite`; a
batch rebuilds nothing while the file exists, including one already waiting
for a scan, and the backfill resumes within one repair interval (5 minutes)
after the file is removed.
Rebuilds are durable one transcript at a time, so a restart resumes with the
sources still behind.
A reader upgrade that changes only how rows carrying one marker are read
names that marker (`transcriptUpgradeMarkers` in `internal/history`). Each
batch first reads the bytes of at most 64 candidates whose upgrade names a
marker, and records a source without it as unaffected at its exact
generation, size and mtime (`conversation_parser_checks`), so it is current
without a re-read; a later append no longer matches the row and rebuilds as
usual. Reader v34 (merged Claude prompts) queued 6,000 sources for a re-read
of about a minute each; 7 carried `mergedSendUuids`. An upgrade that names
no marker re-reads every source.

## APIs

`GET /api/v1/threads/{id}/usage` pages by `afterOrdinal` / `limit`. Each
turn includes envelope usage, model, effort, service tier, and the turn's
user/assistant messages so a spike maps to the prompt and replies.

`GET /api/v1/usage/threads` returns per-thread envelope sums, `turnsTotal`,
and `turnsWithUsage`. It intentionally has no aggregate service tier: a
thread can contain both Standard and Priority turns. No cross-provider grand
total.
