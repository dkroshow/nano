# handshake-queue-responses-errors

Provider: Grok Build. Version: Unknown native build; observed corpus sampled 2026-09-07.

Origin: constructed example.

Covers: initialize model catalog, new session effective mode, queue acceptance, response terminal, unscoped prompt_complete, errors.

Pairing: No paired live capture.

Known gaps: Result ids require a request ledger. prompt_complete without sessionId may affect only an already-bound prompt. Duplicated completion observations must not double usage.

Scrubbing: private prose, argument strings, paths, ids, model keys, and opaque data use neutral placeholders. Captured arrays keep at most three members. Hashes describe scrubbed input, not private source bytes. Only minimal excerpts are retained.
