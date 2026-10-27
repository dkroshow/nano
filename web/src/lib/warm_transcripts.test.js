// @vitest-environment jsdom
// The warm stream's browser end: the page names what it shows and the window
// it draws into, holds what the server sends from the window's first row,
// and sends markers for what it holds when the stream reopens. Until this,
// the server chose eight threads and every reopen resent all of them.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

// The module holds the page's one stream, so each test loads a fresh copy.
let startWarmTranscripts
let warmSetChanged
let warmTranscript

class MockEventSource {
  constructor(url) {
    this.url = url
    this.closed = false
    MockEventSource.instances.push(this)
  }
  close() {
    this.closed = true
  }
  emit(message) {
    this.onmessage?.({ data: JSON.stringify(message) })
  }
}
const urlOf = (es) => new URL(es.url, 'http://nano.test')
// A reference FNV-1a (64-bit) over UTF-8, written apart from the module.
function fnv64a(text) {
  let hash = 0xcbf29ce484222325n
  for (const byte of new TextEncoder().encode(text)) hash = ((hash ^ BigInt(byte)) * 0x100000001b3n) & 0xffffffffffffffffn
  return hash.toString(16).padStart(16, '0')
}
const row = (id, projectedSeq, turnId = 't1') => ({ id, projectedSeq, turnId, type: 'agent_message', role: 'assistant', text: id })

let set
let probe

beforeEach(async () => {
  vi.resetModules()
  ;({ startWarmTranscripts, warmSetChanged, warmTranscript } = await import('./warm_transcripts.js'))
  vi.useFakeTimers()
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true })
  globalThis.innerWidth = 1280
  globalThis.innerHeight = 633
  // The probe is 100 characters wide and two lines tall in the message font.
  probe = { width: 782.67, height: 40.3 }
  vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(() => ({ width: probe.width, height: probe.height }))
  set = ['nht_a', 'nht_b']
  startWarmTranscripts(() => set)
})

afterEach(() => {
  // Hiding the page closes this test's stream, so its module instance's
  // window listeners stay quiet in the next test.
  Object.defineProperty(document, 'visibilityState', { value: 'hidden', configurable: true })
  document.dispatchEvent(new Event('visibilitychange'))
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('warm stream request', () => {
  it('names the rendered set in order and the window in lines and cols', () => {
    const url = urlOf(MockEventSource.instances[0])
    expect(url.searchParams.get('set')).toBe('nht_a,nht_b')
    expect(url.searchParams.get('lines')).toBe(String(Math.floor(633 / 20.15)))
    expect(url.searchParams.get('cols')).toBe(String(Math.floor((1280 - 56) / 7.8267)))
    expect(url.searchParams.get('markers')).toBeNull()
  })

  it('sends a marker for each held thread when it reopens for a new member', async () => {
    const es = MockEventSource.instances[0]
    es.emit({ type: 'tail', ref: 'nht_a', rows: [row('r1', 10), row('r2', 12)], turns: [{ id: 't1' }], projection: { key: 'ntp_a' }, page: { startSeq: 10 } })
    set = ['nht_b', 'nht_a', 'nht_c']
    warmSetChanged()
    await vi.advanceTimersByTimeAsync(999)
    expect(MockEventSource.instances).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(es.closed).toBe(true)
    const url = urlOf(MockEventSource.instances[1])
    expect(url.searchParams.get('set')).toBe('nht_b,nht_a,nht_c')
    // The digest is 64-bit FNV-1a over the Enhanced fields of each row, one
    // per line, absent ones empty, which the server computes the same way
    // (warmHeldDigest).
    const digestOf = (r) => `${r.id}\n${r.projectedSeq}\n${r.turnId}\n\n${r.type}\n\n${r.role}\n\n${r.text}\n\n\n0\n0\n\n0\n\n`
    expect(JSON.parse(url.searchParams.get('markers'))).toEqual({ nht_a: { key: 'ntp_a', first: 10, last: 12, digest: fnv64a(digestOf(row('r1', 10)) + digestOf(row('r2', 12))) } })
    expect(warmTranscript('nht_a').items.map((r) => r.id)).toEqual(['r1', 'r2'])
  })

  it('reopens itself after a dropped connection, with the markers of what it holds now', async () => {
    const es = MockEventSource.instances[0]
    es.emit({ type: 'tail', ref: 'nht_a', rows: [row('r1', 10)], turns: [], projection: { key: 'ntp_a' } })
    es.onerror()
    expect(es.closed).toBe(true)
    await vi.advanceTimersByTimeAsync(1999)
    expect(MockEventSource.instances).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1)
    const url = urlOf(MockEventSource.instances[1])
    expect(JSON.parse(url.searchParams.get('markers')).nht_a).toMatchObject({ key: 'ntp_a', first: 10, last: 10 })
  })

  it('a reorder or a smaller window does not reopen; a larger window does', async () => {
    set = ['nht_b', 'nht_a']
    warmSetChanged()
    globalThis.innerHeight = 400
    globalThis.dispatchEvent(new Event('resize'))
    await vi.advanceTimersByTimeAsync(1000)
    expect(MockEventSource.instances).toHaveLength(1)

    globalThis.innerHeight = 900
    globalThis.dispatchEvent(new Event('nano-text-size'))
    await vi.advanceTimersByTimeAsync(1000)
    expect(MockEventSource.instances).toHaveLength(2)
    expect(urlOf(MockEventSource.instances[1]).searchParams.get('lines')).toBe(String(Math.floor(900 / 20.15)))
  })
})

describe('warm entries', () => {
  it('holds the window from its first row, merges updates and keeps the entry after a pane reads it', () => {
    const es = MockEventSource.instances[0]
    es.emit({ type: 'tail', ref: 'nht_a', rows: [row('r1', 10), row('r0', 8), row('r2', 12, 't2')], turns: [{ id: 't1' }, { id: 't2' }, { id: 't9' }], projection: { key: 'ntp_a' }, page: { startSeq: 10 } })
    expect(warmTranscript('nht_a').items.map((r) => r.id)).toEqual(['r1', 'r2'])
    expect(warmTranscript('nht_a').turns.map((t) => t.id)).toEqual(['t1', 't2'])

    es.emit({ type: 'update', ref: 'nht_a', rows: [row('r3', 14, 't2'), { ...row('r2', 12, 't2'), text: 'edited' }], removed: ['r1'], turns: [{ id: 't2' }], page: { startSeq: 12 } })
    const entry = warmTranscript('nht_a')
    expect(entry.items.map((r) => [r.id, r.text])).toEqual([['r2', 'edited'], ['r3', 'r3']])
    expect(entry.projection.key).toBe('ntp_a')
    expect(warmTranscript('nht_a')).toBe(entry) // a read does not take it away

    es.emit({ type: 'set', refs: ['nht_b'] })
    expect(warmTranscript('nht_a')).toBeNull()
    es.emit({ type: 'tail', ref: 'nht_b', rows: [row('b1', 1)], turns: [], projection: { key: 'ntp_b' } })
    es.emit({ type: 'error', ref: 'nht_b', error: 'no live session holds this thread' })
    expect(warmTranscript('nht_b')).toBeNull()
  })
})
