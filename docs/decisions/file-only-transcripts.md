
## What

For every Claude, Codex and Grok thread, rows come only from the provider's
saved file, through the catalog's
incremental tail. Nothing from the live stream is stored, merged
into rows or served to agents.

The live stream adds **live text**: the reply being streamed and the prompts
the outbox holds that the file does not have yet. One loop per thread
(`live_text.go`) orders live stream changes, outbox changes and catalog
applies. After an apply, new live text or a new pane, one tick reads every
open pane's newest page and matches saved rows to live text by exact
identity only:

- Claude reply: API message id plus block index.
- Claude prompt: the UUID Nano stamps on each submission.
- Codex prompt: the native turn id its delivery binds it to; the catalog
  finds the saved prompt row in the turn with that id.
- Codex reply: the item id, which the live wire and the saved file share.
- Grok prompt: Nano's submission UUID on the text block, indexed as
  `nano.submissionId`; the native prompt id naming its saved turn also proves
  older prompts that carry no submission key.
- Grok reply: the newest chunk's eventId. The live entry keeps its first
  chunk's eventId as its stable id. Thinking, tools, a new response or a turn
  end ends the segment and checks its final key. A segment starts live only
  at chunkId 1 or after its preceding chunk was seen in that response.

The tick asks, in the same catalog transaction as every pane's page, which
keys saved rows carry. One answer serves every pane, so a saved row and the
removal of its live text travel in one message: a pane never shows both and
never neither. An earlier design gave each pane its own proof and retired
text only when every pane agreed; its bookkeeping failed four review rounds,
and one snapshot per tick removed it. Each
pane opens its own stream (`GET /api/v1/hosted-threads/{ref}/live`) and the
browser matches nothing.
Every connect starts with a fresh tail (the newest page plus live text) that
replaces the pane's rows in one render. That one rule covers reconnect,
sleep, restart, deploy, rebuild, session change, switch-on, a profile switch
and a stream that falls 1 MB behind. Older pages are plain requests.

## Why

The saved/live join (owner window, boundary B, held and frozen rows, pending
prefixes) kept failing at its seams: rows lost, doubled or reordered across
releases, restarts and rebuilds. The saved file is already the authority, and
the file-write test showed both providers save each message within 0.2 s of
its end with the same ids the live stream carries. Matching by those ids
inside one ordered loop removes every join rule the browser and server had.

## Current behavior

All providers always read saved files. `-file-transcripts` remains an inert
compatibility flag. Reverting this behavior requires a code change.
Operation answers are read from saved rows. An answer missing after the bounded
indexing wait reports `result_not_saved`; a later catalog commit can resolve it.

## Known limits

- Lag is set by the catalog's freshness delay (about 3 s), not by the file
  write. A prompt or finished reply stays as live text until its saved row
  arrives; the browser records `file.lag.prompt` (L1, send to saved prompt
  row) and `file.lag.reply` (L2, last reply text to its saved row).
- An ended reply nobody saved stays, also across a reload, until the saved
  row of the next prompt; a loop stop or restart drops it. Codex saves no
  reply that Stop cut, so for Codex a reload of a browser pane also clears it
  once the reload's own read shows the file recorded that turn's end.
- Grok may save a Stop-cut reply only when the next prompt arrives or the
  session ends. Its live text stays through reload until matched, without a
  saved-turn-end clear. A loop stop or restart loses it until it is saved;
  agent reads show saved rows only. Native load replay creates no live text.
- A Grok saved row's notice can beat its final live chunk. Until that chunk
  supplies the saved row's exact key, the saved row and live prefix can both
  show. One read at segment end, normally at turn end, settles them without
  a gap using the final chunk's key; later chunks do not read the catalog.
  If the final chunk never arrives, the unmatched prefix stays until a later
  delivered prompt is saved, or the loop stops or restarts. This is an exception
  to the ordinary atomic handoff, not permission to match by text, time or position.
- Grok's block metadata and newest-chunk eventId behavior were measured on
  1.0.40 and are undocumented. After an upgrade, repeat the real served
  prompt/reply/Stop check before relying on these keys; a changed key is a
  finding, never permission to match by text or position.
- A finished reply with no saved row 20 s later is recorded as
  `file.reply.unsaved`. The check reads the catalog itself, so the next
  prompt or a reload never hides it.
