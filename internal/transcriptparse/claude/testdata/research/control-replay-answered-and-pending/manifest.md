# control-replay-answered-and-pending

Provider: Claude Code.

Provider version: not pinned; constructed from inspected code.

Source classification: **constructed example**.

Shapes: control-replay-answered-and-pending.

Treat all three lines as shim replay, then replayDone. Do not open dialogs before replay finishes. Only pending1 survives. Replaying again changes no semantic items. Capture direction and seq in a real envelope; this fixture supplies replay disposition as assertions.

Expected output is a research proposal, not an assertion that Nano passes this case. Null observation time means the fixture supplies no capture clock. Native ids and derived aliases remain separate. Hashes refer to scrubbed input bytes, never private source bytes.
