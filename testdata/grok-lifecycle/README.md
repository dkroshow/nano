# Grok handshake response excerpts

Captured read-only from the served Nano `/state` snapshots on 2026-09-09,
using Grok Build 1.0.5. `session-load-1.0.5.json` came from the boot-reattached
sample-task simplifier; `session-new-1.0.5.json` came from its fresh v2 comparison.
The complete snapshots remain in sample-task's stage-4 lane evidence.

Native IDs, title and cwd are replaced with fixture values. Model catalogs,
unselected configuration options, git details and unrelated metadata are
omitted. Identity nesting, selected posture and model fields are preserved.
The load reply has no top-level identity. The new reply does.

The handshake tests replay these excerpts through a disposable ACP process.
Derived cases omit all response identities, retain only sessionDetail, or add
a running prompt. Identity-free success uses the requested native identity,
per the overseer's sample-task residual ruling; a different response identity is
refused. No provider process or live database is used by these tests.
