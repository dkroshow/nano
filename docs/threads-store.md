# Threads store

Nano keeps hosted identity, settings, attachment generations, prompts,
send attempts, and turn facts in `nano_threads.sqlite`, beside its history
database. `internal/threads` owns the database mechanics. `internal/core/prompts`
owns prompt delivery facts through acceptance, while `internal/core/turns` owns
adapter-reported started and finished turn facts. Provider
adapters supply current connection and work evidence; stored status and timing
anchors cannot make a provider ready. See [Lifecycle](lifecycle.md).

These are the default locations, where `~` means the operator's home directory:

| File or directory | Default path and use |
| --- | --- |
| Canonical threads store | `~/.nanod/nano_threads.sqlite`, with matching `-wal` and `-shm` SQLite sidecars while open |
| History database | `~/.nanod/history.sqlite`; its directory locates the threads store |
| Legacy identity and settings | `~/.nanod/control.sqlite` |
| Legacy queued prompts | `~/.nanod/queued-turns.json` |
| Legacy send attempts | `~/.nanod/send-attempts.json` |
| Legacy completion migration source | `~/.nanod/turn-completed.sqlite`, or the path supplied with `-completed-turns-db` |
| Migration backups | `~/.nanod/threads-backups/threads-migration-*/`, including `control-original.sqlite` and a `turn-records/` backup directory |
| Server log | `~/.nanod/logs/server.log`; startup success and refusal messages appear here |

`-data-dir` relocates every unspecified server write target, including these
stores, feature stores, maintenance artifacts, the server log, and the transient
wire spool. With only a custom `-history-db`, the canonical store, legacy control
and JSON sources, migration backups, mail, usage, and maintenance files retain
their historical behavior and use that database's directory. The completion
source uses its own flag or default path. Each resolved path also has an
individual flag; run `nanod -help` for the complete list.

The package depends on the standard library and SQLite driver. It does not import
sessions, lifecycle, provider protocols, transcripts, or HTTP. Application
wrappers pass durable values. Existing metadata projections and delivery
coordinators remain callers; feature tables await their owning stages.

## Startup and migration

Migration runs automatically once, on the first start of a build that uses this
store. It writes the finished `nano_threads.sqlite` file into place only after
the copy is complete and checked. The migration leaves the original files where
they are and does not change their contents.

The server log confirms a successful startup with
`canonical hosted metadata and turn stores using …/nano_threads.sqlite`.
Confirm that line after deployment: a healthy HTTP endpoint alone does not prove
the threads store opened, because ordinary store errors can disable hosted
control while the rest of Nano serves requests.

Before provider recovery starts, startup prepares the canonical database and
opens identity, prompt, and turn stores on one serialized SQLite connection.
Prompt admission, claim, acceptance, uncertainty, queued cancellation, and
restore are transactions. Accepted rows never change. Adapter events alone add
started or finished turn facts; native-file events update only the transcript.

For the first migration, all application writers must be stopped. An online
SQLite backup cannot make the old JSON journals transactional with SQLite.
The startup routine, `PrepareStore`, takes explicit paths and performs these steps:

1. Back up `control.sqlite`, including committed WAL pages, then restore it into
   a private working copy and run the existing schema migrations through version 19.
2. Back up that copy, exact `queued-turns.json` and `send-attempts.json` bytes,
   and the old completed-turn database.
3. Import the three kinds of turn record in one version-20 transaction. Write a
   migration receipt, a database row recording that the migration finished. It
   includes record counts, the backup file list, and source fingerprints: hashes
   of the original file contents or the database facts copied from them.
4. Check and sync the working copy, then write the finished `nano_threads.sqlite`
   file into place. Keep every original source and backup.

Import preserves queue positions even when timestamps are out of order. It
preserves prompt and presentation text, hashes, action and provider turn IDs,
uncertainty, conflicts, and legacy confirmation fields. Completion IDs and the
SQLite AUTOINCREMENT high watermark remain feed cursors. Import performs no
delivery or pruning.

