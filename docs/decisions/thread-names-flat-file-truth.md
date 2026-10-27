# Thread names remain separate from the catalog

User-assigned names live in `thread-names.json`, keyed by provider and native
thread id. The default is beside `history.sqlite`; `-thread-names` overrides
it. Writes are atomic. SQLite keeps a derived copy for queries and joins it
at read time, with the derived title as fallback.

Names are operator data; the history catalog is rebuildable from sources.
Keeping their authority separate lets a catalog rebuild preserve names.
A native thread id survives a catalog rebuild and file move, unlike a random
catalog id or a source path. Provenance distinguishes manual names, imported
names, and creation defaults so one kind cannot silently overwrite another.
