import { placeBucket, setThreadBucket } from './sidebar_buckets.js'

// Drag and drop in the compact rail, with the browser's own drag events.
// A root thread row drags into a bucket, or onto the "remove" strip to leave
// its bucket; a bucket header drags before or after another bucket. Rows
// inside a bucket keep the ordinary order, so a drop never sets a position
// among threads. The drag's subject lives here because dragover events
// cannot read the drag's data.
export const bucketDrag = $state({
  thread: null, // { ref, bucketId } while a thread row is dragged
  bucket: null, // bucket id while a bucket header is dragged
  over: null, // { bucketId, after } | { remove: true } — where a drop would land
  error: '',
})

export function startThreadDrag(event, ref, bucketId) {
  bucketDrag.thread = { ref, bucketId: bucketId ?? '' }
  bucketDrag.bucket = null
  bucketDrag.error = ''
  event.dataTransfer?.setData('text/plain', ref)
  if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move'
}

export function startBucketDrag(event, bucketId) {
  bucketDrag.bucket = bucketId
  bucketDrag.thread = null
  bucketDrag.error = ''
  event.dataTransfer?.setData('text/plain', bucketId)
  if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move'
}

export function endDrag() {
  bucketDrag.thread = null
  bucketDrag.bucket = null
  bucketDrag.over = null
}

// overBucket marks a bucket group as the drop target. A dragged bucket lands
// before or after it by which half of the group the pointer is over.
export function overBucket(event, bucketId) {
  if (bucketDrag.thread) {
    if (bucketDrag.thread.bucketId === bucketId) return
  } else if (!bucketDrag.bucket || bucketDrag.bucket === bucketId) {
    return
  }
  event.preventDefault()
  const box = event.currentTarget.getBoundingClientRect()
  const after = bucketDrag.bucket ? event.clientY > box.top + box.height / 2 : false
  if (bucketDrag.over?.bucketId !== bucketId || bucketDrag.over?.after !== after) bucketDrag.over = { bucketId, after }
}

export function overRemove(event) {
  if (!bucketDrag.thread?.bucketId) return
  event.preventDefault()
  if (!bucketDrag.over?.remove) bucketDrag.over = { remove: true }
}

// leaveTargets runs as a dragover reaches the list: a target below it took
// the event, or the pointer is over no target and nothing is marked.
export function leaveTargets(event) {
  if (!event.defaultPrevented && bucketDrag.over) bucketDrag.over = null
}

export async function drop(event) {
  const { thread, bucket, over } = bucketDrag
  endDrag()
  if (!over) return
  event.preventDefault()
  try {
    if (thread) await setThreadBucket(thread.ref, over.remove ? '' : over.bucketId)
    else if (bucket && over.bucketId) await placeBucket(bucket, over.bucketId, over.after)
  } catch (error) {
    bucketDrag.error = String(error?.message ?? error)
  }
}
