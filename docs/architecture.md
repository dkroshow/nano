# Architecture

Nano is one Go binary that serves a web page and an HTTP API on
`127.0.0.1:7690`. It starts and drives coding-agent sessions through each
provider's own CLI, reads the transcript files those CLIs write, and keeps its
own state in a few SQLite files. This page describes the pieces and how they
fit. [principles.md](principles.md) says how changes are judged; the records
under [decisions/](decisions/) say why each piece is the way it is.

## The server

The `internal/app` package wires the server; `cmd/nanod` calls its entry point. It
uses `internal/config` to parse every flag into one `Config` value before it opens a
store or a listener, builds the HTTP router (`routes.go`), and composes the
session registry from `internal/sessions`. That package owns live attachments,
provider connections, lifecycle application, prompt delivery and ordered wire
events, live projection and the loop that reconciles saved rows with streamed
output. `internal/hosted` owns hosted identity, admission and control;
`internal/hosted/store` supplies the shared durable adapters over canonical
records. `internal/sidebar` owns the visible-thread projection, fleet changes,
stars and browser warm streams. `internal/agentapi` adapts the existing feature
stores and schedulers to HTTP. App constructs these concrete owners, composes routes and listeners, and runs
and shuts down the process. Hosted owns boot registration and restart evidence.

`internal/` also holds the packages the server is built from. Each package's doc
comment states what it may depend on, and import tests hold the direction
for the parsing module, the state layer and lifecycle.

| Package | Owns |
| --- | --- |
| `hosted` | Hosted identity/control APIs, admission ordering, boot registration, restart evidence and transcript pins. It reads concrete session state and the catalog; it owns no second durable record set. |
| `hosted/store` | Shared metadata, execution and attachment adapters over the canonical thread/prompt/turn stores. Hosted and sessions use the same records. |
| `sessions` | Live attachment state and its registry, provider wiring, lifecycle application, prompt delivery, wire ordering, live projection and saved/live reconciliation. It also owns pane queues, warm sizing and pending/resting boot attachment facts; it never imports `app` or `sidebar`. |
| `sidebar` | Visible-thread materialization, fleet transitions, operator stars, runtime/list overlays and browser warm-stream membership. It consumes concrete catalog, session and feature owners; it does not own saved/live reconciliation. |
| `agentapi` | HTTP and application adapters for mail, heartbeat, feedback and completion subscriptions. Their feature packages retain the stores and schedulers; the adapter imports no app or liveview implementation. |
| `liveview` | Shared live-view values only; no loop, session state or orchestration. |
| `providers`, `providers/{codex,claude,grok}` | The five turn operations and capability declaration of each provider adapter. Adapters take values and return normalized evidence; they never see the app. |
| `wire`, `wire/{codex,claude,grok}` | The live connection to each runtime: Codex JSON-RPC over WebSocket, the Claude shim's framed stdio, Grok's ACP process. |
| `transcriptparse` | Pure readers of provider transcript bytes, file and wire, for all three providers (below). No Nano dependency. |
| `history` | Discovery and read-only projection of the native session files under `~/.codex`, `~/.claude` and `~/.grok`. |
| `threadindex` | Saved-thread catalog orchestration, provider-file ingestion, name projection, bounded transcript reads and their HTTP API. It consumes explicit provider path functions; sidebar composes runtime, hosted and mail overlays, the full catalog response and the combined event stream. |
| `catalog` | The kqueue watcher over held files, the FSEvents trigger that finds sessions Nano did not start, the conflated index-change stream, and the fleet read models the sidebar and Thread Explorer share. |
| `threads` | Durable hosted-thread state: identity, metadata, execution sessions, operations, attachment generations. |
| `core/prompts`, `core/turns`, `core/session` | Durable prompt delivery through provider acceptance; the per-turn record and usage; the mutable runtime of one hosted thread. |
| `lifecycle` | The provider-neutral state machine of a hosted thread: provider observations and attachment commands in, connection and work state and ordered effects out. |
| `transcript` | Whether a provider's saved transcript proves a prompt Nano delivered. |
| `interact` | The four operations features build on: submit a prompt, send and read mail, read the completed-turn feed, read a transcript page and lifecycle facts. Standard library only. |
| `features/{mail,heartbeat,subscriptions,feedback}` | Durable inbox, scheduled wakes, child-completion notices, operator notes. Each depends on `interact`, never on providers. |
| `config` | Process flags, defaults and resolved filesystem paths; opens no stores or workers. |
| `usage`, `usageclients` | Account-usage polling, retention and HTTP reads; read-only Claude, Codex and Grok clients. |
| `historical` | Prepared workspace/configuration reads for saved Codex and Claude threads. |
| `files/api` | Workspace file HTTP operations and reference resolution over saved and live evidence; `files` keeps its standard-library filesystem primitives. |
| `setup` | Read-only Claude context sources and the workspace skills cache. Session receiver handlers live in `sessions`. |
| `observability` | Bounded request measurements, rotating process logs, host-monitor reads, browser validation and transcript audits. Durable browser evidence remains in diagnostics. |
| `inspection`, `files`, `diagnostics` | Read-only setup evidence (git, MCP tools, Claude settings); bounded file-reference resolution; content-free browser evidence. |
| `httpapi`, `httpapi/remoteauth` | HTTP transport conventions, compression, discovery document delivery and connection measurement; the password-guarded remote listener and its store. |
| `state/db`, `state/store`, `hosted/schema`, `edge/statewiring`, `edge/stateopen` | Shared SQLite policy, checked migration and backup paths, hosted table schemas, and ordered opening of canonical stores at the program edge. |

