<script>
  import { deleteBucket, renameBucket } from './sidebar_buckets.js'
  import { endDrag, startBucketDrag } from './sidebar_bucket_drag.svelte.js'

  // One operator bucket's heading in the compact rail. Dragging the heading
  // is the only thing that moves a bucket.
  let { bucket, count = 0 } = $props()
  let busy = $state(false)
  let error = $state('')

  async function run(action) {
    if (busy) return
    busy = true
    error = ''
    try {
      await action()
    } catch (err) {
      error = String(err?.message ?? err)
    } finally {
      busy = false
    }
  }

  function rename() {
    const name = window.prompt('Rename bucket', bucket.name)
    if (name === null || name.trim() === bucket.name) return
    void run(() => renameBucket(bucket.id, name))
  }

  function remove() {
    if (!window.confirm(`Delete the bucket "${bucket.name}"? Its threads go back to the ungrouped list.`)) return
    void run(() => deleteBucket(bucket.id))
  }
</script>

<div
  class="sidebar-bucket"
  data-testid="sidebar-bucket"
  data-bucket-id={bucket.id}
  title={error || 'Drag to move this bucket'}
  role="heading"
  aria-level="3"
  draggable="true"
  ondragstart={(event) => startBucketDrag(event, bucket.id)}
  ondragend={endDrag}
>
  <span class="sidebar-bucket-grip" aria-hidden="true">⠿</span>
  <span class="sidebar-bucket-name">{bucket.name}</span>
  <span class="sidebar-bucket-count">{count}</span>
  <span class="sidebar-bucket-actions">
    <button type="button" aria-label="Rename {bucket.name}" title="Rename" disabled={busy} onclick={rename}>✎</button>
    <button type="button" aria-label="Delete {bucket.name}" title="Delete bucket" disabled={busy} onclick={remove}>×</button>
  </span>
</div>
{#if error}<div class="ifile-error" role="alert">{error}</div>{/if}

<style>
  .sidebar-bucket {
    display: flex;
    align-items: center;
    gap: 6px;
    min-height: 28px;
    padding: 4px 6px 2px 4px;
    color: var(--text3);
    font-family: var(--mono);
    font-size: 11px;
    letter-spacing: 0.04em;
    text-transform: uppercase;
    cursor: grab;
  }
  .sidebar-bucket-grip { font-size: 14px; opacity: 0.6; }
  .sidebar-bucket-name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .sidebar-bucket-count { opacity: 0.7; }
  .sidebar-bucket-actions {
    margin-left: auto;
    display: flex;
    gap: 2px;
    opacity: 0;
  }
  .sidebar-bucket:hover .sidebar-bucket-actions,
  .sidebar-bucket:focus-within .sidebar-bucket-actions {
    opacity: 1;
  }
  @media (hover: none), (pointer: coarse) {
    .sidebar-bucket-actions { opacity: 1; }
  }
  .sidebar-bucket-actions button {
    width: 26px;
    height: 26px;
    border: 0;
    border-radius: 4px;
    background: none;
    color: inherit;
    cursor: pointer;
    padding: 0;
    font-size: 16px;
    line-height: 1;
  }
  .sidebar-bucket-actions button:hover { color: var(--text); background: var(--surface); }
  .sidebar-bucket-actions button:disabled {
    opacity: 0.35;
    cursor: default;
  }
</style>
