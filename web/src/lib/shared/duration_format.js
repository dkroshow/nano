// Both duration formatters accept milliseconds. Round once before choosing
// a display band so rollover never renders values such as "1m 60s".
const MS_PER_SECOND = 1000
const SECONDS_PER_MINUTE = 60
const SECONDS_PER_HOUR = 3600
// Below ten seconds a live clock reads better with a tenth: it is visibly
// moving even when the work is short.
const DECIMAL_BAND_TENTHS = 100

// A duration is never negative and never unknown-but-rendered: a bad input
// collapses to zero rather than to NaN or to a negative sign (D2 — no surface
// may render from something the server did not provide, and "-3s" is exactly
// the kind of nonsense a clock-skew or a missing anchor produces).
function clampMs(ms) {
  const value = Number(ms)
  return Number.isFinite(value) && value > 0 ? value : 0
}

function minuteParts(seconds) {
  return [Math.floor(seconds / SECONDS_PER_MINUTE), seconds % SECONDS_PER_MINUTE]
}

function hourParts(seconds) {
  return [Math.floor(seconds / SECONDS_PER_HOUR), Math.floor((seconds % SECONDS_PER_HOUR) / SECONDS_PER_MINUTE)]
}

// Live elapsed: what a ticking timer shows. Seconds are always present below
// the hour, because a clock that stops moving reads as a frozen session.
export function fmtDuration(ms) {
  const value = clampMs(ms)
  const tenths = Math.round(value / 100)
  if (tenths < DECIMAL_BAND_TENTHS) return `${(tenths / 10).toFixed(1)}s`
  const seconds = Math.round(value / MS_PER_SECOND)
  if (seconds < SECONDS_PER_MINUTE) return `${seconds}s`
  if (seconds < SECONDS_PER_HOUR) {
    const [minutes, rest] = minuteParts(seconds)
    return `${minutes}m ${rest}s`
  }
  const [hours, minutes] = hourParts(seconds)
  return `${hours}h ${minutes}m`
}

// Historical duration: what a finished turn or thread took. Whole seconds only
// — a tenth is precision the projection's evidence does not warrant — and a
// zero remainder is dropped, so a five-minute turn reads "5m", not "5m 0s".
// Under one second renders NOTHING: the empty string is the caller's signal to
// omit the parenthetical entirely rather than clutter the transcript with "0s".
export function fmtTurnDuration(ms) {
  const value = clampMs(ms)
  if (value < MS_PER_SECOND) return ''
  const seconds = Math.round(value / MS_PER_SECOND)
  if (seconds < SECONDS_PER_MINUTE) return `${seconds}s`
  if (seconds < SECONDS_PER_HOUR) {
    const [minutes, rest] = minuteParts(seconds)
    return rest ? `${minutes}m ${rest}s` : `${minutes}m`
  }
  const [hours, minutes] = hourParts(seconds)
  return minutes ? `${hours}h ${minutes}m` : `${hours}h`
}
