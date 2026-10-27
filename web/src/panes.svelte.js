
export const panes = $state({
  items: [{ id: 1, tabIds: [], activeTabId: null, detail: null }],
  layout: [[1]],
  tabs: {},
  activePaneId: 1,
  nextPaneId: 2,
  nextTabId: 1,
  dragTabId: null,
})

const ACTIVE_THREAD_VIEW_KEY = 'nano:active-thread-view'
const ACTIVE_THREAD_VIEW_VERSION = 2
const THREAD_TAB_KEYS = ['catalogId', 'sessionId', 'provider', 'nativeId', 'title', 'cwd', 'model', 'effort', 'status']

export function isThreadTab(tab) {
  return Boolean(tab) && (tab.kind == null || tab.kind === 'thread')
}

function metadataValue(item, key) {
  const field = item?.metadata?.[key]
  return field?.state === 'known' ? field.value ?? '' : ''
}

function fallbackTitle(target) {
  const cwd = target.cwd || ''
  return (
    target.title ||
    cwd.split('/').filter(Boolean).at(-1) ||
    target.nativeId?.slice(0, 8) ||
    target.sessionId ||
    'Thread'
  )
}

export function tabTarget({ item = null, session = null } = {}) {
  const runtime = item?.runtime
  return {
    catalogId: item?.transient ? '' : item?.id ?? '',
    sessionId: session?.id ?? (runtime?.attached ? runtime.sessionId ?? '' : ''),
    provider: item?.provider ?? session?.provider ?? '',
    nativeId: item?.nativeId ?? session?.threadId ?? '',
    title: item?.name?.value || session?.name || item?.title || item?.preview || '',
    cwd: metadataValue(item, 'cwd') || session?.cwd || '',
    model: metadataValue(item, 'model') || session?.model || '',
    effort: metadataValue(item, 'effort') || '',
    status: runtime?.status || session?.status || '',
    turnActive: runtime?.turnActive ?? session?.turnActive ?? false,
    detached: Boolean(runtime?.detached),
  }
}

function sameThread(tab, target) {
  if (!isThreadTab(tab)) return false
  if (tab.provider && target.provider && tab.provider !== target.provider) return false
  if (tab.nativeId && target.nativeId) return tab.nativeId === target.nativeId
  if (tab.sessionId && target.sessionId) return tab.sessionId === target.sessionId
  return Boolean(tab.catalogId && target.catalogId && tab.catalogId === target.catalogId)
}

function mergeTarget(tab, target) {
  for (const key of ['catalogId', 'sessionId', 'provider', 'nativeId', 'title', 'cwd', 'model', 'effort', 'status']) {
    if (target[key]) tab[key] = target[key]
  }
  tab.turnActive = Boolean(target.turnActive)
  tab.detached = Boolean(target.detached)
  tab.title = fallbackTitle(tab)
}

function requestTailReset(tab, force = false, timing = null) {
  if (!tab) return
  const nonce = Math.max(Date.now(), Number(tab.tailResetNonce || 0) + 1)
  tab.openStartedAt = timing?.startedAt ?? globalThis.performance?.now?.() ?? 0
  tab.openTrigger = timing?.trigger ?? (force ? 'sidebar' : 'tab')
  tab.tailResetNonce = nonce
  tab.forceTailResetNonce = force ? nonce : 0
}

