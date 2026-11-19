// @vitest-environment jsdom
//
// The brand-row monitor of attached threads. It catches a count or list that
// drifts from the catalog's runtime.attached flag, the fact the attach and
// detach events update, so a leak of held agent processes stays visible.
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { render, cleanup, fireEvent } from '@testing-library/svelte'
import { flushSync } from 'svelte'
import AttachedThreads from './AttachedThreads.svelte'
import { explorer } from '../../explorer.svelte.js'

const thread = (id, provider, runtime, extra = {}) => ({
  id, provider, nativeId: `nat-${id}`, kind: 'thread', title: `Thread ${id}`,
  createdAt: '2026-07-31T08:00:00Z', updatedAt: '2026-07-31T12:00:00Z', runtime, ...extra,
})
const live = (sessionId) => ({ attached: true, sessionId, status: 'ready', turnActive: false })

beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, status: 200, json: async () => ({}), text: async () => '' })))
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  explorer.dataset = []
})

describe('attached threads monitor', () => {
  it('counts and lists only attached threads, and follows a detach', async () => {
    const parent = thread('p', 'claude', live('s_p'), { title: 'Owner thread' })
    const child = thread('c', 'codex', live('s_c'), { invokerNativeThreadId: 'nat-p', updatedAt: '2026-07-31T09:00:00Z' })
    const resting = thread('r', 'claude', { attached: false, detached: true })
    explorer.dataset = [parent, child, resting]

    const { getByTestId, getByRole } = render(AttachedThreads)
    const button = getByTestId('attached-threads')
    expect(button.textContent.replace(/\s+/g, ' ').trim()).toBe('Attached 2: Claude 1, Codex 1')

    await fireEvent.click(button)
    const items = [...getByRole('dialog').querySelectorAll('.mail-item')].map((li) => li.textContent.replace(/\s+/g, ' ').trim())
    // Longest idle first; the child names the thread that started it.
    expect(items).toHaveLength(2)
    expect(items[0]).toContain('Thread c')
    expect(items[0]).toContain('started by Owner thread')
    expect(items[1]).toContain('started by you')

    explorer.dataset = [parent, { ...child, runtime: { attached: false, detached: true } }, resting]
    flushSync()
    expect(button.textContent.replace(/\s+/g, ' ').trim()).toBe('Attached 1: Claude 1')
    expect(getByRole('dialog').querySelectorAll('.mail-item')).toHaveLength(1)
  })
})
