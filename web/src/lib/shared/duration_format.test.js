import { describe, expect, it } from 'vitest'
import { fmtDuration, fmtTurnDuration } from './duration_format.js'

describe('fmtDuration (live elapsed)', () => {
  it('reads in tenths under ten seconds', () => {
    expect(fmtDuration(0)).toBe('0.0s')
    expect(fmtDuration(450)).toBe('0.5s')
    expect(fmtDuration(9_400)).toBe('9.4s')
  })

  it('reads in whole seconds up to a minute', () => {
    expect(fmtDuration(10_000)).toBe('10s')
    expect(fmtDuration(42_000)).toBe('42s')
  })

  it('reads minutes and seconds past one minute', () => {
    expect(fmtDuration(60_000)).toBe('1m 0s')
    expect(fmtDuration(125_000)).toBe('2m 5s')
    expect(fmtDuration(3_540_000)).toBe('59m 0s')
  })

  it('reads hours and minutes past one hour', () => {
    expect(fmtDuration(3_600_000)).toBe('1h 0m')
    expect(fmtDuration(7_530_000)).toBe('2h 5m')
  })

  it('carries on rollover instead of printing an overflowed remainder', () => {
    expect(fmtDuration(9_960)).toBe('10s')
    expect(fmtDuration(59_600)).toBe('1m 0s')
    expect(fmtDuration(119_600)).toBe('2m 0s')
    expect(fmtDuration(3_599_500)).toBe('1h 0m')
  })

  it('pins the exact band boundaries', () => {
    expect(fmtDuration(9_949)).toBe('9.9s')
    expect(fmtDuration(9_950)).toBe('10s')
    expect(fmtDuration(59_499)).toBe('59s')
    expect(fmtDuration(59_500)).toBe('1m 0s')
    expect(fmtDuration(3_599_499)).toBe('59m 59s')
  })

  it('never renders a negative or unusable duration', () => {
    expect(fmtDuration(-5_000)).toBe('0.0s')
    expect(fmtDuration(NaN)).toBe('0.0s')
    expect(fmtDuration(null)).toBe('0.0s')
    expect(fmtDuration(undefined)).toBe('0.0s')
  })
})

describe('fmtTurnDuration (historical)', () => {
  it('renders nothing under one second', () => {
    expect(fmtTurnDuration(0)).toBe('')
    expect(fmtTurnDuration(999)).toBe('')
    expect(fmtTurnDuration(null)).toBe('')
    expect(fmtTurnDuration(-1)).toBe('')
  })

  it('renders whole seconds, never tenths', () => {
    expect(fmtTurnDuration(1_000)).toBe('1s')
    expect(fmtTurnDuration(1_400)).toBe('1s')
    expect(fmtTurnDuration(42_600)).toBe('43s')
  })

  it('drops a zero remainder', () => {
    expect(fmtTurnDuration(300_000)).toBe('5m')
    expect(fmtTurnDuration(3_600_000)).toBe('1h')
  })

  it('keeps a non-zero remainder', () => {
    expect(fmtTurnDuration(760_000)).toBe('12m 40s')
    expect(fmtTurnDuration(7_530_000)).toBe('2h 5m')
  })

  it('carries on rollover', () => {
    expect(fmtTurnDuration(59_600)).toBe('1m')
    expect(fmtTurnDuration(119_600)).toBe('2m')
    expect(fmtTurnDuration(3_599_500)).toBe('1h')
  })
})