export function openThreadTab(target, { forceTail = false, timing = null } = {}) {
  for (const pane of panes.items) {
    for (const tabId of pane.tabIds) {
      const tab = panes.tabs[tabId]
      if (!tab || !sameThread(tab, target)) continue
      mergeTarget(tab, target)
      requestTailReset(tab, forceTail, timing)
      // A resumed session rebinds the same historical tab. Keep its open
      // detail view mounted so the restore status remains visible in place.
      if (pane.detail?.tabId !== tabId) pane.detail = null
      pane.activeTabId = tabId
      panes.activePaneId = pane.id
      return tabId
    }
  }

  const pane = panes.items.find((candidate) => candidate.id === panes.activePaneId) ?? panes.items[0]
  if (!pane) return null
  pane.detail = null
  const tabId = panes.nextTabId++
  const tailResetNonce = Date.now()
  panes.tabs[tabId] = {
    id: tabId,
    kind: 'thread',
    ...target,
    title: fallbackTitle(target),
    tailResetNonce,
    openStartedAt: timing?.startedAt ?? globalThis.performance?.now?.() ?? 0,
    openTrigger: timing?.trigger ?? (forceTail ? 'sidebar' : 'tab'),
    forceTailResetNonce: forceTail ? tailResetNonce : 0,
  }
  pane.tabIds.push(tabId)
  pane.activeTabId = tabId
  panes.activePaneId = pane.id
  return tabId
}

function fileRequestKey(path, context = {}) {
  return [path, context.cwd || '', context.catalogThreadId || '', context.sessionId || ''].join('\u0000')
}

function fileTitle(path) {
  return String(path || '').split('/').filter(Boolean).at(-1) || 'File'
}

function paneHoldingTab(tabId) {
  return panes.items.find((pane) => pane.tabIds.includes(tabId)) ?? null
}

// General transcript/file-reader entry point. The authored path stays on the
// pending tab until the backend resolves it to a canonical absolute path;
// reconcileFileTab then coalesces aliases onto one file tab.
export function openFileTab(reference, context = {}, sourcePaneId = panes.activePaneId) {
  const requestedPath = String(reference?.path ?? reference ?? '').trim()
  if (!requestedPath) return null
  const line = Number(reference?.line || 0)
  const requestKey = fileRequestKey(requestedPath, context)
  for (const [rawId, tab] of Object.entries(panes.tabs)) {
    if (tab?.kind !== 'file') continue
    if (tab.requestKey !== requestKey && !(tab.path && reference?.canonicalPath === tab.path)) continue
    const tabId = Number(rawId)
    tab.line = line
    tab.navigationNonce = Math.max(Date.now(), Number(tab.navigationNonce || 0) + 1)
    const pane = paneHoldingTab(tabId)
    if (pane) {
      pane.activeTabId = tabId
      pane.detail = null
      panes.activePaneId = pane.id
    }
    return tabId
  }

  const source = panes.items.find((pane) => pane.id === sourcePaneId) ?? panes.items[0]
  const target = panes.items.length > 1 ? nearestPane(source?.id) : source
  if (!target) return null
  const tabId = panes.nextTabId++
  panes.tabs[tabId] = {
    id: tabId,
    kind: 'file',
    requestedPath,
    requestKey,
    path: '',
    title: fileTitle(requestedPath),
    basename: fileTitle(requestedPath),
    cwd: context.cwd || '',
    catalogThreadId: context.catalogThreadId || '',
    sessionId: context.sessionId || '',
    line,
    navigationNonce: Date.now(),
    fileDirty: false,
    fileEditable: false,
  }
  target.detail = null
  target.tabIds.push(tabId)
  target.activeTabId = tabId
  panes.activePaneId = target.id
  return tabId
}

export function reconcileFileTab(tabId, result) {
  const tab = panes.tabs[tabId]
  if (!tab || tab.kind !== 'file' || !result?.path) return tabId
  const canonical = result.path
  for (const [rawId, candidate] of Object.entries(panes.tabs)) {
    const candidateId = Number(rawId)
    if (candidateId === tabId || candidate?.kind !== 'file' || candidate.path !== canonical) continue
    candidate.line = tab.line
    candidate.navigationNonce = Math.max(Date.now(), Number(candidate.navigationNonce || 0) + 1)
    closeTab(paneHoldingTab(tabId)?.id, tabId)
    const pane = paneHoldingTab(candidateId)
    if (pane) {
      pane.activeTabId = candidateId
      pane.detail = null
      panes.activePaneId = pane.id
    }
    return candidateId
  }
  tab.path = canonical
  tab.basename = result.basename || fileTitle(canonical)
  tab.title = tab.basename
  tab.fileEditable = Boolean(result.editable)
  return tabId
}

