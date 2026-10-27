import { describe, expect, it } from 'vitest'
import { readAPIError } from './api_error.js'

describe('API error responses', () => {
  it('reads the common envelope without exposing its JSON', async () => {
    const result = await readAPIError({
      status: 409,
      text: async () => JSON.stringify({
        error: { code: 'thread_attached', message: 'Thread is already attached', details: { existingSessionId: 'session-2' } },
      }),
    })
    expect(result).toEqual({
      code: 'thread_attached',
      message: 'Thread is already attached',
      details: { existingSessionId: 'session-2' },
      envelope: true,
    })
  })

  it('keeps proxy text readable and uses a fallback for an empty body', async () => {
    await expect(readAPIError({ status: 502, text: async () => 'proxy unavailable' }))
      .resolves.toMatchObject({ code: '', message: 'proxy unavailable' })
    await expect(readAPIError({ status: 503, text: async () => '' }, 'Nano unavailable'))
      .resolves.toMatchObject({ code: '', message: 'Nano unavailable' })
  })
})
