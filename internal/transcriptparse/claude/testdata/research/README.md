# Claude fixture suite

122 research cases. `index.json` lists shape coverage. Each case has input.jsonl, expected.json and manifest.md. Expected JSON is the proposed standard, not a snapshot of current Nano output. Many singleton captures intentionally leave turn identity null because their neighbors are absent. Complex cases state partial assertions where a complete expected timeline is not supplied.

Only paired-split-file and paired-split-wire form an explicit same-conversation pair, and both are constructed. No genuine paired native-file/wire capture was available in the named evidence. No Claude process was launched.

Captured examples retain structural fields and short neutral replacements. Signature and image payloads are placeholders. Source aliases are hashes of paths, not resolvable private paths. Byte coordinates refer to the sampled source, but hashes in expected output refer only to scrubbed fixture input. List shortening means the captures prove shape, not original list cardinality.