export function setFileTabDirty(tabId, dirty) {
  const tab = panes.tabs[tabId]
  if (tab?.kind === 'file') tab.fileDirty = Boolean(dirty)
}

export function focusTab(paneId, tabId) {
  const pane = panes.items.find((candidate) => candidate.id === paneId)
  if (!pane?.tabIds.includes(tabId)) return
  requestTailReset(panes.tabs[tabId])
  pane.activeTabId = tabId
  pane.detail = null
  panes.activePaneId = paneId
}

export function closeTab(paneId, tabId) {
  const pane = panes.items.find((candidate) => candidate.id === paneId)
  if (!pane) return
  const index = pane.tabIds.indexOf(tabId)
  if (index < 0) return
  pane.tabIds.splice(index, 1)
  delete panes.tabs[tabId]
  clearDetailsForTab(tabId)
  if (pane.activeTabId === tabId) {
    pane.activeTabId = pane.tabIds[Math.max(0, index - 1)] ?? null
    requestTailReset(panes.tabs[pane.activeTabId])
  }
}

export function addPane() {
  if (panes.items.length >= 12) return null
  const id = panes.nextPaneId++
  panes.items.push({ id, tabIds: [], activeTabId: null, detail: null })
  panes.layout = normalizeLayout([...panes.layout, [id]])
  panes.activePaneId = id
  return id
}

export function addPaneToStack(existingPaneId) {
  if (panes.items.length >= 12) return null
  const id = panes.nextPaneId++
  panes.items.push({ id, tabIds: [], activeTabId: null, detail: null })
  panes.activePaneId = id
  return id
}

export function removePane(paneId) {
  if (panes.items.length <= 1) return
  const index = panes.items.findIndex((candidate) => candidate.id === paneId)
  if (index < 0) return
  for (const tabId of panes.items[index].tabIds) {
    delete panes.tabs[tabId]
    clearDetailsForTab(tabId)
  }
  panes.items.splice(index, 1)
  panes.layout = normalizeLayout(panes.layout)
  if (panes.activePaneId === paneId) {
    panes.activePaneId = panes.items[Math.min(index, panes.items.length - 1)]?.id ?? null
  }
  clearPaneDimensions()
}

export function normalizeLayout(layout = panes.layout) {
  const valid = new Set(panes.items.map((pane) => pane.id))
  const seen = new Set()
  const next = []
  for (const rawColumn of layout ?? []) {
    const column = []
    for (const rawId of Array.isArray(rawColumn) ? rawColumn : [rawColumn]) {
      const id = Number(rawId)
      if (!valid.has(id) || seen.has(id)) continue
      column.push(id)
      seen.add(id)
    }
    if (column.length) next.push(column)
  }
  for (const pane of panes.items) {
    if (!seen.has(pane.id)) next.push([pane.id])
  }
  return next
}

export function setLayout(layout) {
  panes.layout = normalizeLayout(layout)
}

export function moveTabToPane(tabId, targetPaneId) {
  const target = panes.items.find((pane) => pane.id === targetPaneId)
  if (!target || !panes.tabs[tabId]) return null
  let source = null
  for (const pane of panes.items) {
    const index = pane.tabIds.indexOf(tabId)
    if (index < 0) continue
    source = pane
    pane.tabIds.splice(index, 1)
    if (pane.activeTabId === tabId) pane.activeTabId = pane.tabIds[Math.max(0, index - 1)] ?? null
    break
  }
  if (!target.tabIds.includes(tabId)) target.tabIds.push(tabId)
  target.activeTabId = tabId
  panes.activePaneId = targetPaneId
  return source?.id ?? null
}

export function reorderTab(paneId, sourceTabId, targetTabId, before) {
  const pane = panes.items.find((candidate) => candidate.id === paneId)
  if (!pane) return
  const sourceIndex = pane.tabIds.indexOf(sourceTabId)
  if (sourceIndex < 0 || !pane.tabIds.includes(targetTabId) || sourceTabId === targetTabId) return
  pane.tabIds.splice(sourceIndex, 1)
  let targetIndex = pane.tabIds.indexOf(targetTabId)
  if (!before) targetIndex += 1
  pane.tabIds.splice(targetIndex, 0, sourceTabId)
}

