// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render } from '@testing-library/svelte'
import ThreadDetail from './ThreadDetail.svelte'
import { explorer, closeDetail } from '../explorer.svelte.js'
import { __resetTranscriptForTest } from '../transcript_store.svelte.js'

afterEach(() => {
  cleanup()
  closeDetail()
  explorer.dataset = []
  __resetTranscriptForTest()
  vi.unstubAllGlobals()
})

describe('thread diagnostics', () => {
  it('renders delivery and session facts as a visible alert', () => {
    explorer.selectedId = 'nth_owner'
    explorer.detailState = 'idle'
    explorer.detail = {
      id: 'nth_owner', provider: 'claude', nativeId: 'native-owner', kind: 'main',
      runtime: {
        attached: true, sessionId: 'nano-owner',
        threadDiagnostics: [
          { code: 'async_session_event_failed', message: 'An asynchronous session event could not be applied.', source: 'session', occurrences: 654 },
        ],
      },
      metadata: {}, sources: [],
    }

    const view = render(ThreadDetail)
    const alert = view.getByRole('alert')
    expect(alert.textContent).toContain('Thread diagnostics')
    expect(alert.textContent).toContain('async_session_event_failed: An asynchronous session event could not be applied.')
    expect(alert.textContent).toContain('654 occurrences')
  })
})
