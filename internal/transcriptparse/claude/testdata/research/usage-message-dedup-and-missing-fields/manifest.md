# usage-message-dedup-and-missing-fields

Provider: Claude Code.

Provider version: not pinned; constructed from inspected code.

Source classification: **constructed example**.

Shapes: usage-message-dedup-and-missing-fields.

Distinct API messages add within a conversation turn; repeated snapshots of m1 replace its prior usage. Null/missing are not zeros. Preserve coverage status because summing observed subsets otherwise looks like a complete total.

Expected output is a research proposal, not an assertion that Nano passes this case. Null observation time means the fixture supplies no capture clock. Native ids and derived aliases remain separate. Hashes refer to scrubbed input bytes, never private source bytes.