export function splitWithTab(existingPaneId, tabId, position) {
  const newPaneId = addPaneToStack(existingPaneId)
  if (newPaneId == null) return null
  const next = normalizeLayout().map((column) => [...column])
  const columnIndex = next.findIndex((column) => column.includes(existingPaneId))
  if (columnIndex < 0) next.push([newPaneId])
  else {
    const rowIndex = next[columnIndex].indexOf(existingPaneId)
    next[columnIndex].splice(position === 'before' ? rowIndex : rowIndex + 1, 0, newPaneId)
  }
  setLayout(next)
  const sourcePaneId = moveTabToPane(tabId, newPaneId)
  const source = panes.items.find((pane) => pane.id === sourcePaneId)
  if (sourcePaneId !== existingPaneId && source && source.tabIds.length === 0 && panes.items.length > 1) {
    removePane(source.id)
  }
  return newPaneId
}

export function clearTabs() {
  panes.tabs = {}
  panes.dragTabId = null
  for (const pane of panes.items) {
    pane.tabIds = []
    pane.activeTabId = null
    pane.detail = null
  }
  clearPaneDimensions()
}

export function activeTab() {
  const pane = panes.items.find((candidate) => candidate.id === panes.activePaneId)
  const tabId = pane?.detail?.tabId ?? pane?.activeTabId
  return tabId ? panes.tabs[tabId] ?? null : null
}

export function saveActiveThreadView() {
  if (typeof localStorage === 'undefined') return false
  try {
    const savedPanes = panes.items.map((pane) => ({
      id: pane.id,
      tabs: pane.tabIds.map((tabId) => serializeTab(tabId)).filter(Boolean),
      activeTabId: pane.activeTabId,
      detailTabId: pane.detail?.tabId ?? null,
    }))
    localStorage.setItem(
      ACTIVE_THREAD_VIEW_KEY,
      JSON.stringify({
        version: ACTIVE_THREAD_VIEW_VERSION,
        panes: savedPanes,
        layout: normalizeLayout(),
        activePaneId: panes.activePaneId,
      }),
    )
    return true
  } catch {
    return false
  }
}

export function restoreActiveThreadView() {
  if (typeof localStorage === 'undefined') return false
  try {
    const raw = localStorage.getItem(ACTIVE_THREAD_VIEW_KEY)
    if (!raw) return false
    const saved = JSON.parse(raw)
    // Nano 0.x persisted only one focused thread. Keep it readable so an
    // upgrade does not discard the operator's last open conversation.
    if (saved?.version === 1) return restoreLegacyActiveThread(saved)
    if (saved?.version !== ACTIVE_THREAD_VIEW_VERSION || !Array.isArray(saved.panes) || !saved.panes.length) {
      localStorage.removeItem(ACTIVE_THREAD_VIEW_KEY)
      return false
    }
    return restoreWorkspace(saved)
  } catch {
    localStorage.removeItem(ACTIVE_THREAD_VIEW_KEY)
    return false
  }
}

function serializeTab(tabId) {
  const tab = panes.tabs[tabId]
  if (!tab) return null
  if (tab.kind === 'file') {
    const requestedPath = String(tab.requestedPath || tab.path || '').trim()
    if (!requestedPath) return null
    return {
      id: tabId,
      kind: 'file',
      requestedPath,
      path: typeof tab.path === 'string' ? tab.path : '',
      title: typeof tab.title === 'string' ? tab.title : '',
      basename: typeof tab.basename === 'string' ? tab.basename : '',
      cwd: typeof tab.cwd === 'string' ? tab.cwd : '',
      catalogThreadId: typeof tab.catalogThreadId === 'string' ? tab.catalogThreadId : '',
      sessionId: typeof tab.sessionId === 'string' ? tab.sessionId : '',
      line: Number(tab.line || 0),
    }
  }
  if (!isThreadTab(tab)) return null
  const saved = { id: tabId, kind: 'thread' }
  for (const key of THREAD_TAB_KEYS) saved[key] = typeof tab[key] === 'string' ? tab[key] : ''
  saved.detached = Boolean(tab.detached)
  return saved.nativeId || saved.catalogId || saved.sessionId ? saved : null
}

