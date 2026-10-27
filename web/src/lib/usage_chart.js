const HOUR = 3600_000
const DAY = 24 * HOUR

export function usageChartXTicks([t0, t1]) {
  const span = t1 - t0
  if (!(span > 0)) return []
  if (span > 36 * HOUR) {
    const ticks = []
    const cursor = new Date(t0)
    cursor.setHours(0, 0, 0, 0)
    if (cursor.getTime() < t0) cursor.setDate(cursor.getDate() + 1)
    const days = Math.ceil(span / DAY)
    const step = days > 10 ? 2 : 1
    for (let t = cursor.getTime(); t <= t1; t += step * DAY) ticks.push(t)
    return ticks
  }
  const stepH = span > 24 * HOUR ? 12 : span > 12 * HOUR ? 4 : 2
  const cursor = new Date(t0)
  cursor.setMinutes(0, 0, 0)
  const remainder = cursor.getHours() % stepH
  if (remainder) cursor.setHours(cursor.getHours() + (stepH - remainder))
  if (cursor.getTime() < t0) cursor.setHours(cursor.getHours() + stepH)
  const ticks = []
  for (let t = cursor.getTime(); t <= t1; t += stepH * HOUR) ticks.push(t)
  return ticks
}

export function usageChartXLabel(ms, [t0, t1]) {
  const date = new Date(ms)
  if (t1 - t0 > 36 * HOUR) {
    const weekday = date.toLocaleDateString('en-US', { weekday: 'short' })
    return `${weekday} ${date.getDate()}`
  }
  const hour = date.getHours()
  const hour12 = hour % 12 || 12
  return `${hour12} ${hour >= 12 ? 'PM' : 'AM'}`
}

export function usageChartYLabel(percent) {
  return `${percent}%`
}

export const USAGE_CHART_PLOT_H = 136
export const USAGE_CHART_PAD = { right: 96, bottom: 22, left: 40 }
