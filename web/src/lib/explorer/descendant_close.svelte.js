// Shared action state for every UI path that lets go of a hosted parent.
// The parent always has a fixed action; only explicitly checked descendants
// are included. The dialog keeps partial failures visible for acknowledgement.
import { app, closeSession } from '../../app.svelte.js'
import { explorer } from '../../explorer.svelte.js'
import { closeDescendantRows, closeSelection } from './descendant_close.js'
import { threadIdentityKeys } from '../../explorer_local.js'

export const descendantClose = $state({ request: null })

function sessionIdentityKeys(session) {
  const keys = []
  if (session?.hostedThreadId) keys.push(`hosted:${session.hostedThreadId}`)
  if (session?.provider && session?.threadId) keys.push(`${session.provider}:${session.threadId}`)
  return keys
}

export function resolveCloseParent(parent, rows = explorer.dataset, sessions = app.sessions) {
  const keys = new Set(threadIdentityKeys(parent))
  const row = (rows ?? []).find((candidate) => threadIdentityKeys(candidate).some((key) => keys.has(key))) ?? null
  for (const key of threadIdentityKeys(row)) keys.add(key)
  const session = (sessions ?? []).find((candidate) => sessionIdentityKeys(candidate).some((key) => keys.has(key))) ?? null
  return {
    ...(parent ?? {}),
    ...(row ?? {}),
    provider: row?.provider ?? session?.provider ?? parent?.provider,
    nativeId: row?.nativeId ?? session?.threadId ?? parent?.nativeId,
    hostedThreadId: row?.hostedThreadId ?? session?.hostedThreadId ?? parent?.hostedThreadId,
  }
}

function resolveRequest(request, result) {
  if (!request?.resolve) return
  const resolve = request.resolve
  request.resolve = null
  resolve(result)
}

export async function requestParentClose({ parent, sessionId, provider }) {
  if (!sessionId) return { parentClosed: false, cancelled: true }
  const resolvedParent = resolveCloseParent(parent)
  const nodes = closeDescendantRows(resolvedParent, explorer.dataset, app.sessions)
  if (nodes.length === 0) {
    await closeSession(sessionId)
    return { parentClosed: true, cancelled: false, failures: [] }
  }
  return new Promise((resolve) => {
    descendantClose.request = {
      parent: resolvedParent,
      parentSessionId: sessionId,
      provider,
      nodes,
      selected: [],
      busy: false,
      results: [],
      resolve,
    }
  })
}

export function toggleCloseDescendant(sessionId, checked) {
  const request = descendantClose.request
  if (!request || !sessionId || request.busy) return
  const selected = new Set(request.selected)
  if (checked) selected.add(sessionId)
  else selected.delete(sessionId)
  request.selected = [...selected]
}

export function toggleAllCloseDescendants(checked) {
  const request = descendantClose.request
  if (!request || request.busy) return
  request.selected = checked ? closeSelection(request.nodes, []).eligible.map((node) => node.sessionId) : []
}

export function cancelDescendantClose() {
  const request = descendantClose.request
  if (!request || request.busy) return
  resolveRequest(request, { parentClosed: false, cancelled: true, failures: [] })
  descendantClose.request = null
}

export async function submitDescendantClose() {
  const request = descendantClose.request
  if (!request || request.busy) return
  request.busy = true
  request.results = []
  const selection = closeSelection(request.nodes, request.selected)
  const targets = [
    ...selection.selectedNodes.map((node) => ({ sessionId: node.sessionId, label: node.label, parent: false })),
    { sessionId: request.parentSessionId, label: 'parent', parent: true },
  ]
  for (const target of targets) {
    try {
      await closeSession(target.sessionId)
      request.results = [...request.results, { ...target, ok: true }]
    } catch (error) {
      request.results = [...request.results, { ...target, ok: false, error: String(error?.message ?? error) }]
    }
  }
  request.busy = false
  const parent = request.results.find((result) => result.parent)
  const failures = request.results.filter((result) => !result.ok)
  resolveRequest(request, { parentClosed: Boolean(parent?.ok), cancelled: false, failures })
  if (failures.length === 0) descendantClose.request = null
}

export function acknowledgeDescendantCloseFailures() {
  descendantClose.request = null
}

export function __resetDescendantCloseForTest() {
  descendantClose.request = null
}
