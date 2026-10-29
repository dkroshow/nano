// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, waitFor, fireEvent } from '@testing-library/svelte'
import NewSessionModal from './NewSessionModal.svelte'
import { app } from '../../app.svelte.js'
import { explorer } from '../../explorer.svelte.js'
import { __resetCodexModelsForTest } from './codex_models.js'

const catalog = {
  data: [{
    id: 'gpt-5.6-sol',
    model: 'gpt-5.6-sol',
    displayName: 'GPT-5.6 Sol',
    defaultReasoningEffort: 'low',
    supportedReasoningEfforts: [{ reasoningEffort: 'low' }],
    defaultServiceTier: null,
    serviceTiers: [{ id: 'priority', name: 'Fast', description: 'Higher priority' }, { id: 'ultrafast', name: 'Ultrafast' }],
  }],
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

describe('new session priority', () => {
  it('sends explicit Standard instead of inheriting a Priority config default', async () => {
    const result = render(NewSessionModal, { onClose: vi.fn() })
    const select = await waitFor(() => result.getByLabelText('Speed'))

    expect(select.options[select.selectedIndex].textContent).toBe('Standard')
    expect(select.value).toBe('default')
    await waitFor(() => expect(previews.at(-1)?.serviceTier).toBe('default'))
    expect([...select.options].map((o) => o.value)).toEqual(['default', 'priority', 'ultrafast'])
    await fireEvent.change(select, { target: { value: 'ultrafast' } })
    await waitFor(() => expect(previews.at(-1)?.serviceTier).toBe('ultrafast'))
  })
})
