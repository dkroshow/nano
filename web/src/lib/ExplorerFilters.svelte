<script>
  import { explorer, setFetchParam } from '../explorer.svelte.js'
  import { applyView } from '../explorer_local.js'

  // The window/facet controls, lifted out of the Explorer's own filter band so
  // the expanded rail keeps the compact rail's three-band header. They ride in
  // the Thread Explorer header row, beside the collapse affordance.
  const visibleCount = $derived(applyView(explorer.dataset, explorer.view).length)
  const programmaticHidden = $derived(
    !explorer.fetchParams.includeProgrammatic ? (explorer.index?.interactionCounts?.programmatic ?? 0) : 0
  )

  let threadIdDraft = $state(explorer.fetchParams.threadId)

  function submitThreadSearch(e) {
    e.preventDefault()
    const threadId = threadIdDraft.trim()
    if (threadId !== explorer.fetchParams.threadId) setFetchParam({ threadId })
  }

  function updateThreadSearch(e) {
    threadIdDraft = e.currentTarget.value
    // The native clear affordance on type=search should restore the full list
    // immediately; non-empty values remain explicit Enter/Search submissions.
    if (!threadIdDraft && explorer.fetchParams.threadId) setFetchParam({ threadId: '' })
  }
</script>

<div class="xfilters">
  <form class="xsearch" role="search" onsubmit={submitThreadSearch}>
    <input
      type="search"
      aria-label="Search by exact thread ID"
      placeholder="exact thread ID"
      autocomplete="off"
      spellcheck="false"
      value={threadIdDraft}
      oninput={updateThreadSearch}
    />
    <button type="submit" disabled={threadIdDraft.trim() === explorer.fetchParams.threadId}>Search</button>
  </form>
  <select
    aria-label="Thread window"
    value={explorer.fetchParams.window}
    onchange={(e) => setFetchParam({ window: e.currentTarget.value })}
  >
    <option value="thirty">last 30 days</option>
    <option value="ninety">last 90 days</option>
    <option value="all">all threads (slower)</option>
  </select>
  <select
    aria-label="Archived filter"
    value={explorer.fetchParams.archived}
    onchange={(e) => setFetchParam({ archived: e.currentTarget.value })}
  >
    <option value="include">archived: include</option>
    <option value="exclude">archived: exclude</option>
    <option value="only">archived: only</option>
  </select>
  <label class="nw-inline-check">
    <input
      type="checkbox"
      checked={explorer.fetchParams.includeSubagents}
      onchange={(e) => setFetchParam({ includeSubagents: e.currentTarget.checked })}
    />
    subagents
  </label>
  <label class="nw-inline-check">
    <input
      type="checkbox"
      checked={explorer.fetchParams.includeProgrammatic}
      onchange={(e) => setFetchParam({ includeProgrammatic: e.currentTarget.checked })}
    />
    programmatic
  </label>
  <span class="xhidden-note">
    {visibleCount} of {explorer.dataset.length} in window{explorer.datasetComplete
      ? ''
      : ' (loading…)'}{#if programmaticHidden > 0}{' · '}{programmaticHidden} programmatic hidden{/if}
  </span>
</div>