`cmd/` holds the `nanod` entry point and the helper binaries: `claude-shim` (below), `nano-agent` (a thin
localhost client for the hosted-thread API), `nano-threads` (inspect or copy
the durable store, every path explicit), `transcript-dump` (run the parsers on
a file with no server), and `nano-launcher` (a small fixed parent process
for launchd jobs, so a macOS privacy grant keys to one binary and survives
CLI upgrades).

## Provider runtimes

Nano never implements a model API. It drives the CLI the user already has,
so sessions carry the user's login, settings and tools.

- **Codex** runs through `codex app-server`, a JSON-RPC daemon that holds
  many threads. Nano keeps one WebSocket to it (`-codex-ws`, default
  `ws://127.0.0.1:7691`) and routes thread-scoped notifications by thread id.
  If nothing listens there, Nano starts the daemon itself and it exits with
  Nano. Run it as a launchd user agent (`tools/local.nano.app-server.plist`)
  and Codex threads survive a Nano restart: reattaching is resubscribing.
- **Claude Code** is one `claude -p` child per session, speaking stream-JSON
  on stdio. The shim in `cmd/claude-shim` (default `ws://127.0.0.1:7692`)
  owns those children: Nano attaches over WebSocket and pumps lines through
  it, the shim sequences every line into a spool, and after a Nano restart the
  next attach replays exactly the gap. With `-claude-shim-ws ""` Nano spawns
  the child directly and the session dies with Nano.
- **Grok Build** (experimental) connects through `grok agent --leader` to
  Grok's own durable leader process on `~/.grok/leader.sock`. The user
  installs and supervises the leader; Nano only connects.

The three adapters normalize the same five turn operations (start, resume,
submit, interrupt, respond to a permission request) into provider-neutral
lifecycle and parser evidence, so the session, prompt and transcript code above them handles one
shape; provider-specific branches stay at the edges, in spawn and resume
resolution and the transports.

## Transcripts: the saved file is the truth

Every pane, agent read and search result shows rows parsed from the
provider's own transcript file, plus the unsaved live text of a reply in
progress. Nano keeps no copy of transcript rows in memory and writes nothing
under the provider directories.

`internal/transcriptparse` is the parsing module: bytes and explicit source
context go in; ordered observations, items, turns, usage and diagnostics come
out. `model` defines the shared `Item` and `Turn`; `codex`, `claude` and `grok`
interpret each provider's file and wire records through a shared assembly
step; `internal/jsonl` frames byte chunks. History ingestion and the three
live adapters all read through it, so a file and its live wire agree. The
contract and the fixture evidence live under [parsing/](parsing/).

