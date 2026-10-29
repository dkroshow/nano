<script>
  import { metaRenderModel } from './explorer_format.js'

  // Renders one canonical metadata field with its three contract-distinct
  // states: known (value + evidence), unknown (concept exists, no evidence),
  // unsupported (provider has no such concept). Never a UI default.
  let { field, provider } = $props()
  const model = $derived(metaRenderModel(field, provider))
</script>

{#if model.mode === 'known'}
  <span class="mv-val">{model.value}</span>
  {#if model.evidence}<span class="mv-evidence">{model.evidence}</span>{/if}
{:else if model.mode === 'unsupported'}
  <span class="mv-na">n/a — not a {model.provider} concept</span>
{:else}
  <span class="mv-unknown">unknown</span>
{/if}
