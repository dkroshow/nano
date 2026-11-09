// The Claude model catalog. Claude Code advertises only moving aliases in its
// `initialize` response, so the exact versioned ids live here, and this list is
// the one place to add a model: New Window and every live session's switcher
// offer it. Check each id with a real call (AGENTS.md, "Adding a Claude
// model"); the CLI can lag a release and warn `unrecognized_model` while the
// API still serves the id.

const PINNED_CLAUDE_MODELS = [
  { value: 'claude-fable-5-2', label: 'Fable 5.2 — claude-fable-5-2' },
  { value: 'claude-fable-5-1', label: 'Fable 5.1 — claude-fable-5-1' },
  { value: 'claude-mythos-5-1', label: 'Mythos 5.1 — claude-mythos-5-1' },
  { value: 'claude-fable-5', label: 'Fable 5 — claude-fable-5' },
]

const OPUS_5_5 = { value: 'claude-opus-5-5', label: 'Opus 5.5' }

// New Window has no child yet, so it cannot read initialize. Keep the existing
// aliases and add exact ids for the versioned choices this feature owns.
export const CLAUDE_START_MODELS = [
  { value: 'opus', label: 'Opus' },
  OPUS_5_5,
  { value: 'fable', label: 'Fable (latest alias)' },
  ...PINNED_CLAUDE_MODELS,
  { value: 'sonnet', label: 'Sonnet' },
  { value: 'haiku', label: 'Haiku' },
]

// claudeModelChoices projects the initialize response's `models` array into the
// {value, label} pairs the pane's switcher renders, then adds the pinned ids the
// CLI did not advertise: `set_model` accepts an exact id the CLI's own list
// lacks. `resolvedModel` is folded into the label because the catalog's aliases
// ("default", "opus[1m]") are not self-explanatory — the operator needs to see
// which concrete model an alias lands on.
export function claudeModelChoices(controlInit) {
  const models = controlInit?.models
  if (!Array.isArray(models)) return []
  const choices = []
  for (const m of models) {
    if (!m || typeof m.value !== 'string' || !m.value) continue
    // Every advertised Opus 5.5 variant, [1m] included, is the one ordinary choice.
    const choice = isOpus55(m) ? OPUS_5_5 : { value: m.value, label: claudeModelLabel(m) }
    if (!choices.some((existing) => existing.value === choice.value)) choices.push(choice)
  }
  return [...choices, ...PINNED_CLAUDE_MODELS.filter((pinned) => !choices.some((choice) => choice.value === pinned.value))]
}

function isOpus55(model) {
  return /opus-5-5/i.test(`${model.value} ${model.resolvedModel ?? ''}`) || /opus 5\.5/i.test(model.displayName ?? '')
}

function claudeModelLabel(model) {
  const name = model.displayName || model.value
  const resolved = model.resolvedModel
  return resolved && resolved !== model.value && !name.includes(resolved) ? `${name} — ${resolved}` : name
}

// claudeSessionModel is what "no override" means on this provider: the model the
// child was spawned with, as the session reported it.
export function claudeSessionModel(meta) {
  return meta?.turnOptions?.threadModel ?? ''
}