Live text is reconciled to saved rows by exact identity, never by text or
clocks: Claude's stamped submission UUID and native message block, Codex's
native turn and item ids, Grok's submission id and event ids. When the
catalog sees the saved row, the pane's live copy retires. The same identity
settles prompt delivery: a prompt is confirmed only when the saved file holds
its user row.

## Stores on disk

All durable state lives under `~/.nanod` unless `-data-dir` moves it:

| File | Holds |
| --- | --- |
| `history.sqlite` | The thread catalog: metadata projected from native session files, cursors, and the contentless full-text search index. |
| `nano_threads.sqlite` | Hosted threads, their native-id claims, execution sessions, queued prompts and send attempts, completed turns. |
| `thread-names.json` | Operator-assigned thread names; a flat file by decision. |
| `mail.sqlite`, `heartbeat.sqlite` | The agent inbox; heartbeat schedules and wake budget. |
| `thread-stars.sqlite`, `thread-pins.sqlite`, `feedback.sqlite` | Stars, transcript pins, operator feedback notes. |
| `usage-limits.sqlite` | Account-usage observations (absent with `-usage=false`). |
| `remote-auth.sqlite` | The remote listener's password hash, browser sessions and login allow-list. |
| `logs/server.log` | The persistent server log. |

Live sessions also append every wire event to a transient JSONL spool
(system temp directory, or `<data-dir>/wire`) so older events can be paged
back without unbounded memory; closing the session deletes it and startup
clears stale ones. Every flag that names a path is listed by `nanod -h`.

## The web UI

`web/` is a Svelte 5 app built with Vite. `npm --prefix web run build`
writes `web/dist`, and `web/embed.go` embeds it (`//go:embed all:dist`), so
the binary is self-contained and a UI change needs a rebuild of both. In
development `npm --prefix web run dev` serves the source on :5173 and
proxies `/api` and `/events` to a running server.

Components and supporting logic are grouped by responsibility under
[`web/src/lib`](../web/src/lib/README.md): transcript, explorer, panes, composer,
mail, status and shared helpers. Tests and fixtures live beside their owning
area. App-level stores and server-event coordination remain in `web/src/`.

The page is a sidebar of threads and a set of panes. The sidebar controller
builds its projection at boot from the catalog, runtime, hosted metadata,
stars and mail, and updates it from events; `GET /api/v1/sidebar/threads` returns that materialized
state and never joins the sources per request. Historical rows load only when
the operator opens the Thread Explorer. Each pane holds its own event stream
and renders the one fleet state the server computes (`idle`, `needs_you`,
`working`); the browser derives no turn state of its own. The Enhanced view
shows user and agent messages; the All view adds tool calls and reasoning.

## HTTP API and event streams

Two layers share one router and one trust boundary, the loopback port:

- `/api/sessions...` is what the page uses: spawn, list and close sessions,
  submit a turn, read the conversation, stream a session's events.
- `/api/v1/...` is the agent API: hosted threads with durable identity
  (`nht_…` ids that outlive a provider respawn), turns with idempotency keys,
  mail, heartbeat, subscriptions, the thread catalog and transcripts, search,
  identity resolution and pins. `GET /api/v1/capabilities` is the guide
  agents read first; `GET /api/v1/openapi.json` is the exact contract, and
  both are embedded from `api/`.

Everything that changes is pushed as server-sent events: a session's wire
feed and conversation (`/api/sessions/{id}/events`, `.../conversation/events`),
a hosted thread's conversation, metadata and permission requests, and the
thread index (`/api/v1/thread-index/events`). Nano carries the fact in the
event rather than having clients poll.

The optional remote listener (`-remote-auth-port` with `-remote-auth-host`)
is a second loopback port for a reverse proxy. It accepts one exact `Host`,
requires the password set on the local `/security` page, and shares the
router behind that check; the local API never gains cookie authentication.
