<script>
  let { depth = 0, path = [], last = false, parent = '', overlay = false } = $props()

  const ancestorLasts = $derived(
    Array.isArray(path) && path.length === Math.max(0, depth - 1)
      ? path
      : Array(Math.max(0, depth - 1)).fill(false),
  )
</script>

{#if depth > 0}
  <span
    class="tree-branch-lines"
    class:overlay
    title={parent ? `Child of ${parent}` : 'Child thread'}
    aria-hidden="true"
  >
    {#each ancestorLasts as ancestorLast}
      <span class="tree-branch-slot ancestor" class:continues={!ancestorLast}></span>
    {/each}
    <span class="tree-branch-slot fork" class:last></span>
  </span>
{/if}
