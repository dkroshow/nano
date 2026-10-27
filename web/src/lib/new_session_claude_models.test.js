// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte'
import NewSessionModal from './NewSessionModal.svelte'
import { app } from '../app.svelte.js'
import { explorer } from '../explorer.svelte.js'
import { __resetCodexModelsForTest } from './codex_models.js'
import { __resetGrokModelsForTest } from './grok_models.js'

function response(body) {
  return { ok: true, status: 200, json: async () => body, text: async () => '' }
}

let previews

beforeEach(() => {
  __resetCodexModelsForTest()
  __resetGrokModelsForTest()
  localStorage.clear()
  previews = []
  app.defaults = { cwd: '', model: '', sandbox: 'read-only', approval: 'never', allowedTools: '' }
  app.sessions = []
  explorer.dataset = []
  vi.stubGlobal('fetch', vi.fn(async (url, init = {}) => {
    if (String(url).includes('/api/codex/models')) return response({ data: [] })
    if (String(url).includes('/api/grok/models')) return response({ data: [] })
    if (String(url).includes('/api/sessions/preview')) {
      previews.push(JSON.parse(init.body))
      return response({ provider: 'claude', argv: [] })
    }
    return response({})
  }))
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('Claude models in New Window', () => {
  it('preserves existing aliases and exposes exact Fable 5.1, Mythos 5.1, and Fable 5 identifiers', async () => {
    const { getByLabelText } = render(NewSessionModal, { onClose: vi.fn() })
    await fireEvent.click(getByLabelText('Claude Code'))

    const model = getByLabelText('Model')
    expect([...model.options].map((option) => option.value)).toEqual([
      'opus',
      'claude-opus-5-5',
      'fable',
      'claude-fable-5-1',
      'claude-mythos-5-1',
      'claude-fable-5',
      'sonnet',
      'haiku',
    ])
    expect([...model.options].map((option) => option.textContent)).toEqual([
      'Opus',
      'Opus 5.5',
      'Fable (latest alias)',
      'Fable 5.1 — claude-fable-5-1',
      'Mythos 5.1 — claude-mythos-5-1',
      'Fable 5 — claude-fable-5',
      'Sonnet',
      'Haiku',
    ])

    await fireEvent.change(model, { target: { value: 'claude-mythos-5-1' } })
    await waitFor(() => expect(previews.at(-1)?.model).toBe('claude-mythos-5-1'))
    await fireEvent.change(model, { target: { value: 'claude-opus-5-5' } })
    await waitFor(() => expect(previews.at(-1)?.model).toBe('claude-opus-5-5'))
  })
})