Only completion facts move from `turn-completed.sqlite`. The
`-completed-turns-db` flag names this legacy migration source.

The one-time schema-20 copy checks every copied legacy fact against the
migration receipt before any later schema step. The two JSON journals use exact
content hashes. Control uses a logical hash of its schema and copied table rows,
including SQLite sequence values. Completion uses its rows and cursor high
watermark. Unrelated legacy tables and SQLite page layout are excluded. A source change
during the copy exits with `threads_legacy_source_changed`; a receipt without
source fingerprints also refuses that copy.

After schema 20 is canonical, later migrations never read or reimport the
legacy sources. They validate `nano_threads.sqlite` with SQLite `quick_check`
and the metadata-reference check. This prevents a rolled-back binary's later
legacy writes from blocking schema 21, schema 22, or a future upgrade, and it
prevents those writes from resurrecting consumed prompts.

Missing legacy files mean that kind of record was unused.
Empty, malformed, unexpected, duplicate, conflicting, or future data
fails closed. A failed migration can leave private backups for inspection, but
cannot publish a partial destination or modify the original sources.

## Privacy, backup, and restore

The main file is created with mode `0600` before SQLite opens it. Existing regular
main and journal files are secured too. Backup files are private and published
only after validation and sync. Backup includes committed WAL data. Restore
reads a closed standalone backup and refuses sidecars, which could contain facts
absent from the main file. Both commands require a new destination and refuse
existing files. A directory-sync error after publication can leave the completed
file present; inspect it before retrying.

Build the operator command with `go build -o nano-threads ./cmd/nano-threads`.
With the default paths on this machine, use explicit destinations for each copy:

```sh
./nano-threads backup --database "$HOME/.nanod/nano_threads.sqlite" \
  --out "$HOME/.nanod/manual-backups/threads-20260909.sqlite"
./nano-threads restore --backup "$HOME/.nanod/manual-backups/threads-20260909.sqlite" \
  --out "$HOME/.nanod/recovery/nano_threads.sqlite"
./nano-threads size --database "$HOME/.nanod/nano_threads.sqlite"
```

Restore creates a separate database; it never replaces or switches a running
server's store. Selecting that restored copy requires a separate operator action
with the server stopped. Size requires the canonical schema. Backup also accepts
legacy `control.sqlite` schema 19, so it can preserve that store before an
approved change. Neither command creates or migrates a missing source. Restore
can also copy a pre-migration backup verbatim for investigation.

The one-off `prompts-cleanup` command can classify and normalize legacy prompt
rows. Operators should preview it against a copy before any separately authorized
apply. Migration and manual backups have no automatic expiry. Manifests and size reports
contain counts, paths, and hashes, but no prompt text; backup files contain private
data.

## Retention and size watch

Migration discards no records. Existing runtime limits remain:

- The prompt store holds at most 1,000 records. A full store refuses new text.
  Accepted prompts remain as permanent idempotency and turn-binding evidence.
- Journal persistence retains all prompt attempts. Opening the store does not
  prune them or remove prompt text.
- Hosted identity, attachment generations, and completion facts have no automatic
  expiry. Changing these policies requires a separate decision.

Startup and an hourly watch log main, WAL, and shared-memory bytes, allocated and
free pages, and hosted/queue/journal/completion counts. The `size` command returns
the same measurements as JSON. Samples are informational and may span concurrent
writes; they trigger no pruning, vacuum, or invented size threshold.

Disposable-copy tests cover schema refusal, private modes, all imported fields,
FIFO and cursor preservation, rollback, backup/restore with WAL data, retention,
size growth, and restart without resurrection. Fixtures prove that a source
change during the one-time copy refuses publication, while later schema steps
ignore changed legacy sources and keep the canonical facts. No test migration
uses live data.
