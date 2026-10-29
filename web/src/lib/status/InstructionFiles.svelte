<script>
  import { readAPIError } from '../../api_error.js'

  // Instruction-source rows for the Responses Input panel. Each row is a click
  // trigger that opens the file in a right-pane tab (FileView). Backed by the
  // policy-wrapped /instruction-files API: every listed file is viewable;
  // repo-local ones are editable. Content is the on-disk source, not the
  // assembled prompt.
  let { session, sourceKinds = null, sourceScopes = null } = $props()

  let files = $state(null) // null while loading
  let listError = $state('')

  const api = $derived(`/api/sessions/${session.id}/instruction-files`)
  const visibleFiles = $derived(
    (files ?? []).filter(
      (f) =>
        (!sourceKinds || sourceKinds.includes(f.sourceKind)) &&
        (!sourceScopes || sourceScopes.includes(f.sourceScope))
    )
  )

  $effect(() => {
    session.id // re-run when the component is reused for another session
    files = null
    listError = ''
    loadList()
  })

  async function loadList() {
    try {
      const res = await fetch(api)
      if (!res.ok) throw new Error((await readAPIError(res, `Instruction file list failed (${res.status}).`)).message)
      files = (await res.json()).files ?? []
    } catch (err) {
      listError = String(err.message ?? err)
      files = []
    }
  }

  function evidenceLabel(f) {
    if (f.evidence === 'wire-authoritative') return 'wire'
    if (f.evidence === 'documented-expected') return 'expected'
    return 'present'
  }

  function evidenceTitle(f) {
    if (f.evidence === 'wire-authoritative') return 'Reported by the agent protocol for this thread'
    if (f.evidence === 'documented-expected') return 'Expected from documented Claude Code instruction-loading rules; not wire-attested'
    return 'Present on disk; loading not proven'
  }
</script>

{#if files == null}
  <div class="muted">Loading instruction files…</div>
{:else}
  {#each visibleFiles as f (f.path)}
    <button
      class="ifile-row"
      type="button"
      disabled={!f.exists}
      onclick={() => session.openInstructionFile(f)}
    >
      <span class="ifile-path">{f.path}</span>
      {#if f.sourceScope}<span class="ifile-badge present-only">{f.sourceScope}</span>{/if}
      {#if f.sourceLoad === 'conditional'}<span class="ifile-badge present-only">conditional</span>{/if}
      <span class="ifile-badge evidence {f.evidence ?? 'present-only'}" title={evidenceTitle(f)}>{evidenceLabel(f)}</span>
      {#if !f.exists}
        <span class="ifile-badge missing">missing</span>
      {:else if f.editable}
        <span class="ifile-badge editable">editable</span>
      {:else}
        <span class="ifile-badge readonly" title={f.readOnlyReason}>view-only</span>
      {/if}
    </button>
  {:else}
    <div class="muted">No instruction files.</div>
  {/each}
  {#if listError}<div class="ifile-error">{listError}</div>{/if}
{/if}
