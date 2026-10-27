## What
The assembled system prompt — the core of a "Model Instructions" panel — is
provider-asymmetric ground truth:

- **Codex**: the rollout's `session_meta.base_instructions` holds the literal
  "You are Codex…" prompt. Fully recoverable; nano can render it.
- **Claude**: exposed on NO surface. Not in `system.init` on the stream-json
  wire (no prompt field), not in the native transcript (verified: only
  tool/agent/skill inventory attachments, no base_instructions). Claude Code
  assembles it internally and persists nothing.

Nano's Model Instructions panel therefore shows a tiered input view:
wire-authoritative instruction-layer config (`output_style`, memory dir) plus
CLAUDE.md / CLAUDE.local.md files that are *documented-expected* by Claude
Code's loading rules when present. The wire still never confirms which
instruction files were actually assembled into the prompt, so those rows must
remain distinct from Codex's `instructionSources` and from any claim to have
the final prompt text.

## Why
An earlier UI revision claimed "ground truth lives in the native transcript"
for Claude — false, written by analogy to the Codex rollout. This is a
structural limit, not an engineering gap: only Claude Code adding a surface
would close it. Input visibility is therefore provider-specific.

## Alternatives rejected
- Reconstructing the Claude system prompt from CLAUDE.md discovery and
  presenting it as ground truth — rejected as client-side inference dressed as
  truth (the exact anti-pattern this project forbids). We do reconstruct
  documented-expected instruction file inputs from Claude Code's published
  loading rules, but label them separately from wire-attested inputs.
