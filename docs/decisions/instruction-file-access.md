
## What

The instruction-source paths shown in the Responses Input panel are now
clickable, viewable, and (where safe) editable through a small,
policy-wrapped, session-scoped API — the brief's **Option 2**, nano-owned
endpoints. This endpoint family is not a general read/write-any-path primitive.

As of 2026-08-05 Nano also has a separate, operator-approved general local-file
surface for transcript links (`/api/v1/files/*`). It accepts existing regular
UTF-8 text files up to 1 MiB, keeps `~/.codex` and `~/.claude` view-only, and
uses the same atomic mtime-conflict write contract. The instruction endpoints
remain distinct: they preserve session provenance, their source allowlist, and
the synthetic instruction-write event.

Endpoints (`instruction_files.go`, routed in `main.go` under
`handleSessionScoped`):

- `GET  /api/sessions/{id}/instruction-files` — the session's instruction
  files with per-file metadata: `editable`, `readOnlyReason`, `exists`,
  `provenLoaded`, `sizeBytes`, `mtime` (unix seconds).
- `GET  /api/sessions/{id}/instruction-files/read?path=` — text content plus
  metadata (`{file, encoding, content, lineCount}`), allowlist-gated.
- `GET  /api/sessions/{id}/instruction-files/mtime?path=` — current on-disk
  mtime, allowlist-gated; polled by the viewer for external-change detection.
- `PUT  /api/sessions/{id}/instruction-files/write` — body
  `{path, content, mtime?}`.

Policy (unchanged from the brief, enforced in `inspectInstructionFile`):
exact allowlist match against the session's instruction sources (Codex
`thread.instructionSources`; Claude discovered-on-disk files); canonical
(symlink-resolved) path classification; `~/.codex` and `~/.claude` always
view-only (repo rule) plus a workspace-root check as defense in depth; writes
only under the session cwd / `runtimeWorkspaceRoots`; existing UTF-8 text
files ≤ 1 MiB only; no file creation; atomic write preserving permissions,
writing the symlink target rather than replacing the link.

**Write-conflict contract: mtime + force + polling.** The write body carries an optional
`mtime` (unix seconds). If present and it differs from disk by > 0.01s, the
write is rejected `409` with `{error, mtime: <current>}` and nothing is
written. An **omitted** `mtime` is a force overwrite — the UI sends it only
after the user confirms an overwrite at a 409. The viewer polls `/mtime`
every 5s while visible: a clean buffer reloads silently; a dirty buffer shows
an amber "File changed on disk" banner. This replaces the brief's suggested
`expectedHash` (sha256) contract.

**Internal transport: direct Go file I/O**, not app-server `fs/*`. Successful
writes publish a synthetic `nano/instructionFile/written` notification into
the session wire (feed + spool) via `recordNanoEvent`, so nano-originated
edits stay visible in the Event Feed.

**UI:** instruction rows open path-deduplicated file tabs. The viewer offers
Formatted/Raw, Edit/View, Save, and conflict handling. View-only files show a
reason and no edit action.

## Why

- **Nano-owned policy boundary (not raw `/rpc`).** The feature needs a
  feature-specific policy layer — protected directories, workspace roots, file
  size, binary detection, write conflicts — that a raw arbitrary-absolute-path
  RPC passthrough cannot express, and that must stay provider-neutral for
  Claude. Centralizing it in Go keeps the safety rules in one place.
- **Direct Go file I/O over app-server `fs/*`,** for four reasons: (1) policy
  checks need Go-side stats/reads anyway; (2) `fs/readFile` base64 blobs would
  bloat the Event Feed and JSONL spool with non-model-input traffic; (3) no
  Claude analog to `fs/*` exists, so Claude would need the Go path regardless;
  (4) an edit does not mutate the live thread's already-resolved instructions,
  so routing it through the thread's own protocol would misrepresent it. Wire
  honesty is preserved instead by the synthetic
  `nano/instructionFile/written` event.
## Alternatives rejected

- **Raw `/api/sessions/{id}/rpc` exposure to the frontend** (brief Option 1).
  Exposes an arbitrary absolute-path read/write primitive with no
  feature-specific policy; hard to make provider-neutral. Rejected as the
  product path.
- **App-server `fs/*` internals for Codex.** Would surface reads/writes as real
  wire traffic, but bloats the feed/spool with base64 blobs, has no Claude
  counterpart, and implies thread mutation the edit does not perform. Rejected
  in favor of direct Go I/O + a synthetic event.
- **External editor links only** (brief Option 3). Does not satisfy the in-app
  inspection goal; inconsistent browser handling of local file/editor links.
- **Replacing the instruction API with general file access.** The later
  `/api/v1/files/*` surface intentionally brings general existing-file access
  to transcript links, but it does not replace this session-scoped provenance
  contract. The general resolver uses direct paths, CWD, or unique thread
  evidence and deliberately omits earlier versions's filesystem-wide suffix walk.
