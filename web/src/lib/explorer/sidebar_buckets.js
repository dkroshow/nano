import { explorer } from '../../explorer.svelte.js'
import { readAPIError } from '../../api_error.js'

// Operator buckets: named sidebar groups in the order the operator sets.
// The ordered `buckets` event is the only writer of explorer.buckets, on this
// browser as on every other: a slow response applied after a newer event
// would move buckets back. A route's own answer serves only that request.

async function bucketRequest(method, url, body) {
  const res = await fetch(url, {
    method,
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!res.ok) throw new Error((await readAPIError(res)).message)
  return (await res.json())?.buckets ?? []
}

// createBucket answers with the new bucket's id; a new bucket goes last.
export async function createBucket(name) {
  return (await bucketRequest('POST', '/api/v1/sidebar/buckets', { name })).at(-1)?.id ?? ''
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
  if (from < 0 || to < 0 || to >= ids.length) return Promise.resolve()
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
