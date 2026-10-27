# interleaved-message-ids

Provider: Claude Code.

Provider version: not pinned; constructed from inspected code.

Source classification: **constructed example**.

Shapes: interleaved-message-ids.

Constructed stress case; not observed in sampled indexed rows. Current nextClaudeContentBlock holds only the latest message id, so it resets on returning to m1. Explicit apiBlockIndex makes the intended identities unambiguous.

Expected output is a research proposal, not an assertion that Nano passes this case. Null observation time means the fixture supplies no capture clock. Native ids and derived aliases remain separate. Hashes refer to scrubbed input bytes, never private source bytes.
