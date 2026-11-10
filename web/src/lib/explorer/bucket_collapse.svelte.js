// Which operator buckets are collapsed, in this browser. Like the agent-group
// disclosure, this is a view choice, so it lives in browser storage: a bucket
// collapsed on one device can stay open on another. Buckets start expanded.

const STORAGE_KEY = 'nano:sidebar-collapsed-buckets'

function storage() {
  try {
    return globalThis.localStorage ?? null
  } catch {
    return null
  }
}

function restore() {
  try {
    const saved = JSON.parse(storage()?.getItem(STORAGE_KEY) ?? '[]')
    return Array.isArray(saved) ? saved.filter((id) => typeof id === 'string') : []
  } catch {
    return []
  }
}

export const bucketCollapse = $state({ collapsed: restore() })

export function toggleBucketCollapsed(id) {
  const next = bucketCollapse.collapsed.includes(id)
    ? bucketCollapse.collapsed.filter((one) => one !== id)
    : [...bucketCollapse.collapsed, id]
  bucketCollapse.collapsed = next
  try {
    storage()?.setItem(STORAGE_KEY, JSON.stringify(next))
  } catch {
    // the choice still holds for this page
  }
}
