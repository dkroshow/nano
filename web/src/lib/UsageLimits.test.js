// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, waitFor } from '@testing-library/svelte'
import UsageLimits from './UsageLimits.svelte'

afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('usage limits', () => {
  it('shows Grok’s primary monthly bucket in the sidebar', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      json: async () => ({
        providers: [{
          provider: 'grok',
          planType: 'X Premium+',
          observedAt: '2026-08-13T12:00:00Z',
          buckets: [{
            key: 'grok:primary',
            label: 'month',
            group: 'monthly',
            usedPercent: 42,
            windowMinutes: 43200,
            resetsAt: 1789000000,
            primary: true,
          }],
        }],
      }),
    })))

    const screen = render(UsageLimits)

    await waitFor(() => expect(screen.getByTestId('usage-limits')).toBeTruthy())
    expect(screen.getByText('grok')).toBeTruthy()
    expect(screen.getByText('month')).toBeTruthy()
    expect(screen.getByText('42%')).toBeTruthy()
  })
})
