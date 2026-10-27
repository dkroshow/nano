# Duplicate-messages gate fixtures

These files feed `duplicate_gate_test.go` and `duplicate_gate_codex_test.go`.
The fixtures preserve observed provider record shapes, but use synthetic
identities, paths and prompts. Repeated text remains repeated: distinct message
identities must survive, while duplicate representations of one message merge.
The historical line hashes below describe the pre-scrub source shape only;
they are not hashes of the synthetic fixture bytes.

## Positive controls: equal messages that must stay separate

`codex-equal-replies-distinct.jsonl` — one turn in which Codex said
"T–Z has no failure output after one minute." twice, 22 minutes apart, with
different native ids.

```text
source: ~/.codex/sessions/2026/09/11/rollout-2026-09-11T23-54-11-5bbbb199-a4bf-5277-a168-8a1dab42d235.jsonl
line 1    6d3bda998198aa1b (session_meta trimmed)
line 318  e5bc726b1b0c23e2
line 319  650696d64b63c6fb
line 320  2abddc8cebff865f
line 321  2f485a0509a650fd
line 601  5ce8eeafb54aac8e
line 602  43f8375f231cd9ab (encrypted_content replaced)
line 603  b43862fc1cf7aa95
line 604  0fadede8a82314ca
line 981  eb6c61a70e4efaaf
line 982  30cb3975a0c7089f (encrypted_content replaced)
line 983  bab5daa4c23aa249
line 984  7c95a36c254fff5b
line 3556 6aeda2dd1b3bb354
```

`claude-repeated-tick.jsonl` — two `/loop` ticks in which Claude said
"Running the tick." (the Astra audit's example). Only the `user`,
`assistant` and `system` rows of source lines 4197–4265 are kept.

```text
source: ~/.claude/projects/-home-user-nano/dcee0889-2586-5d7b-a5dd-62b2866262ea.jsonl
line 4197 ec4e9450bfc1d7e8
line 4198 cd1eeecdc639fa58 (meta text trimmed)
line 4200 ffccf9b60920e00b
line 4201 8b5de42f3088a884
line 4202 43c4aadbeae0b4f6 (tool output trimmed)
line 4217 4b3c7f806b4f47d5 (thinking signature replaced)
line 4218 a7cf6bcb2d3f6c1a
line 4219 4daa9331309725a0 (tool output trimmed)
line 4226 889d85e88873eb2b (thinking signature replaced)
line 4227 d64162e781b87f9d
line 4233 419d22b5b5ce0083
line 4240 ba34bbc926b4753c
line 4241 92389f02b73710e0
line 4248 1d877ec35f5e0b56 (thinking signature replaced)
line 4249 893c459a86d7c711
line 4250 acd4163f8bffa9c9
line 4251 68034b315f4dfaa1
line 4254 b0e88232f4cc3e77
line 4256 771e07e98eae56f2
line 4257 241052fee163feb7 (meta text trimmed)
line 4264 d2fe6ac5e249f390
line 4265 1d55e72d68f3ed9a
```

## Codex serialization twins that must be served once

Codex writes each item twice: an event copy and a response copy. These three
files hold the three pairs reader v36 joins. On main `0cf5b1b` each pair is
served as two rows.

`codex-agent-message-local-id.jsonl` — pair 1: a completed `AgentMessage`
with a local id (`item-16`) and its response message (`msg_…`),
"Yes—it finished.". The split-append test cuts this file before line 423.

```text
source: ~/.codex/sessions/2026/09/15/rollout-2026-09-15T12-25-02-40b7284b-c7dd-56b1-a8d1-f5f536dbe04c.jsonl
line 1   9a91facd296c1071 (session_meta trimmed)
line 418 3333fb0986179beb
line 419 193c833a4a3643d9
line 420 488e936205626933
line 421 f599d552ca13d6b5
line 422 fb7de69023e45b02
line 423 a8b56b9d65c560c9
line 424 849ab4fbc6463f70
line 425 1edb908545005e58
```

`codex-agent-reasoning-event.jsonl` — pair 2: legacy `agent_reasoning`
events, each followed by a one-summary response reasoning with the same text.

```text
source: ~/.codex/sessions/2026/08/19/rollout-2026-08-19T21-29-52-397cb5c8-3ab7-5c8f-a6b1-566ced0e6b00.jsonl
line 1     e3c2a5d51f33e3e1 (session_meta trimmed)
line 58604 d5e28058bb2d7c24
line 58605 4faba45638f7e245
line 58606 94414abe8e115b52
line 58607 6bf3bb6a59b22f6a
line 58608 69207d4066ba3d39
line 58609 42dcaec64fedead2 (encrypted_content replaced)
line 58610 e3bd485dc3b7c424
line 58611 7cf651f7a8ec8da3
line 58634 e273cd52b007e4a1
line 58635 9fd26f9089553948 (encrypted_content replaced)
line 58651 2db83664c3337d16
line 58652 dbe50942731c4de2
line 58655 d8ddfe4021594c5f
```

`codex-completed-reasoning-local-id.jsonl` — pair 3: completed `Reasoning`
items with local ids (`item-2`, which Codex completed twice as its summary
grew, and `item-4`) and their response reasoning. It also holds two more
pair-1 messages (`item-3`, `item-5`).

```text
source: ~/.codex/sessions/2026/09/01/rollout-2026-09-01T08-07-09-c6f79940-ea48-5a90-98eb-cf99c8578e04.jsonl
line 1   ecc63278608fc38b (session_meta trimmed)
line 2   cc26f5664877e00a
line 8   eeb0578effc37c4d
line 9   a871f0279c176d87
line 10  0bd3e2887b93bcb5
line 11  9d4597f57b88781b
line 12  1ee327c48288ced0
line 13  6c0e3616b0c8dc96 (encrypted_content replaced)
line 14  3d94dc6b3028318f
line 15  b84b36b07fdbf57a
line 19  875d2b74f2b8b9f5
line 20  76fb237cde7cb95c (encrypted_content replaced)
line 21  59295761c96a895b
line 22  b993f3938b838cca
line 111 73308f6daf11088f
```
