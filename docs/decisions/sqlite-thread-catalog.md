# SQLite stores the local catalog

Nano uses SQLite for its local catalog and durable feature stores. One server
owns runtime writes and exposes data through HTTP. WAL mode supports readers
while the server serializes short write transactions. No external database
service is required.

`history.sqlite` is a derived index of provider transcripts and the names
file. Hosted identities, prompt state, mail, pins and stars are durable data
with separate ownership; rebuilding history does not rebuild those facts.
A provider file that disappeared may leave its only indexed copy in history,
so preserve the catalog when that history matters.

Superseded transcript projections are collected only when a newer active
generation exists. The collector removes large generations in bounded
transactions and releases the writer between batches. It does not vacuum the
database. `nano-threads history-collect --db PATH --dry-run` previews candidates;
the explicit maintenance command rechecks each candidate before changing it.

Multiple independent writers or direct remote database access would change
these assumptions and require a fresh storage design. Current clients use
Nano's HTTP interface instead.
