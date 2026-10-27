// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, fireEvent, waitFor } from '@testing-library/svelte'
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
    // handleGrokModels (main.go) answers non-2xx when the ACP probe fails
    // (binary missing, no auth, timeout) — exercise that real failure mode
    // rather than a fabricated empty-but-200 body.
    if (String(url).includes('/api/grok/models')) {
      return { ok: false, status: 502, json: async () => ({}), text: async () => 'start Grok model probe: exec: "grok": executable file not found in $PATH' }
    }
    if (String(url).includes('/api/sessions/preview')) {
      previews.push(JSON.parse(init.body))
      return response({ provider: JSON.parse(init.body).provider, method: 'thread/start', params: {} })
    }
    return response({})
  }))
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('grok in New Window', () => {
  it('falls back to free-text model inputs and offers per-thread permissions', async () => {
    const { getByLabelText, queryByRole } = render(NewSessionModal, { onClose: vi.fn() })

    await fireEvent.click(getByLabelText(/Grok Build/))

    // Resume remains in Explorer. The permission dial maps only the two ACP
    // session overrides Grok 1.0.3 documents; sandbox stays leader-owned.
    expect(queryByRole('radio', { name: 'Resume' })).toBeNull()
    const permissions = getByLabelText('Permissions')
    expect([...permissions.options].map((option) => option.value)).toEqual(['', 'auto', 'bypassPermissions'])

    const modelInput = await waitFor(() => getByLabelText('Model'))
    expect(modelInput.placeholder).toBe('inherit')
    const effortInput = getByLabelText('Effort')
    expect(effortInput.placeholder).toBe('inherit')

    await waitFor(() => expect(previews.at(-1)?.provider).toBe('grok'))
    const sent = previews.at(-1)
    expect(sent.mode).toBe('start')
    expect(sent).not.toHaveProperty('sandbox')
    expect(sent).not.toHaveProperty('approval')
    expect(sent).not.toHaveProperty('permissionMode')

    await fireEvent.change(permissions, { target: { value: 'bypassPermissions' } })
    await waitFor(() => expect(previews.at(-1)?.permissionMode).toBe('bypassPermissions'))
  })

  it('defaults to Grok 4.7/high while preserving older provider-advertised models and efforts', async () => {
    vi.stubGlobal('fetch', vi.fn(async (url, init = {}) => {
      if (String(url).includes('/api/codex/models')) return response({ data: [] })
      if (String(url).includes('/api/grok/models')) {
        const fullEfforts = ['xhigh', 'high', 'medium', 'low'].map((reasoningEffort) => ({ reasoningEffort }))
        return response({ data: [
          { model: 'grok-4.7', displayName: 'Grok 4.7', isDefault: true, defaultReasoningEffort: 'high', supportedReasoningEfforts: fullEfforts },
          { model: 'grok-4.7-build-fast', displayName: 'Grok 4.7 Fast', isDefault: false, defaultReasoningEffort: 'high', supportedReasoningEfforts: fullEfforts },
          { model: 'grok-4.6', displayName: 'Grok 4.6', isDefault: false, defaultReasoningEffort: 'high', supportedReasoningEfforts: fullEfforts },
          {
            model: 'grok-4.5', displayName: 'Grok 4.5', isDefault: false, defaultReasoningEffort: 'high',
            supportedReasoningEfforts: ['high', 'medium', 'low'].map((reasoningEffort) => ({ reasoningEffort })),
          },
        ] })
      }
      if (String(url).includes('/api/sessions/preview')) {
        previews.push(JSON.parse(init.body))
        return response({ provider: JSON.parse(init.body).provider, method: 'thread/start', params: {} })
      }
      return response({})
    }))

    const { getByLabelText, findByLabelText } = render(NewSessionModal, { onClose: vi.fn() })
    await fireEvent.click(getByLabelText(/Grok Build/))

    const select = await findByLabelText('Model')
    await waitFor(() => expect(select.value).toBe('grok-4.7'))
    expect([...select.options].map((option) => option.value)).toEqual(['grok-4.7', 'grok-4.7-build-fast', 'grok-4.6', 'grok-4.5'])
    const effort = getByLabelText('Effort')
    expect(effort.value).toBe('high')
    expect([...effort.options].map((option) => option.value)).toEqual(['', 'xhigh', 'high', 'medium', 'low'])

    await fireEvent.change(select, { target: { value: 'grok-4.5' } })
    expect([...effort.options].map((option) => option.value)).toEqual(['', 'high', 'medium', 'low'])
    await waitFor(() => expect(previews.at(-1)).toMatchObject({ provider: 'grok', model: 'grok-4.5', effort: 'high' }))
  })
})
