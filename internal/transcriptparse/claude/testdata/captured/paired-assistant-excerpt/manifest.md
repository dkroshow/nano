# Genuine paired assistant excerpt

This pair comes from an existing Claude native transcript and its existing
shim spool, read on 2026-09-09. No provider process was launched. Two assistant
UUIDs and their content blocks matched exactly before scrubbing. The file
stores final message usage and stop reasons; the wire stores their earlier
snapshots followed by usage and terminal events.

- `file.jsonl`: two native assistant rows, with `apiBlockIndex` 0 and 1.
- `wire.jsonl`: fourteen shim spool records for the same API message, including
  its start, both blocks, both assistant snapshots, message delta/stop and result.
- This is an excerpt. The human prompt, initialization/control traffic and
  intervening system metadata are omitted. Original shim sequence gaps remain.
  It does not prove a complete conversation capture or cross-process recovery.

Text is replaced with `OK`; the original empty thinking content remains empty.
Signatures, paths, branch/version/configuration strings, native identifiers and
model names are replaced. Equal native identifiers retain equal placeholders
across both files. Clocks are shifted together to 2026-01-01 and retained to
microsecond precision. Protocol enums, counters, ordering, null fields and
record shapes remain, except opaque configuration strings become placeholders.
No private prompt, response, path or credential is included.

`TestGenuinePairedAssistantExcerpt` checks the two logical blocks, matching
native aliases and snapshot clocks, absent native stream clocks, capture clocks,
response usage convergence and separate result model buckets. File-only
`end_turn` gives candidate finality; the wire's native completed result gives
final finality. The file and wire do not have identical evidence.

This pair exposed two reader omissions, reproduced before the fix:
`request_id` was not an API request alias, and `terminal_reason: completed`
was not recognized as successful native completion. They are recorded as
CLAUDE-PAIR-1 in the provider README. The 122 original research cases remain
unchanged; this is additional evidence.
