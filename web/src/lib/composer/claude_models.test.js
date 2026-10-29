import { describe, it, expect } from 'vitest'
import { CLAUDE_START_MODELS, claudeModelChoices } from './claude_models.js'

const INIT = {
  models: [
    { value: 'default', resolvedModel: 'claude-opus-5[1m]', displayName: 'Default (recommended)' },
    { value: 'haiku', resolvedModel: 'claude-haiku-4-5', displayName: 'Haiku' },
    { value: 'claude-fable-5', resolvedModel: 'claude-fable-5', displayName: 'claude-fable-5' },
    { displayName: 'nameless' },
  ],
}

describe('claude model catalog', () => {
  it('names the concrete model an alias resolves to', () => {
    const choices = claudeModelChoices(INIT)
    // "Default (recommended)" alone does not say what will actually run.
    expect(choices[0]).toEqual({ value: 'default', label: 'Default (recommended) — claude-opus-5[1m]' })
    expect(choices[1].label).toBe('Haiku — claude-haiku-4-5')
  })

  it('does not repeat a model that already names itself', () => {
    expect(claudeModelChoices(INIT)[2].label).toBe('claude-fable-5')
  })

  it('keeps every existing start alias and adds exact versioned choices', () => {
    expect(CLAUDE_START_MODELS.map((choice) => choice.value)).toEqual([
      'opus',
      'claude-opus-5-5',
      'fable',
      'claude-fable-5-1',
      'claude-mythos-5-1',
      'claude-fable-5',
      'sonnet',
      'haiku',
    ])
  })

  it('offers Opus 5.5 as one ordinary choice with no 1M duplicate', () => {
    const startOpus55 = CLAUDE_START_MODELS.filter((choice) => /opus-5-5|opus 5\.5/i.test(`${choice.value} ${choice.label}`))
    expect(startOpus55).toEqual([{ value: 'claude-opus-5-5', label: 'Opus 5.5' }])
    const live = claudeModelChoices({
      models: [
        { value: 'claude-opus-5-5[1m]', resolvedModel: 'claude-opus-5-5[1m]', displayName: 'Opus 5.5 (1M context)' },
        { value: 'claude-opus-5-5', resolvedModel: 'claude-opus-5-5', displayName: 'Opus 5.5' },
        { value: 'sonnet', resolvedModel: 'claude-sonnet-5', displayName: 'Sonnet' },
      ],
    })
    expect(live).toEqual([{ value: 'claude-opus-5-5', label: 'Opus 5.5' }, { value: 'sonnet', label: 'Sonnet — claude-sonnet-5' }])
    expect(live.some((choice) => /1m/i.test(choice.label) && /5\.5/.test(choice.label))).toBe(false)
  })

  it('drops provider entries with no value — an option nano cannot send is worse than none', () => {
    expect(claudeModelChoices(INIT).some((choice) => choice.label === 'nameless')).toBe(false)
    expect(claudeModelChoices(INIT)).toHaveLength(3)
  })

  it('is empty before the control handshake lands, so the pane simply shows no switcher', () => {
    expect(claudeModelChoices(null)).toEqual([])
    expect(claudeModelChoices({})).toEqual([])
  })
})
