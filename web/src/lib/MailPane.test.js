// @vitest-environment jsdom
//
// The inbox pane inside the thread pop-up: it reads with peek=true only (the
// UI must never mark mail seen for the agent), renders newest first, sends
// operator mail, and offers Edit only on unread operator-authored messages.
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { render, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import MailPane from './MailPane.svelte'
import { applyMailDelta, resetMailCounts } from './mail.svelte.js'

const inboxItems = [
  {
    messageId: 'nmsg_1', clientMessageId: 'a-1', origin: 'agent', seq: 1,
    sender: { kind: 'thread', name: 'sample-task worker' },
    recipient: { hostedThreadId: 'nht_r' },
    subject: 'Oldest', body: 'First message', refs: [], status: 'acted',
    createdAt: '2026-08-27T10:00:00Z', updatedAt: '2026-08-27T10:05:00Z',
  },
  {
    messageId: 'nmsg_2', clientMessageId: 'op-1', origin: 'operator', seq: 2,
    sender: { kind: 'operator', name: 'Sample Operator' },
    recipient: { hostedThreadId: 'nht_r' },
    subject: 'Newest', body: 'Operator message', refs: [], status: 'unread',
    createdAt: '2026-08-27T11:00:00Z', updatedAt: '2026-08-27T11:00:00Z',
  },
]

let fetchMock
let inboxRequests

beforeEach(() => {
  resetMailCounts()
  inboxRequests = []
  fetchMock = vi.fn(async (url, init) => {
    const href = String(url)
    if (href.includes('/inbox')) {
      inboxRequests.push(href)
      return { ok: true, status: 200, json: async () => ({ items: inboxItems, nextSince: 2 }) }
    }
    if (href.endsWith('/messages') && init?.method === 'POST') {
      const body = JSON.parse(init.body)
      return {
        ok: true, status: 201,
        json: async () => ({
          messageId: 'nmsg_3', clientMessageId: body.clientMessageId, origin: 'operator', seq: 3,
          sender: { kind: 'operator', name: 'Sample Operator' }, recipient: { hostedThreadId: 'nht_r' },
          subject: body.subject ?? '', body: body.body, refs: [], status: 'unread',
          createdAt: '2026-08-27T12:00:00Z', updatedAt: '2026-08-27T12:00:00Z',
        }),
      }
    }
    if (href.includes('/api/v1/messages/')) {
      return { ok: true, status: 200, json: async () => ({ message: inboxItems[1], events: [] }) }
    }
    throw new Error(`unexpected fetch ${href}`)
  })
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('MailPane', () => {
  it('peeks, renders newest first, and gates Edit to unread operator mail', async () => {
    const { getAllByRole, getByText, queryByText } = render(MailPane, { hostedThreadId: 'nht_r' })
    await waitFor(() => getByText('Operator message'))
    for (const request of inboxRequests) {
      expect(request).toContain('peek=true')
    }

    const rows = getAllByRole('listitem')
    expect(rows[0].textContent).toContain('Newest')
    expect(rows[1].textContent).toContain('Oldest')

    // Expand the unread operator message: Edit appears.
    await fireEvent.click(getByText('Operator message'))
    await waitFor(() => getByText('Edit'))

    // Expand the acted agent message: no Edit affordance.
    await fireEvent.click(getByText('First message'))
    await waitFor(() => expect(queryByText('Edit')).toBeNull())
  })

  it('sends operator mail with operator:true and no senderThreadRef', async () => {
    const { getByLabelText, getByText, queryByLabelText } = render(MailPane, { hostedThreadId: 'nht_r' })
    await waitFor(() => getByText('Operator message'))
    expect(queryByLabelText('Message subject')).toBeNull()
    await fireEvent.input(getByLabelText('Message to this agent'), { target: { value: 'New instruction' } })
    await fireEvent.click(getByText('Send as Operator'))
    await waitFor(() => getByText('New instruction'))

    const send = fetchMock.mock.calls.find(([url, init]) => String(url).endsWith('/messages') && init?.method === 'POST')
    expect(send).toBeTruthy()
    const payload = JSON.parse(send[1].body)
    expect(payload.operator).toBe(true)
    expect(payload.senderThreadRef).toBeUndefined()
    expect(payload.subject).toBeUndefined()
    expect(payload.body).toBe('New instruction')
    expect(payload.clientMessageId.startsWith('operator:')).toBe(true)
  })

  it('refetches when a mail delta arrives', async () => {
    const { getByText } = render(MailPane, { hostedThreadId: 'nht_r' })
    await waitFor(() => getByText('Operator message'))
    const before = inboxRequests.length
    applyMailDelta({ counts: { nht_r: { total: 3, unread: 1, seen: 0 } }, summary: { total: 3, unread: 1, seen: 0 } })
    await waitFor(() => expect(inboxRequests.length).toBeGreaterThan(before))
  })
})
