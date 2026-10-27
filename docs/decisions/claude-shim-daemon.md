# Claude shim daemon

`cmd/claude-shim` owns `claude -p` subprocesses separately from Nano. It wraps
stream-json lines with session identity and sequence numbers and serves them
over a loopback WebSocket, defaulting to `127.0.0.1:7692`. It does not translate
provider content. A Nano disconnect leaves the children running.

Nano attaches with a sequence cursor to replay the unsaved tail and unanswered
control requests. An idle attachment can start at the spool tail. The replay
reader drains the socket before applying batches so projection work cannot
block replay transport. Replayed events do not prove present activity; the
settled runtime snapshot and newer live evidence establish it.

Before Claude announces a native id, the shim handle is provisional identity.
It is replaced by native evidence without claiming a new conversation lineage.
When the shim child is gone, Nano can resume a saved native transcript with the
recorded launch posture. A never-prompted child with no saved transcript has no
such fallback. Direct child spawning remains available when the shim is not
configured or cannot be reached, with a warning about the lost durability.

The shim's `-spool-dir` defaults to `~/.nanod/claude-shim/spools`. Nano's
`-claude-shim-spool-dir` defaults below its `-data-dir`; configure both to the
same path when overriding either process. The spool is transport evidence,
not the saved transcript authority. Children die when the shim itself stops.
Provider upgrades therefore require a deliberate shim restart with no active
turns, separately from ordinary Nano restarts.
