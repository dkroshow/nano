# Fixture contract

These 215 cases are short research fixtures for a proposed standard, not snapshots of current Nano output. Each case contains input.jsonl, expected.json and manifest.md. A manifest says constructed or scrubbed capture and identifies limitations.

The file-* cases cover every primary observed shape and additional code/fixture-supported shapes. wire-* covers all 68 schema notifications plus all 18 ThreadItem variants. request-* covers all ten server requests. edge-* and capture-* exercise cross-record and version cases. sidecar-* covers both JSONL metadata files. paired-first-turn contains a genuine 34-frame/12-record pair.

Expected fields use the supplied draft plus proposed observations. Items have a stable fixture-local ID; aliases retain native versus derived origin. Null means no evidence. Turns may lack native identity in one-row excerpts. observation-only cases intentionally express unresolved updates or source facts rather than inventing a completed Item. A fixture is an oracle proposal, not proof that a future parser passes it.

Validation checks JSON readability, item IDs and required model fields, schema compliance for 96 baseline wire/request/item specimens, 25 known-method edge frames and eight reply payloads and 16 raw ResponseItem specimens, pair correspondence, and fixture presence for documented cases. It does not run Nano or prove real provider emission for constructed examples. Oversize and physical rewrite scenarios are recipes and were not executed.

The schema-response-item-* cases cover all 16 raw ResponseItem alternatives admitted by thread/inject_items. A constructed rollout envelope around them does not prove historical emission. Five of these tags were absent from the bounded corpus and are explicitly labeled schema-only in the references.
