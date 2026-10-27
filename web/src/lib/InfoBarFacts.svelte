<script>
  // The info bar's fact run, shared by the live pane (Conversation) and the
  // read-only historical view so the two bars cannot drift in markup or
  // rhythm. Each item is a field plus its trailing dot, kept together so a
  // wrap breaks between fields (a line ends on a dot, never starts with one)
  // and the container's one column-gap keeps every space uniform.

  let { fields = [] } = $props()
</script>

<div class="info-bar-facts">
  {#each fields as f, i (f.key)}
    <span class="info-bar-item" class:identity={f.key === 'thread'}
      ><span class="info-bar-field" class:path={f.key === 'cwd'} title={f.title ?? null}>{f.text}</span
      >{#if i < fields.length - 1}<span class="info-bar-sep" aria-hidden="true">·</span>{/if}</span
    >
  {/each}
</div>