function restoreWorkspace(saved) {
  const restoredPanes = []
  const restoredTabs = {}
  const paneIds = new Map()
  const tabIds = new Map()
  let nextPaneId = 1
  let nextTabId = 1

  for (const paneData of saved.panes.slice(0, 12)) {
    if (!paneData || typeof paneData !== 'object') continue
    const paneId = nextPaneId++
    const oldPaneId = String(paneData.id ?? paneId)
    if (paneIds.has(oldPaneId)) continue
    paneIds.set(oldPaneId, paneId)
    const pane = { id: paneId, tabIds: [], activeTabId: null, detail: null }
    for (const [index, tabData] of (Array.isArray(paneData.tabs) ? paneData.tabs : []).entries()) {
      const tab = restoreTab(tabData, nextTabId)
      if (!tab) continue
      const tabId = nextTabId++
      tab.id = tabId
      restoredTabs[tabId] = tab
      pane.tabIds.push(tabId)
      const oldTabId = String(tabData?.id ?? index)
      if (!tabIds.has(oldTabId)) tabIds.set(oldTabId, tabId)
    }
    pane.activeTabId = tabIds.get(String(paneData.activeTabId))
      ?? pane.tabIds.at(-1)
      ?? null
    restoredPanes.push(pane)
  }
  if (!restoredPanes.length) return false

  for (let index = 0; index < restoredPanes.length; index += 1) {
    const paneData = saved.panes[index]
    const detailTabId = tabIds.get(String(paneData?.detailTabId))
    if (detailTabId && isThreadTab(restoredTabs[detailTabId])) restoredPanes[index].detail = { tabId: detailTabId }
  }

  panes.items = restoredPanes
  panes.tabs = restoredTabs
  panes.nextPaneId = nextPaneId
  panes.nextTabId = nextTabId
  const mappedLayout = (Array.isArray(saved.layout) ? saved.layout : [])
    .map((column) => (Array.isArray(column) ? column : [column])
      .map((id) => paneIds.get(String(id)))
      .filter(Boolean))
    .filter((column) => column.length)
  panes.layout = normalizeLayout(mappedLayout)
  panes.activePaneId = paneIds.get(String(saved.activePaneId)) ?? restoredPanes[0].id
  panes.dragTabId = null
  return true
}

function restoreTab(saved, fallbackID) {
  if (!saved || typeof saved !== 'object') return null
  if (saved.kind === 'file') {
    const requestedPath = typeof saved.requestedPath === 'string' && saved.requestedPath.trim()
      ? saved.requestedPath.trim()
      : typeof saved.path === 'string' ? saved.path.trim() : ''
    if (!requestedPath) return null
    const context = {
      cwd: typeof saved.cwd === 'string' ? saved.cwd : '',
      catalogThreadId: typeof saved.catalogThreadId === 'string' ? saved.catalogThreadId : '',
      sessionId: typeof saved.sessionId === 'string' ? saved.sessionId : '',
    }
    return {
      id: fallbackID,
      kind: 'file',
      requestedPath,
      requestKey: fileRequestKey(requestedPath, context),
      path: typeof saved.path === 'string' ? saved.path : '',
      title: typeof saved.title === 'string' && saved.title ? saved.title : fileTitle(requestedPath),
      basename: typeof saved.basename === 'string' && saved.basename ? saved.basename : fileTitle(requestedPath),
      ...context,
      line: Number(saved.line || 0),
      navigationNonce: Date.now(),
      fileDirty: false,
      fileEditable: false,
    }
  }
  if (saved.kind && saved.kind !== 'thread') return null
  const tab = { id: fallbackID, kind: 'thread', turnActive: false, detached: Boolean(saved.detached) }
  for (const key of THREAD_TAB_KEYS) tab[key] = typeof saved[key] === 'string' ? saved[key] : ''
  if (!tab.nativeId && !tab.catalogId && !tab.sessionId) return null
  tab.title = fallbackTitle(tab)
  tab.tailResetNonce = Date.now()
  tab.forceTailResetNonce = 0
  return tab
}

