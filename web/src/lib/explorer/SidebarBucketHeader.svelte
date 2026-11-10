<script>
  import { deleteBucket, renameBucket } from './sidebar_buckets.js'
  import { endDrag, startBucketDrag } from './sidebar_bucket_drag.svelte.js'
  import { bucketCollapse, toggleBucketCollapsed } from './bucket_collapse.svelte.js'

  // One operator bucket's heading in the compact rail. Dragging the heading
  // is the only thing that moves a bucket. The caret collapses the bucket;
  // collapsed, the heading carries the liveliest state among its threads.
  let { bucket, count = 0, activity = '' } = $props()
  const collapsed = $derived(bucketCollapse.collapsed.includes(bucket.id))
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
  <button
    type="button"
    class="sidebar-bucket-toggle"
    aria-expanded={!collapsed}
    aria-label="{collapsed ? 'Expand' : 'Collapse'} {bucket.name}"
    title={collapsed ? 'Expand bucket' : 'Collapse bucket'}
    onclick={() => toggleBucketCollapsed(bucket.id)}
  >
    <span class="sidebar-bucket-caret" aria-hidden="true">{collapsed ? '▸' : '▾'}</span>
    <span class="sidebar-bucket-name">{bucket.name}</span>
  </button>
  <span class="sidebar-bucket-count">{count}</span>
  {#if collapsed && activity}<span class="t-dot {activity}" data-testid="bucket-state" title={activity === 'working' ? 'A thread here is working' : 'A thread here needs you'}></span>{/if}
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
  .sidebar-bucket-toggle {
    display: flex;
    align-items: center;
    gap: 4px;
    min-width: 0;
    min-height: 26px;
    padding: 0 2px;
    border: 0;
    background: none;
    color: inherit;
    font: inherit;
    letter-spacing: inherit;
    text-transform: inherit;
    cursor: pointer;
  }
  .sidebar-bucket-toggle:hover { color: var(--text); }
  .sidebar-bucket-caret { font-size: 14px; }
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
