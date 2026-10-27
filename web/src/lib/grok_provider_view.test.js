// @vitest-environment jsdom
//
// Grok's live Session view identity: the right tab must read "Under the Hood"
// (not Codex's "Responses Input" or a fallen-through Claude/Codex-shaped
// panel), and it must never reach for the codex-app-server-only MCP registry
// call (mcp_inventory.go) that ResponsesInput's Codex branch triggers.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render } from '@testing-library/svelte'
import SessionView from './SessionView.svelte'
import ResponsesInput from './ResponsesInput.svelte'
import { getSessionStore, dropSessionStore } from '../state.svelte.js'

class MockEventSource {
  constructor(url) {
    this.url = url
  }
  close() {}
}

function jsonResponse(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

const info = { provider: 'grok', status: 'ready', cwd: '/work/proj', model: '', spawnMode: 'start' }

let fetchMock

beforeEach(() => {
  vi.stubGlobal('EventSource', MockEventSource)
  fetchMock = vi.fn(async () => jsonResponse({}))
  vi.stubGlobal('fetch', fetchMock)
  localStorage.clear()
})

afterEach(() => {
  cleanup()
  dropSessionStore('grok-view-1')
  vi.unstubAllGlobals()
})

describe('grok provider identity in the live session view', () => {
  it('labels the right tab "Under the Hood", not "Responses Input"', () => {
    const session = getSessionStore('grok-view-1')
    session.applySummary({ status: 'ready', provider: 'grok', threadId: 'nat-grok' })

    const { getByRole, queryByRole } = render(SessionView, { info, session })

    expect(getByRole('button', { name: 'Under the Hood' })).toBeTruthy()
    expect(queryByRole('button', { name: 'Responses Input' })).toBeNull()
  })

  it('renders grok-specific runtime evidence instead of falling into the Codex panel', () => {
    const session = getSessionStore('grok-view-1')
    session.applySummary({ status: 'ready', provider: 'grok', threadId: 'nat-grok' })

    const { container } = render(SessionView, { info, session })

    expect(container.textContent).toContain('durable native leader')
    expect(container.textContent).toContain('permissions are requested by Grok')
    expect(container.textContent).not.toContain('Model Instructions')
    expect(container.textContent).not.toContain('Tool Registry')
  })

  it('never fetches the codex-only MCP tool registry for a grok session', async () => {
    const session = getSessionStore('grok-view-1')
    session.applySummary({ status: 'ready', provider: 'grok', threadId: 'nat-grok' })

    session.loadMcpServers()
    await Promise.resolve()

    expect(fetchMock.mock.calls.some(([url]) => String(url).includes('/mcp'))).toBe(false)
  })

  // The effort comes from the store's accepted hosted metadata, not the
  // /state snapshot's copy, which a later metadata read can supersede.
  it('shows provider-effective effort and flags a requested/effective conflict', () => {
    const session = {
      store: {
        provider: 'grok',
        items: [],
        hostedMetadata: {
          revision: 2,
          fields: {
            effort: {
              desiredValue: 'xhigh',
              desiredDisposition: 'requested',
              effectiveValue: 'high',
              effectiveDisposition: 'confirmed',
            },
          },
        },
        meta: {
          cwd: '/work/proj',
          threadId: 'nat-grok',
          turnOptions: { effort: 'xhigh', threadModel: 'grok-4.7' },
          metadata: {
            revision: 1,
            fields: { effort: { desiredValue: 'high', effectiveValue: 'high', effectiveDisposition: 'confirmed' } },
          },
          grokSession: {
            models: { currentModelId: 'grok-4.7', availableModels: [] },
            _meta: {
              'x.ai/sessionConfig': {
                options: [
                  { category: 'mode', id: 'xhigh', selected: false },
                  { category: 'mode', id: 'high', selected: true },
                ],
              },
            },
          },
        },
      },
    }

    const { container } = render(ResponsesInput, { session })

    expect(container.textContent).toContain('effective high')
    expect(container.textContent).toContain('requested xhigh')
    expect(container.textContent).toContain('conflict')
    expect(container.textContent).toContain('grok-4.7')
  })
})
