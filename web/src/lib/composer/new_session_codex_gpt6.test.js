// @vitest-environment jsdom
//
// Catches: GPT-6.1 Sol or GPT-6 Sol/Luna advertised by the app-server but not
// selectable in New Window, started with an effort Nano invented rather than
// the one the catalog advertises, or the provider's new isDefault displacing
// Nano's default. Boundary: the New Window model/effort selects and the spawn
// preview request they produce.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte'
import NewSessionModal from './NewSessionModal.svelte'
import { app } from '../../app.svelte.js'
import { explorer } from '../../explorer.svelte.js'
import { __resetCodexModelsForTest } from './codex_models.js'

const efforts = (...names) => names.map((reasoningEffort) => ({ reasoningEffort }))
const entry = (model, displayName, defaultReasoningEffort, supported, isDefault = false) => ({
  id: model, model, displayName, hidden: false, isDefault, defaultReasoningEffort, supportedReasoningEfforts: supported,
})

// Shape and values from Codex 0.159.0 model/list, 2026-09-29.
const catalog = {
  data: [
    entry('gpt-6.1-sol', 'GPT-6.1-Sol', 'low', efforts('low', 'medium', 'high', 'xhigh', 'max', 'ultra'), true),
    entry('gpt-6-astra', 'GPT-6-Astra', 'medium', efforts('low', 'medium', 'high', 'xhigh', 'max', 'ultra')),
    entry('gpt-6-sol', 'GPT-6-Sol', 'medium', efforts('low', 'medium', 'high', 'xhigh', 'max', 'ultra')),
    entry('gpt-6-luna', 'GPT-6-Luna', 'medium', efforts('low', 'medium', 'high', 'xhigh', 'max')),
    entry('gpt-5.6-sol', 'GPT-5.6-Sol', 'low', efforts('low', 'medium', 'high', 'xhigh', 'max', 'ultra')),
    entry('gpt-5.6-terra', 'GPT-5.6-Terra', 'medium', efforts('low', 'medium', 'high', 'xhigh', 'max', 'ultra')),
    entry('gpt-5.6-luna', 'GPT-5.6-Luna', 'medium', efforts('low', 'medium', 'high', 'xhigh', 'max')),
    entry('gpt-5.5', 'GPT-5.5', 'medium', efforts('low', 'medium', 'high', 'xhigh')),
  ],
}

function response(body) {
  return { ok: true, status: 200, json: async () => body, text: async () => '' }
}

let previews

beforeEach(() => {
  __resetCodexModelsForTest()
  localStorage.clear()
  previews = []
  app.defaults = { cwd: '', model: '', sandbox: 'read-only', approval: 'never', allowedTools: '' }
  app.sessions = []
  explorer.dataset = []
  vi.stubGlobal('fetch', vi.fn(async (url, init = {}) => {
    if (String(url).includes('/api/codex/models')) return response(catalog)
    if (String(url).includes('/api/sessions/preview')) {
      previews.push(JSON.parse(init.body))
      return response({ provider: 'codex', method: 'thread/start', params: {} })
    }
    return response({})
  }))
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('GPT-6.1 Sol and GPT-6 Sol and Luna in New Window', () => {
  it('lists them in order, keeps GPT-5.6 and its default, and starts each with its advertised efforts', async () => {
    const result = render(NewSessionModal, { onClose: vi.fn() })
    const model = await waitFor(() => {
      const select = result.container.querySelector('#nw-codex-model')
      expect(select).not.toBeNull()
      return select
    })
    expect([...model.options].map((o) => o.value)).toEqual([
      'gpt-6.1-sol', 'gpt-6-astra', 'gpt-6-sol', 'gpt-6-luna', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna', 'gpt-5.5',
    ])
    const effortValues = () => [...result.container.querySelector('#nw-codex-effort').options].map((o) => o.value)
    // The provider marks gpt-6.1-sol isDefault; Nano's own default, gpt-5.6-sol, still wins.
    await waitFor(() => expect(model.value).toBe('gpt-5.6-sol'))

    await fireEvent.change(model, { target: { value: 'gpt-6.1-sol' } })
    await waitFor(() => expect(previews.at(-1)).toMatchObject({ model: 'gpt-6.1-sol', effort: 'low' }))
    expect(effortValues()).toEqual(['low', 'medium', 'high', 'xhigh', 'max', 'ultra'])

    await fireEvent.change(model, { target: { value: 'gpt-6-sol' } })
    await waitFor(() => expect(previews.at(-1)).toMatchObject({ model: 'gpt-6-sol', effort: 'medium' }))
    expect(effortValues()).toEqual(['low', 'medium', 'high', 'xhigh', 'max', 'ultra'])

    await fireEvent.change(model, { target: { value: 'gpt-6-luna' } })
    await waitFor(() => expect(previews.at(-1)).toMatchObject({ model: 'gpt-6-luna', effort: 'medium' }))
    expect(effortValues()).toEqual(['low', 'medium', 'high', 'xhigh', 'max'])
  })
})
