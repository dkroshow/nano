// @vitest-environment jsdom
//
// The one-time reset's own expiry must appear in the mounted dialog. It must
// never be substituted with the separate usage-window reset time.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, waitFor } from '@testing-library/svelte'
import UsageDetail from './UsageDetail.svelte'

function jsonResponse(body) {
  return { ok: true, json: async () => body }
}

afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('usage detail', () => {
  it('renders Grok’s monthly series in the account usage modal', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date(2026, 7, 12, 8, 0, 0))
    vi.stubGlobal('fetch', vi.fn(async () => jsonResponse({
      providers: [{
        provider: 'grok',
        planType: 'X Premium+',
        buckets: [{
          key: 'grok:primary', label: 'month', group: 'monthly',
          usedPercent: 42, windowMinutes: 43200, resetsAt: 1789000000, primary: true,
        }],
      }, {
        provider: 'codex',
        planType: 'Pro',
        buckets: [{
          key: 'codex:primary', label: 'week', group: 'weekly',
          usedPercent: 15, resetsAt: 1789000000, primary: true,
        }, {
          key: 'codex_bengalfox:primary', label: 'GPT-5.3-Codex-Spark', group: 'weekly',
          usedPercent: 1, resetsAt: 1789000000,
        }, {
          key: 'codex_bengalfox:secondary', label: 'GPT-5.3-Codex-Spark', group: 'weekly',
          usedPercent: 1, resetsAt: 1789000000,
        }],
      }],
      series: [{
        provider: 'grok', key: 'grok:primary', label: 'month', group: 'monthly',
        points: [{ at: '2026-08-13T12:00:00Z', percent: 42 }],
      }, {
        provider: 'codex', key: 'codex:primary', label: 'week', group: 'weekly',
        points: [{ at: '2026-08-13T12:00:00Z', percent: 15 }],
      }, {
        provider: 'codex', key: 'codex_bengalfox:primary', label: 'GPT-5.3-Codex-Spark', group: 'weekly',
        points: [{ at: '2026-08-13T12:00:00Z', percent: 1 }],
      }, {
        provider: 'codex', key: 'codex_bengalfox:secondary', label: 'GPT-5.3-Codex-Spark', group: 'weekly',
        points: [{ at: '2026-08-13T12:00:00Z', percent: 1 }],
      }],
      attempts: [{ at: '2026-08-13T12:00:00Z', provider: 'grok', ok: true, durationMs: 12, buckets: 1 }],
      pollSeconds: 300,
    })))

    const screen = render(UsageDetail, { open: true, onClose: () => {} })

    await waitFor(() => expect(screen.getByRole('img', { name: /grok limit usage/i })).toBeTruthy())
    const grokChart = screen.getByRole('img', { name: /grok limit usage/i })
    expect(grokChart.getAttribute('width')).toBe('560')
    expect(grokChart.getAttribute('height')).toBe('170')
    expect(grokChart.textContent).toMatch(/%/)
    expect(grokChart.textContent).not.toMatch(/\dAM|\dPM/)
    expect(grokChart.querySelector('.ud-reset-label')).toBeNull()
    const charts = screen.getByTestId('usage-charts')
    expect(charts).toBeTruthy()
    expect(charts.contains(screen.getByRole('button', { name: '3d' }))).toBe(true)
    expect(screen.getByRole('button', { name: '3d' }).classList.contains('on')).toBe(true)
    expect(fetch).toHaveBeenCalledWith('/api/v1/usage-limits/detail?hours=72')
    const resets = screen.getByRole('heading', { name: 'Resets' }).closest('section')
    expect(resets.textContent).toMatch(/grok\s+month/)
    expect(resets.textContent).toMatch(/in \d+d/)
    const codexChart = screen.getByRole('img', { name: /codex limit usage/i })
    expect(codexChart.getAttribute('height')).toBe('170')
    expect(charts.textContent).not.toContain('GPT-5.3-Codex-Spark')
    expect(screen.getByTestId('usage-information')).toBeTruthy()
    expect(screen.getByText('month 42%')).toBeTruthy()
    expect(screen.getByText(/1\/1 ok/)).toBeTruthy()
  })

  // Catches a future reset stretching the x-axis, which parks the live
  // reading left of now. The plot's right edge is now.
  it('places the live reading on the right edge when the reset is days away', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    const now = new Date(2026, 9, 13, 19, 45, 0)
    vi.setSystemTime(now)
    const resetsAt = Math.floor(now.getTime() / 1000) + 5 * 24 * 60 * 60
    vi.stubGlobal('fetch', vi.fn(async () => jsonResponse({
      providers: [{
        provider: 'grok',
        planType: 'SuperGrok',
        buckets: [{
          key: 'grok:primary', label: 'week', group: 'weekly',
          usedPercent: 40, resetsAt, primary: true,
        }],
      }],
      series: [{
        provider: 'grok', key: 'grok:primary', label: 'week', group: 'weekly',
        points: [{ at: now.toISOString(), percent: 40 }],
      }],
      attempts: [],
      pollSeconds: 300,
    })))

    const screen = render(UsageDetail, { open: true, onClose: () => {} })
    await waitFor(() => expect(screen.getByRole('img', { name: /grok limit usage/i })).toBeTruthy())
    const chart = screen.getByRole('img', { name: /grok limit usage/i })
    const right = Number(chart.querySelector('.ud-grid').getAttribute('x2'))
    const dot = chart.querySelector('.ud-dot')
    expect(Number(dot.getAttribute('cx'))).toBeCloseTo(right, 0)
    expect(chart.textContent).not.toMatch(/Wed 14/)
  })

  it('shows an available Codex reset credit beside its own expiry', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date(2026, 7, 12, 8, 0, 0)) // Wednesday
    const resetAt = Math.floor(Date.now() / 1000) + 2 * 60 * 60
    vi.stubGlobal('fetch', vi.fn(async () => jsonResponse({
      providers: [{
        provider: 'codex',
        resetCredits: 1,
        resetCreditDetails: [
          { id: 'credit-1', title: 'Full reset', status: 'available', expiresAt: resetAt },
          { id: 'credit-2', title: 'Later reset', status: 'available', expiresAt: resetAt + 24 * 60 * 60 },
        ],
        buckets: [{ key: 'codex:primary', label: 'week', usedPercent: 85, resetsAt: resetAt + 5 * 24 * 60 * 60 }],
      }],
      series: [],
      attempts: [],
      pollSeconds: 300,
    })))

    const screen = render(UsageDetail, { open: true, onClose: () => {} })

    await waitFor(() => expect(screen.getByText('Full reset')).toBeTruthy())
    expect(screen.getByText('One-time usage resets')).toBeTruthy()
    expect(screen.getByText(/Expires/)).toBeTruthy()
    expect(screen.getByTestId('usage-reset-credits').textContent).toMatch(/in 2h/)
    expect(screen.getByTestId('usage-reset-credits').textContent).toContain('Today 10:00 AM')
    expect(screen.getByTestId('usage-reset-credits').textContent).toContain('Tomorrow 10:00 AM')
  })
})
