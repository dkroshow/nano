// The Claude model catalog. A live session uses the control channel's
// `initialize` response; New Window adds exact versioned choices Nano promises
// to keep available for cost/capability comparisons.
//
// Claude Code 2.1.257 advertises only moving aliases in initialize: Fable
// resolves to Fable 5.1, while Fable 5 and approved-org Mythos 5.1 are absent.
// The versioned ids below come from that installed CLI's own model constants.

const PINNED_CLAUDE_MODELS = [
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
// {value, label} pairs the pane's switcher renders. `resolvedModel` is folded
// into the label because the catalog's aliases ("default", "opus[1m]") are not
// self-explanatory — the operator needs to see which concrete model an alias
// lands on.
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
  return choices
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
