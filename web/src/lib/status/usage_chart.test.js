import { describe, expect, it } from 'vitest'
import {
  usageChartXLabel,
  usageChartXTicks,
  usageChartYLabel,
} from './usage_chart.js'

function local(year, month, day, hour = 0) {
  return new Date(year, month - 1, day, hour, 0, 0, 0).getTime()
}

describe('usage chart axis labels', () => {
  it('uses midnight-aligned day labels on a multi-day runway, not reset-hour clock times', () => {
    const domain = [local(2026, 8, 16, 8), local(2026, 8, 24, 0)]
    const ticks = usageChartXTicks(domain)
    const labels = ticks.map((t) => usageChartXLabel(t, domain))

    expect(labels[0]).toBe('Mon 17')
    expect(labels.at(-1)).toBe('Mon 24')
    expect(labels.join(' ')).not.toMatch(/AM|PM/)
    expect(new Date(ticks[0]).getHours()).toBe(0)
  })

  it('uses spaced clock times on a short window', () => {
    const domain = [local(2026, 8, 19, 8), local(2026, 8, 19, 20)]
    const labels = usageChartXTicks(domain).map((t) => usageChartXLabel(t, domain))

    expect(labels).toContain('10 AM')
    expect(labels).toContain('4 PM')
    expect(labels.join(' ')).not.toMatch(/\dAM|\dPM/)
  })

  it('marks the percent scale as percents', () => {
    expect(usageChartYLabel(0)).toBe('0%')
    expect(usageChartYLabel(100)).toBe('100%')
  })
})
