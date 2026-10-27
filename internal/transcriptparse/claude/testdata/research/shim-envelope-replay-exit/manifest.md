# shim-envelope-replay-exit

Provider: Claude Code.

Provider version: not pinned; constructed from inspected code.

Source classification: **constructed example**.

Shapes: shim-envelope-replay-exit.

Constructed exact spool envelope. seq/ts are Nano shim fields, not Claude native ids/time. Replaying an envelope preserves original observed time. Process exit after success does not change that turn outcome. A mid-turn exit without result instead yields unknown/interrupted evidence depending on exit cause.

Expected output is a research proposal, not an assertion that Nano passes this case. Null observation time means the fixture supplies no capture clock. Native ids and derived aliases remain separate. Hashes refer to scrubbed input bytes, never private source bytes.