function restoreLegacyActiveThread(saved) {
  if (!saved.thread || typeof saved.thread !== 'object') {
    localStorage.removeItem(ACTIVE_THREAD_VIEW_KEY)
    return false
  }
  const target = {}
  for (const key of THREAD_TAB_KEYS) target[key] = typeof saved.thread[key] === 'string' ? saved.thread[key] : ''
  target.turnActive = false
  target.detached = Boolean(saved.thread.detached)
  if (!target.nativeId && !target.catalogId && !target.sessionId) {
    localStorage.removeItem(ACTIVE_THREAD_VIEW_KEY)
    return false
  }
  return openThreadTab(target) != null
}

export function isActiveSession(sessionId) {
  return activeTab()?.sessionId === sessionId
}

export function isActiveCatalogThread(catalogId) {
  return activeTab()?.catalogId === catalogId
}

// adoptSessionTabs is the inverse of detachSessionTabs: a tab whose thread is
// attached becomes live in place. A thread can go live without any staging
// call — Resume from the pane, another client attaching, a reload that finds
// the attachment already up — and an open tab that missed that call kept
// rendering read-only history with no composer. Deriving liveness from the
// session list on every poll makes the transition a property of the thread
// rather than of whichever code path happened to open it.
export function adoptSessionTabs(sessions, onSessionRotated = null) {
  const rotated = new Map()
  for (const tab of Object.values(panes.tabs)) {
    if (!isThreadTab(tab)) continue
    const bound = tab.sessionId ? sessions.find((session) => session.id === tab.sessionId) : null
    if (bound) {
      // A live tab follows its session's identity. Normally that is a no-op,
      // but a claude `--resume` child mints a *new* native id and announces it
      // after the tab is already bound: without this the tab keeps naming the
      // predecessor, and a later resume from it would fork the conversation a
      // second time instead of continuing the one on screen.
      if (bound.threadId) tab.nativeId = bound.threadId
      if (bound.name) tab.title = bound.name
      continue
    }
    // No session answers to the tab's id — either the tab was never live, or a
    // nano restart minted fresh ids and this one is stale. Both resolve the
    // same way: the THREAD is the tab's identity, so if any session carries
    // its native id, adopt that session in place. This is what keeps a pane's
    // composer mounted across a deploy restart instead of flipping it to
    // read-only history for a poll cycle.
    if (!tab.nativeId) continue
    const match = sessions.find(
      (session) =>
        session.threadId === tab.nativeId && (!tab.provider || !session.provider || session.provider === tab.provider),
    )
    if (!match) continue
    const previousSessionId = tab.sessionId
    tab.sessionId = match.id
    tab.status = match.status || tab.status
    tab.detached = false
    if (previousSessionId && previousSessionId !== match.id) rotated.set(previousSessionId, match)
  }
  if (typeof onSessionRotated === 'function') {
    for (const [sessionId, successor] of rotated) {
      if (!Object.values(panes.tabs).some((tab) => tab.sessionId === sessionId)) {
        onSessionRotated(sessionId, successor)
      }
    }
  }
}

export function detachSessionTabs(sessionId) {
  for (const tab of Object.values(panes.tabs)) {
    if (!isThreadTab(tab)) continue
    if (tab.sessionId !== sessionId) continue
    tab.sessionId = ''
    tab.status = 'detached'
    tab.detached = true
  }
}

