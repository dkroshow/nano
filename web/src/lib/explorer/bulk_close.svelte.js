import { refreshSessions } from '../../app.svelte.js'
import { explorer, loadDataset } from '../../explorer.svelte.js'
import { readAPIError } from '../../api_error.js'
import { activityDot, rowLabel, sidebarAge } from './explorer_format.js'

export const bulkClose = $state({ request: null })

// The server preview is authoritative because a collapsed sidebar may know a
// descendant count without having every historical row loaded. Local catalog
// rows only supply human labels and last-active context.
export function bulkClosePreviewRows(root, members, rows, now = Date.now()) {
  return (members ?? []).map((member, index) => {
    const row = (rows ?? []).find((candidate) => (
      candidate.hostedThreadId === member.hostedThreadId ||
      (candidate.provider === member.provider && candidate.nativeId === member.nativeThreadId)
    )) ?? (index === 0 ? root : null)
    const fallback = member.nativeThreadId ? `${member.provider} · ${member.nativeThreadId}` : `${member.provider} thread`
    const label = row
      ? rowLabel({ name: row.name?.value, title: row.title, preview: row.preview, kind: row.kind }).text
      : fallback
    const age = sidebarAge(row?.updatedAt ?? row?.createdAt, now)
    return {
      key: `hosted:${member.hostedThreadId}`,
      row,
      depth: member.depth ?? 0,
      label: label || fallback,
      dot: row ? activityDot(row) : null,
      activity: member.working ? 'working' : member.status === 'exited' ? 'closed' : age ? `last active ${age}` : member.status || 'inactive',
      working: Boolean(member.working),
      starred: Boolean(member.starred),
    }
  })
}

function resolveRequest(request, result) {
  if (!request?.resolve) return
  const resolve = request.resolve
  request.resolve = null
  resolve(result)
}

export async function requestBulkClose(root) {
  if (!root?.hostedThreadId) return Promise.resolve({ complete: false, cancelled: true })
  const response = await fetch(`/api/v1/hosted-threads/${encodeURIComponent(root.hostedThreadId)}/family/close`, { cache: 'no-store' })
  if (!response.ok) {
    throw new Error((await readAPIError(response, `Bulk close preview failed (${response.status}).`)).message)
  }
  const preview = await response.json()
  if (!Array.isArray(preview?.members) || preview.members.length !== preview.memberCount) {
    throw new Error('Bulk close preview returned an incomplete tree.')
  }
  const nodes = bulkClosePreviewRows(root, preview.members, explorer.dataset)
  return new Promise((resolve) => {
    bulkClose.request = {
      root,
      rootHostedThreadId: preview.rootHostedThreadId,
      nodes,
      busy: false,
      results: [],
      response: null,
      resolve,
    }
  })
}

export function cancelBulkClose() {
  const request = bulkClose.request
  if (!request || request.busy) return
  resolveRequest(request, { complete: false, cancelled: true })
  bulkClose.request = null
}

export async function submitBulkClose(starredPolicy) {
  const request = bulkClose.request
  if (!request || request.busy) return
  request.busy = true
  request.results = []
  try {
    const response = await fetch(`/api/v1/hosted-threads/${encodeURIComponent(request.rootHostedThreadId)}/family/close`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ starredPolicy }),
    })
    if (!response.ok && response.status !== 207) {
      throw new Error((await readAPIError(response, `Bulk close failed (${response.status}).`)).message)
    }
    const raw = await response.text()
    let result = null
    try { result = raw ? JSON.parse(raw) : null } catch { /* plain server error below */ }
    if (!result) throw new Error('Bulk close returned no result.')
    request.response = result
    request.results = result.members ?? []
    await refreshSessions()
    await loadDataset()
    resolveRequest(request, { ...result, cancelled: false })
    if (result.complete) bulkClose.request = null
  } catch (error) {
    request.results = [{ closeAction: 'failed', error: String(error?.message ?? error) }]
    resolveRequest(request, { complete: false, cancelled: false, error: request.results[0].error })
  } finally {
    if (bulkClose.request === request) request.busy = false
  }
}

export function acknowledgeBulkCloseFailure() {
  bulkClose.request = null
}

export function __resetBulkCloseForTest() {
  bulkClose.request = null
}
