<script>
  import { deleteBucket, moveBucket, renameBucket } from './sidebar_buckets.js'

  // One operator bucket's heading in the compact rail. The bucket's place
  // changes only through these arrows; nothing else reorders buckets.
  let { bucket, first = false, last = false, count = 0 } = $props()
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

<div class="sidebar-bucket" data-testid="sidebar-bucket" data-bucket-id={bucket.id} title={error || null}>
  <span class="sidebar-bucket-name">{bucket.name}</span>
  <span class="sidebar-bucket-count">{count}</span>
  <span class="sidebar-bucket-actions">
    <button type="button" aria-label="Move {bucket.name} up" title="Move up" disabled={busy || first} onclick={() => void run(() => moveBucket(bucket.id, -1))}>↑</button>
    <button type="button" aria-label="Move {bucket.name} down" title="Move down" disabled={busy || last} onclick={() => void run(() => moveBucket(bucket.id, 1))}>↓</button>
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
    padding: 6px 8px 2px;
    color: var(--text3);
    font-family: var(--mono);
    font-size: 10.5px;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }
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
  @media (hover: none) {
    .sidebar-bucket-actions { opacity: 0.8; }
  }
  .sidebar-bucket-actions button {
    border: 0;
    background: none;
    color: inherit;
    cursor: pointer;
    padding: 0 3px;
    font-size: 12px;
  }
  .sidebar-bucket-actions button:disabled {
    opacity: 0.35;
    cursor: default;
  }
</style>
