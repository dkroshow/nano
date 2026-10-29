# Grok 1.0.40 file/live keys

`grok-turns.wire.jsonl` is a small subset of the recorded
`probe3-exact-block-think-stop-wire.jsonl`. It keeps the last two thinking
chunks, first three reply chunks and final reply chunk from each response,
the submitted text blocks and turn ends. Omitted chunks are represented by
neutral replacement text on the retained chunks, so the saved neutral text
still equals their assembled text. All ids, chunk numbers, clocks and
metadata structure stay exact.

`grok-turns.saved.jsonl` uses the real content and terminal rows from
`probe3-saved/updates.jsonl`, captured from the same session. Only text was
replaced; hook rows were omitted. The three exact Nano text-block keys and
the replies' final eventId/chunkId pairs are unchanged. The second reply was
cut by Stop at chunk 298. Tests may delay that actual saved row until the
next actual prompt row, or apply it before the final live chunk.

`grok-tools.wire.jsonl` uses the real thinking/tool/reply/terminal sequence
from `thread2-nano-wire.jsonl`. Text, tool commands and the response signature
are scrubbed; ids, chunk numbers and structure stay exact. In this captured
order, tools ended a response and the next reply began at chunk 1.

The reply/tool/reply-in-one-response case is a constructed ordering, not
observed Grok 1.0.40 behavior. It inserts those captured tool frames between
probe 3's captured reply chunks, changing only the tools' session, turn and
response scope. Reply eventIds and chunkIds remain exact. No saved row exists
for its first constructed segment; the test keeps it live until the next
saved prompt, and matches the second segment to the actual final-chunk row
with its text scrubbed for that segment.

`grok-task.saved.jsonl` is the content/terminal subset of the real six-row
`task-completed-saved/updates-excerpt.jsonl`. Text was already neutralized
in that capture; ids and structure remain exact. Its HTTP test constructs
wire envelopes by removing only the file timestamp and adding the real ACP
jsonrpc envelope field. There is no captured live task-completed turn. The
merged reply's chunk 79 has no observed predecessor, so it stays saved-only.
