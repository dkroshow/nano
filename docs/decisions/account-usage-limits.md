
## What

Nano reads account-wide subscription usage ("how much of my weekly limit have
I burned?") for Codex, Claude, and Grok on a single five-minute timer, serves the
current reading at `GET /api/v1/usage-limits`, serves history plus a call log
at `GET /api/v1/usage-limits/detail`, and renders both in the sidebar rail
(`web/src/lib/status/UsageLimits.svelte`) with a detail modal
(`web/src/lib/status/UsageDetail.svelte`).

The three providers publish this in different places.

**Codex** exposes it as a first-class app-server request,
`account/rateLimits/read`, answered over the websocket nano already holds. The
response carries `rateLimitsByLimitId` — the account-wide `codex` bucket plus
per-model buckets — and `rateLimitResetCredits`, the count of unused one-shot
limit resets. No credentials, no second connection, no log scraping.

**Claude** stores nothing on disk and exposes nothing through its CLI. Its
`/usage` screen is drawn from `GET https://api.anthropic.com/api/oauth/usage`
with the OAuth access token Claude Code maintains in
`~/.claude/.credentials.json`, plus header `anthropic-beta: oauth-2025-04-20`.
Nano makes the same call. The useful field is `limits[]`, the provider's own
flattened projection: `session` (rolling 5-hour), `weekly_all`, and
`weekly_scoped` (one per separately metered model). Reading `limits[]` rather
than the typed `five_hour`/`seven_day` fields means a newly metered model
appears on its own without a code change.

**Grok** exposes the unstable ACP extension `_x.ai/billing`. Nano starts a
short-lived `grok --sandbox off agent --leader … stdio` client using the same
resolved executable, updater-disabled environment, and configured leader
socket as Grok sessions. It performs only `initialize` and `_x.ai/billing`,
then closes and reaps that child. It never calls `session/new`, loads a
session, starts a model turn, stops the leader, or changes launchd state.

Nano keeps only the subscription tier and one included-credit weekly or
monthly window: used percentage, duration, and reset time. The modern
`currentPeriod` shape identifies that window; Grok 1.0.3 omits a zero-valued
`creditUsagePercent`, which means 0%. The legacy path computes a monthly
percentage only when both non-negative `used` and positive `monthlyLimit` are
present. Unknown or no-limit shapes produce a muted notice and no bar.

Prepaid balance, on-demand caps and usage, auto-top-up settings, and raw
billing JSON are financial data outside this feature. Nano does not persist,
log, or show them, and does not call `x.ai/auto-topup-rule`. `_x.ai/billing`
is an extension method whose literal need not appear in the binary; JSON-RPC
`-32601` is the runtime upgrade guard for a Grok version that no longer
supports it.

## Why these constraints

**Read-only toward `~/.claude`; nano never refreshes the OAuth token.** An
OAuth refresh normally invalidates the refresh token it consumed, and nano
cannot write the replacement back without racing Claude Code and corrupting
the operator's login. The contract is: read whatever token is on disk; if it
has expired, say so and keep serving the last good reading with its age.
Claude Code rewrites the token roughly every eight hours, so any machine that
runs Claude at all stays fresh on its own.

**Storage is `~/.nanod/usage-limits.sqlite`, NOT `history.sqlite`.** The
catalog database is delete-and-rebuild safe by construction — every table in
it is derived from provider files. A usage time-series is the opposite: once
the moment passes the reading is unrecoverable. Keeping it in its own file
preserves that invariant for both.

**Rows are written only when a reading changes.** Two details make that work:
the change signature deliberately EXCLUDES the reset timestamp (Codex reports
an untouched window's reset as "now + the full window", so it advances by one
poll interval every poll and would defeat the whole design), and the
in-memory signature map is seeded from the newest stored row at startup (or
every restart re-appends a duplicate for every bucket). The signature is only
committed after the write succeeds, so a locked database retries next poll
instead of suppressing the reading permanently.

**A failed read keeps the last good numbers.** The snapshot carries them
forward with the error attached, and `ObservedAt` is not advanced, so the UI
can grey the bars and date them rather than blanking or silently lying. The
served reading is also seeded from SQLite at startup, so a restart mid-outage
does not blank the strip. Failure messages are written for the operator; the
technical cause rides along in `errorDetail` for a tooltip. An HTTP 404 gets
its own message because that is the shape a Claude Code update moving the
undocumented endpoint would take.

**A failure and a wait are different things.** The snapshot carries a `Notice`
beside `Error`: a notice means something is briefly not current, needs nothing
from the operator, and leaves the bars at full strength. This exists because
nano read only the ~8-hour `expiresAt` and reported every routine access-token
rollover as "Claude login expired" in red — the phrase Claude Code reserves for
a dead refresh token — sending the operator hunting for an account problem that
did not exist (2026-08-10). The credentials file carries
`refreshTokenExpiresAt` too, so the real login expiry (needs `/login`, roughly
monthly) is now told apart from the routine rollover (needs nothing, hourly).
A missing refresh expiry is treated as healthy rather than expired; a genuinely
dead login still surfaces as a 401.

## Codex's push notification is deliberately ignored

The app-server also pushes `account/rateLimits/updated` as turns consume
quota, and nano originally refetched on each push. With a live fleet those
pushes are continuous: on 2026-08-06 that produced **20 Codex reads in 7.4
minutes**, gaps as short as 1.7 seconds, where the timer alone would have made
2. (An earlier probe that saw zero notifications was wrong — it used a bare
connection with no sessions subscribed.)

Consuming the pushed payload instead of refetching would fix the volume — it
carries real numbers, not just a changed flag — but the schema documents it as
a SPARSE update carrying one bucket at a time, it never carries the
reset-credit count, and it has no Claude equivalent. Operator decision was to
drop it: one timer and one code path for all providers. The rationale is repeated
at the bottom of `usage_limits_codex.go` so it is not helpfully re-added.

## Cadence is set by Anthropic, not taste

A one-minute cadence drew **HTTP 429** from the Claude endpoint after roughly
thirteen calls (2026-08-06); five minutes had run clean before that. The 429
does carry a `Retry-After` header, but the observed value was `0` — useless,
since obeying it means retrying immediately into the same refusal. Five
minutes is 288 cycles and 864 requests a day.

**Known gap: there is no backoff.** A sustained 429 is retried on the same
timer. That is survivable at five minutes and was not at one.

## A cheaper source exists, for two of the three Claude numbers

Claude Code learns its own rate-limit state from response headers on ordinary
API calls — `anthropic-ratelimit-unified-5h-utilization` / `-reset` and the
`7d` pair — caches them in memory, and hands them to a configured status-line
command as `rate_limits: {five_hour, seven_day}`. That path costs **zero API
calls**. It does not carry the per-model weekly bucket, which is the one
currently binding this operator, and it requires a status line configured in
`~/.claude/settings.json` (which nano may not write). Recorded as a known
option, not adopted.

## Key files

- `usage_limits.go` — types, store, append-on-change, poller, `/api/v1/usage-limits`
- `usage_limits_codex.go` — `account/rateLimits/read`; the ignore-the-push note
- `usage_limits_claude.go` — the OAuth endpoint and its failure taxonomy
- `usage_limits_grok.go` — ACP billing probe, normalization, and failure taxonomy
- `usage_limits_detail.go` — in-memory call log, history query, `/detail`
- `web/src/lib/status/UsageLimits.svelte` — the always-visible rail strip (weekly, plus Grok monthly)
- `web/src/lib/status/UsageDetail.svelte` — modal: step charts, resets, call log
