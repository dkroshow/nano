import { explorer } from '../../explorer.svelte.js'
import { readAPIError } from '../../api_error.js'

// Operator buckets: named sidebar groups in the order the operator sets.
// Every route answers with the whole state, which replaces explorer.buckets
// at once; other browsers get the same state from the `buckets` event.

async function bucketRequest(method, url, body) {
  const res = await fetch(url, {
    method,
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!res.ok) throw new Error((await readAPIError(res)).message)
  const state = await res.json()
  if (Array.isArray(state?.buckets)) explorer.buckets = state.buckets
  return explorer.buckets
}

export function createBucket(name) {
  return bucketRequest('POST', '/api/v1/sidebar/buckets', { name })
}

export function renameBucket(id, name) {
  return bucketRequest('PATCH', `/api/v1/sidebar/buckets/${encodeURIComponent(id)}`, { name })
}

export function deleteBucket(id) {
  return bucketRequest('DELETE', `/api/v1/sidebar/buckets/${encodeURIComponent(id)}`)
}

// moveBucket shifts one bucket a step up (-1) or down (+1).
export function moveBucket(id, step) {
  const ids = explorer.buckets.map((bucket) => bucket.id)
  const from = ids.indexOf(id)
  const to = from + step
  if (from < 0 || to < 0 || to >= ids.length) return Promise.resolve(explorer.buckets)
  ids.splice(to, 0, ...ids.splice(from, 1))
  return bucketRequest('PUT', '/api/v1/sidebar/buckets/order', { bucketIds: ids })
}

// setThreadBucket puts a thread in a bucket, or takes it out with ''.
export function setThreadBucket(threadId, bucketId) {
  return bucketRequest('PUT', `/api/v1/threads/${encodeURIComponent(threadId)}/bucket`, { bucketId: bucketId || null })
}

// bucketOfThread names the bucket holding a provider-native thread, or ''.
export function bucketOfThread(buckets, provider, nativeId) {
  if (!provider || !nativeId) return ''
  for (const bucket of buckets ?? []) {
    if (bucket.threads?.some((thread) => thread.provider === provider && thread.nativeId === nativeId)) return bucket.id
  }
  return ''
}
