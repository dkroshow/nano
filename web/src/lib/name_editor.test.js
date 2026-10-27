// Inline name-editor state machine (Slice E): commit (PUT), clear (DELETE on an
// empty commit), cancel, and error-stays-open, all with mocked fetch.
import { describe, it, expect, vi, afterEach } from 'vitest'
import { createNameEditor } from './name_editor.svelte.js'
import { explorer } from '../explorer.svelte.js'

const okJson = (data, status = 200) => ({ ok: true, status, json: async () => data })
const errJson = (message, status = 409) => ({ ok: false, status, json: async () => ({ error: { message } }) })

afterEach(() => {
  vi.unstubAllGlobals()
  explorer.dataset = []
  explorer.detail = null
})

describe('inline name editor', () => {
  it('commits a rename via PUT and closes on success', async () => {
    const record = { value: 'graph classifier', provenance: 'manual', evidence: 'user_assigned' }
    const fetchMock = vi.fn(async () => okJson({ name: record }))
    vi.stubGlobal('fetch', fetchMock)

    const ed = createNameEditor()
    ed.begin('nth_1', null)
    expect(ed.state.editing).toBe(true)
    ed.state.value = 'graph classifier'

    const pending = ed.commit()
    expect(ed.state.saving).toBe(true) // disabled while in flight
    await pending

    const [url, opts] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/v1/threads/nth_1/name')
    expect(opts.method).toBe('PUT')
    expect(JSON.parse(opts.body)).toEqual({ name: 'graph classifier' })
    expect(ed.state.editing).toBe(false)
    expect(ed.state.saving).toBe(false)
    expect(ed.state.error).toBeNull()
  })

  it('an empty/whitespace commit clears the name via DELETE', async () => {
    const fetchMock = vi.fn(async () => ({ ok: true, status: 204, json: async () => ({}) }))
    vi.stubGlobal('fetch', fetchMock)

    const ed = createNameEditor()
    ed.begin('nth_2', 'old name')
    ed.state.value = '   '
    await ed.commit()

    const [url, opts] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/v1/threads/nth_2/name')
    expect(opts.method).toBe('DELETE')
    expect(ed.state.editing).toBe(false)
  })

  it('cancel closes the editor without any request', () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)

    const ed = createNameEditor()
    ed.begin('nth_3', 'keep me')
    ed.state.value = 'discarded'
    ed.cancel()

    expect(ed.state.editing).toBe(false)
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('an API error keeps the input open with the message inline', async () => {
    const fetchMock = vi.fn(async () => errJson('Thread names file is malformed', 409))
    vi.stubGlobal('fetch', fetchMock)

    const ed = createNameEditor()
    ed.begin('nth_4', null)
    ed.state.value = 'new name'
    await ed.commit()

    expect(ed.state.editing).toBe(true) // stays open for correction
    expect(ed.state.saving).toBe(false)
    expect(ed.state.error).toBe('Thread names file is malformed')
  })

  it('patches the edited row from the response record, never the input text', async () => {
    explorer.dataset = [{ id: 'nth_5', name: null }]
    const record = { value: 'canonical', provenance: 'manual', evidence: 'user_assigned' }
    vi.stubGlobal('fetch', vi.fn(async () => okJson({ name: record })))

    const ed = createNameEditor()
    ed.begin('nth_5', null)
    ed.state.value = 'typed-but-not-authoritative'
    await ed.commit()

    expect(explorer.dataset[0].name).toEqual(record)
  })
})
