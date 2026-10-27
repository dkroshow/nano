# wire-multiple-user-blocks

Provider: Claude Code.

Provider version: not pinned; constructed from inspected code.

Source classification: **constructed example**.

Shapes: wire-multiple-user-blocks.

One message retains ordered content parts. Current live adapter upserts the same uuid for each text block; its own state retains only the last block. Historical path emits two items without the row UUID.

Expected output is a research proposal, not an assertion that Nano passes this case. Null observation time means the fixture supplies no capture clock. Native ids and derived aliases remain separate. Hashes refer to scrubbed input bytes, never private source bytes.