// The explicit-action counterpart to detachSessionTabs: an operator who
// deliberately detaches or closes a thread (the pane's Detach/End session
// button, or the quick-detail popup's Close session/Forget thread) wants the
// tab gone, not flipped to read-only history. Match by sessionId while the
// thread is still live, or by (provider, nativeId) once it is already
// detached — detachSessionTabs clears sessionId on that transition, so a
// dismissed detached row has only its native identity left to match on.
export function closeThreadTabs({ sessionId = '', provider = '', nativeId = '' } = {}) {
  for (const tab of Object.values(panes.tabs)) {
    if (!isThreadTab(tab)) continue
    const matches = sessionId ? tab.sessionId === sessionId : provider && nativeId && tab.provider === provider && tab.nativeId === nativeId
    if (!matches) continue
    const pane = paneHoldingTab(tab.id)
    if (pane) closeTab(pane.id, tab.id)
  }
}

// Details is a workspace view, not a child of the transcript. With a single
// pane it gets a new right-hand pane; otherwise it temporarily sits above the
// nearest pane's current tabs, which remain intact underneath it.
export function openDetailForActiveTab(sourcePaneId) {
  const source = panes.items.find((pane) => pane.id === sourcePaneId)
  if (!source?.activeTabId || !panes.tabs[source.activeTabId]) return null
  if (!isThreadTab(panes.tabs[source.activeTabId])) return null

  let target = null
  if (panes.items.length === 1) {
    const id = addPane()
    target = panes.items.find((pane) => pane.id === id) ?? null
  } else {
    target = nearestPane(sourcePaneId)
  }
  if (!target) return null
  target.detail = { tabId: source.activeTabId }
  panes.activePaneId = target.id
  return target.id
}

export function closePaneDetail(paneId) {
  const pane = panes.items.find((candidate) => candidate.id === paneId)
  if (!pane) return
  const source = paneHoldingTab(pane.detail?.tabId)
  pane.detail = null
  requestTailReset(panes.tabs[pane.activeTabId])
  // A one-pane workspace creates an empty companion pane for Details. On a
  // phone only the focused pane is mounted, so closing that companion must
  // return to the source thread instead of leaving an empty workspace. A
  // reused pane keeps its own active tab and remains focused.
  if (!pane.activeTabId && source) panes.activePaneId = source.id
}

function nearestPane(sourcePaneId) {
  const layout = normalizeLayout()
  const position = new Map()
  for (let column = 0; column < layout.length; column += 1) {
    for (let row = 0; row < layout[column].length; row += 1) position.set(layout[column][row], { column, row })
  }
  const sourcePosition = position.get(sourcePaneId)
  if (!sourcePosition) return panes.items.find((pane) => pane.id !== sourcePaneId) ?? null
  return panes.items
    .filter((pane) => pane.id !== sourcePaneId)
    .sort((a, b) => {
      const ap = position.get(a.id)
      const bp = position.get(b.id)
      const ad = Math.abs((ap?.column ?? 0) - sourcePosition.column) + Math.abs((ap?.row ?? 0) - sourcePosition.row)
      const bd = Math.abs((bp?.column ?? 0) - sourcePosition.column) + Math.abs((bp?.row ?? 0) - sourcePosition.row)
      if (ad !== bd) return ad - bd
      // A same-distance pane to the right is the least surprising tie-break.
      const ar = ap?.column > sourcePosition.column ? 0 : 1
      const br = bp?.column > sourcePosition.column ? 0 : 1
      return ar - br || a.id - b.id
    })[0] ?? null
}

function clearDetailsForTab(tabId) {
  for (const pane of panes.items) {
    if (pane.detail?.tabId === tabId) pane.detail = null
  }
}

function clearPaneDimensions() {
  if (typeof document === 'undefined') return
  document.querySelectorAll('.thread-pane, .thread-pane-stack').forEach((element) => {
    element.style.flex = ''
    element.style.width = ''
    element.style.height = ''
  })
}

export function __resetPanesForTest() {
  panes.items = [{ id: 1, tabIds: [], activeTabId: null, detail: null }]
  panes.layout = [[1]]
  panes.tabs = {}
  panes.activePaneId = 1
  panes.nextPaneId = 2
  panes.nextTabId = 1
  panes.dragTabId = null
}
