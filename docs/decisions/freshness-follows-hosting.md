# Catalog freshness follows attached files

Nano watches attached files, finds sessions it did not start from file
events, and never polls or walks external history on its own.

1. **Every attached thread's native file is watched with one kqueue.** An
   attachment (any hosted session with a real native identity, visible or
   not) holds its file from identity binding until close, detach or a failed
   spawn. A write, extension, truncation or replacement of that file is a
   hint to re-read it; there is no settle window. One file has at most one
   pass in flight and one pending; a pass reads a snapshot, so a write during
   it always gets the pending pass. kqueue also sees appends through a
   descriptor the writer keeps open, which FSEvents never reported for Codex
   rollouts.
2. **Absent or replaced files are waited for, not polled.** While an expected
   file does not exist, a temporary watch on its nearest existing directory
   (shared by every file waiting there, released when none is) re-checks only
   the expected names. Directory events never discover anything else.
3. **No automatic root walk remains.** The startup sweep, the 15-second stat
   differ and the 5-minute repair ticker are removed. At startup
   the saved catalog is ready at once; each attachment catches up on its own
   file before its saved tail is restored. Explorer's Refresh
   (`POST /api/v1/thread-index/refresh`) runs the full recovery walk:
   discovery, absence and a retry of every errored checkpoint. It may hold
   attached ingestion while it runs.
4. **Outside sessions are found from file events, without a walk.** One
   FSEvents stream over the live provider roots (`~/.claude/projects`,
   `~/.codex/sessions`, `~/.grok/sessions`), with file-level events batched
   over 2 seconds, names the files that changed. The kernel records the
   changes as part of each write, so the stream costs nothing while nothing
   is written and holds no descriptor per folder or file. A named file that an
   attachment or an outside hold already watches is skipped. Any other file is
   checked against its root's rules (one `lstat`), held like an attached
   file, and given one targeted background pass. Nothing is listed or walked.
   A dropped batch (`MustScanSubDirs`) is logged, and Refresh recovers it.
   This was removed on 2026-10-02 and restored on 2026-10-27, when Nano's
   history search became the only transcript search and missed every session
   started outside Nano since the last Refresh.
5. **Outside holds are bounded.** A hold puts the file on the kqueue, which
   sees appends that FSEvents misses (Codex writes through a descriptor it
   keeps open). So a hold is not released for being quiet: its writer may
   still have the file open. At most 256 outside sessions are held at once
   (`outsideSources` in `GET /api/v1/monitor`); past that, the hold whose
   files were saved least recently gives way, and its file is held again on
   its next FSEvents event. A Grok session's summary and updates are held
   together, so a title written after the transcript still arrives. An
   attachment that holds a file takes over its outside hold, as for a hosted
   thread written before its re-attach bound it after a restart.
6. **Wire hints remain targeted.** A turn edge names its known file; no hint
   and no multiplexed child's raw traffic ever requests a walk. Name changes
   run the names reconciliation alone.

`-no-watch` now means no attached-file watches and no outside discovery:
attached threads then advance on wire hints and Refresh only, and outside
sessions enter on Refresh. Ingestion speed, throughput, backlog,
failures and watch counts are in `GET /api/v1/monitor` (`ingest`).
