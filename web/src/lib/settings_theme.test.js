// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte'
import SettingsMenu from './SettingsMenu.svelte'

beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({ remote: false, authenticated: false }),
  }))
  localStorage.clear()
  document.documentElement.classList.remove('light')
  const meta = document.createElement('meta')
  meta.name = 'theme-color'
  meta.content = '#191a1b'
  document.head.append(meta)
})

afterEach(() => {
  cleanup()
  document.querySelector('meta[name="theme-color"]')?.remove()
  document.documentElement.classList.remove('light')
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('settings menu', () => {
  it('offers one-click locking only on an authenticated remote session', async () => {
    fetch.mockResolvedValue({
      ok: true,
      json: async () => ({ remote: true, authenticated: true }),
    })
    const { getByRole } = render(SettingsMenu)
    await waitFor(() => expect(fetch).toHaveBeenCalledWith('/auth/status', {
      headers: { accept: 'application/json' },
    }))
    await fireEvent.click(getByRole('button', { name: 'Settings' }))

    const lock = await waitFor(() => getByRole('button', { name: 'Lock Nano' }))
    expect(lock.closest('form')?.getAttribute('method')).toBe('post')
    expect(lock.closest('form')?.getAttribute('action')).toBe('/auth/logout')
  })

  it('links to the remote security controls', async () => {
    const { getByRole } = render(SettingsMenu)
    await fireEvent.click(getByRole('button', { name: 'Settings' }))

    expect(getByRole('link', { name: 'Remote Security' }).getAttribute('href')).toBe('/security')
  })

  it('switches to V4 light mode and persists the choice', async () => {
    const { getByRole } = render(SettingsMenu)
    await fireEvent.click(getByRole('button', { name: 'Settings' }))

    const toggle = getByRole('button', { name: 'Dark' })
    await fireEvent.click(toggle)

    expect(toggle.textContent).toBe('Light')
    expect(document.documentElement.classList.contains('light')).toBe(true)
    expect(document.querySelector('meta[name="theme-color"]')?.content).toBe('#f5f5f5')
    expect(localStorage.getItem('nano-theme')).toBe('light')
  })
})
