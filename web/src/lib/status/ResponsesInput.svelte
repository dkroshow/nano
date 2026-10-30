<script>
  import InstructionFiles from './InstructionFiles.svelte'

  let { session } = $props()
  const store = $derived(session.store)

  // Mirrors the Codex Nano demo's "Responses Input" view: role selector, then
  // Model Instructions → Tool Registry, Permissions & Runtime, Available
  // Skills, Thread History. Roles beyond `user` light up once we render the
  // raw Responses-API input (research thread); the selector is the demo shape.
  let role = $state('user')

  const thread = $derived(store.meta?.thread ?? null)
  const skills = $derived(store.skills?.data ?? [])
  // The tool registry is fetched on first expand, not with /state: the
  // app-server call behind it costs seconds (see mcp_inventory.go).
  const mcpServers = $derived(store.mcp?.servers ?? [])
  const toolCount = $derived(mcpServers.reduce((n, s) => n + s.tools.length, 0))
  const toolRegistryNote = $derived(
    store.mcp
      ? `${mcpServers.length} MCP servers · ${toolCount} tools`
      : store.mcpLoading
        ? 'loading…'
        : store.mcpError
          ? 'unavailable'
          : 'expand to load'
  )
  const sandbox = $derived(thread?.sandbox ?? null)

  // Claude provider: everything comes from the system.init snapshot.
  const init = $derived(store.meta?.init ?? null)
  const claudeContext = $derived(store.meta?.claudeContext ?? { sources: [], configSources: [], docs: [], notes: [] })
  // Instruction files the server found on disk. Claude rows use documented
  // loading rules; Codex rows are wire-authoritative instructionSources.
  const instructionFiles = $derived(store.meta?.instructionFiles ?? [])
  const claudeStartupSources = $derived((claudeContext.sources ?? []).filter((s) => s.load !== 'conditional'))
  const claudeConditionalSources = $derived((claudeContext.sources ?? []).filter((s) => s.load === 'conditional'))
  const claudeConfigSources = $derived(claudeContext.configSources ?? [])
  const claudeGlobalSources = $derived(
    (claudeContext.sources ?? []).filter((s) => ['managed', 'user'].includes(s.scope) && s.kind !== 'auto-memory')
  )
  const claudeProjectSources = $derived(
    (claudeContext.sources ?? []).filter((s) => ['project', 'local'].includes(s.scope) && s.kind !== 'auto-memory')
  )
  const claudeGlobalRules = $derived(claudeGlobalSources.filter((s) => s.kind === 'claude-rule'))
  const claudeProjectRules = $derived(claudeProjectSources.filter((s) => s.kind === 'claude-rule'))
  const claudeProjectConditionalSources = $derived(claudeProjectSources.filter((s) => s.load === 'conditional'))
  const claudeMemorySources = $derived((claudeContext.sources ?? []).filter((s) => s.kind === 'auto-memory'))
  const claudeToolCount = $derived((init?.tools ?? []).length)
  const claudeMcpCount = $derived((init?.mcp_servers ?? []).length)

  // Grok provider: ACP initialize/session snapshots carry the native model
  // state, while turnOptions preserves the requested effort and resolved
  // thread model in Nano's provider-generic shape.
  const grokModelState = $derived(
    store.meta?.grokSession?.models ??
      store.meta?.grokSession?._meta?.modelState ??
      store.meta?.grokInitialize?._meta?.modelState ??
      null
  )
  const grokCurrentModel = $derived(
    grokModelState?.currentModelId ?? store.meta?.turnOptions?.threadModel ?? 'inherit'
  )
  const grokSessionOptions = $derived(
    store.meta?.grokSession?._meta?.['x.ai/sessionConfig']?.options ?? []
  )
  const grokEffortMetadata = $derived(store.hostedMetadata?.fields?.effort ?? null)
  const grokRequestedEffort = $derived(
    grokEffortMetadata?.desiredValue || store.meta?.turnOptions?.effort || ''
  )
  const grokEffectiveEffort = $derived(
    grokEffortMetadata?.effectiveValue ||
      grokSessionOptions.find((option) => option.category === 'mode' && option.selected)?.id ||
      ''
  )
  const grokEffortConflict = $derived(
    Boolean(grokRequestedEffort && grokEffectiveEffort && grokRequestedEffort !== grokEffectiveEffort)
  )

  function sourceKindLabel(kind) {
    return {
      'managed-claude-md': 'managed',
      'claude-md': 'CLAUDE.md',
      'claude-rule': 'rule',
      'auto-memory': 'memory',
      settings: 'settings',
      mcp: 'MCP',
    }[kind] ?? kind
  }

  function memoryEntrypoint(path) {
    if (!path) return 'default project memory'
    return path.endsWith('/MEMORY.md') ? path : `${path}/MEMORY.md`
  }
</script>

{#if store.provider === 'claude'}
  <div class="rinput">
    <details class="sect" open>
      <summary>System Prompt &amp; Runtime <span class="n">{init ? 'wire snapshot' : 'waiting for wire'}</span></summary>
      <div class="sect-body">
        {#if !init}
          <div class="muted">Waiting for system.init…</div>
        {:else}
          <dl class="kv">
            <dt>model</dt><dd>{init.model}</dd>
            <dt>effort</dt><dd>{init.effort ?? init.effort_level ?? '—'}</dd>
            <dt>output style</dt><dd>{init.output_style ?? 'default'}</dd>
            <dt>permission mode</dt><dd>{init.permissionMode ?? 'default'}</dd>
            <dt>auth</dt><dd>{init.apiKeySource ?? '—'}</dd>
            <dt>cwd</dt><dd>{init.cwd ?? store.meta?.cwd}</dd>
          </dl>
        {/if}
      </div>
    </details>

    <details class="sect" open>
      <summary>Global Instructions <span class="n">{claudeGlobalSources.length} files · {claudeGlobalRules.length} rules</span></summary>
      <div class="sect-body">
        <dl class="kv">
          <dt>load order</dt><dd>managed policy → user CLAUDE.md → user rules</dd>
          <dt>scope</dt><dd>applies across projects before repository-specific context</dd>
        </dl>
        {#if claudeGlobalSources.length}
          <InstructionFiles
            {session}
            sourceKinds={['managed-claude-md', 'claude-md', 'claude-rule']}
            sourceScopes={['managed', 'user']}
          />
        {:else}
          <div class="muted">No managed or user-level CLAUDE.md/rules files found on disk.</div>
        {/if}
      </div>
    </details>

    <details class="sect" open>
      <summary>Project Instructions <span class="n">{claudeProjectSources.length} files · {claudeProjectRules.length} rules</span></summary>
      <div class="sect-body">
        <dl class="kv">
          <dt>CLAUDE.md mode</dt><dd>context after global instructions <span class="n">not system prompt</span></dd>
          <dt>load order</dt><dd>root → cwd, with CLAUDE.local.md after CLAUDE.md</dd>
          <dt>rule loading</dt><dd>{claudeProjectRules.length} project rules <span class="n">{claudeProjectConditionalSources.length} conditional</span></dd>
        </dl>
        {#if claudeProjectSources.length}
          <InstructionFiles
            {session}
            sourceKinds={['claude-md', 'claude-rule']}
            sourceScopes={['project', 'local']}
          />
        {:else}
          <div class="muted">No project CLAUDE.md, CLAUDE.local.md, or .claude/rules files found for this cwd.</div>
        {/if}
      </div>
    </details>

    <details class="sect" open>
      <summary>Auto Memory <span class="n">{claudeMemorySources.length ? 'MEMORY.md' : 'none found'}</span></summary>
      <div class="sect-body">
        <dl class="kv">
          <dt>entrypoint</dt><dd>{memoryEntrypoint(init?.memory_paths?.auto)}</dd>
          <dt>startup limit</dt><dd>first 200 lines or 25KB</dd>
        </dl>
        {#if claudeMemorySources.length}
          <InstructionFiles {session} sourceKinds={['auto-memory']} />
        {:else}
          <div class="muted">No auto-memory entrypoint found on disk for this repository.</div>
        {/if}
      </div>
    </details>

    <details class="sect" open>
      <summary>Tools &amp; MCP <span class="n">{claudeToolCount} tools · {claudeMcpCount} MCP servers</span></summary>
      <div class="sect-body">
        {#if !init}
          <div class="muted">Waiting for system.init…</div>
        {:else}
          <div class="list-row"><div class="desc">{(init.tools ?? []).join(', ') || 'none'}</div></div>
          {#each init.mcp_servers ?? [] as s}
            <div class="list-row"><div class="name">{s.name} <span class="n">{s.status}</span></div></div>
          {/each}
        {/if}
        {#if claudeConfigSources.length}
          <div class="context-subhead">Configuration sources present on disk</div>
          {#each claudeConfigSources as source (source.path)}
            <div class="context-row" title={source.detail}>
              <span class="context-path">{source.path}</span>
              <span class="ifile-badge present-only">{sourceKindLabel(source.kind)}</span>
              <span class="ifile-badge">{source.scope}</span>
            </div>
          {/each}
        {/if}
      </div>
    </details>

    <details class="sect">
      <summary>Skills &amp; Commands <span class="n">{(init?.slash_commands ?? []).length}</span></summary>
      <div class="sect-body">
        {#if !init}
          <div class="muted">Waiting for system.init…</div>
        {:else}
          <div class="list-row"><div class="desc">{(init.slash_commands ?? []).join(', ') || 'none reported'}</div></div>
          <div class="muted">Skills load lazily when invoked or relevant; command inventory comes from system.init/native attachments when reported.</div>
        {/if}
      </div>
    </details>

    <details class="sect">
      <summary>Evidence Boundary <span class="n">wire + documented rules</span></summary>
      <div class="sect-body">
        {#if store.meta?.protectedFolder}<div class="context-note">{store.meta.protectedFolder}</div>{/if}
        <dl class="kv">
          <dt>wire snapshot</dt><dd>system.init {#if init}<span class="ok">observed</span>{:else}<span class="warn">waiting</span>{/if}</dd>
          <dt>assembled prompt</dt><dd>not exposed <span class="n">no Claude Code wire/transcript field</span></dd>
          <dt>context files</dt><dd>{claudeStartupSources.length} startup · {claudeConditionalSources.length} conditional</dd>
        </dl>
        {#each claudeContext.notes ?? [] as note}
          <div class="context-note">{note}</div>
        {/each}
        {#if instructionFiles.length}
          <div class="context-note">Openable source rows appear in Global Instructions, Project Instructions, and Auto Memory.</div>
        {/if}
      </div>
    </details>

    <details class="sect">
      <summary>Disk History <span class="n">native transcript</span></summary>
      <div class="sect-body">
        <dl class="kv">
          <dt>session id</dt><dd>{store.meta?.threadId || '—'}</dd>
          <dt>transcript</dt><dd>{store.meta?.transcriptPath ?? '—'}</dd>
        </dl>
        {#if (claudeContext.docs ?? []).length}
          <div class="context-subhead">Grounding docs</div>
          {#each claudeContext.docs as doc}
            <div class="context-row">
              <a class="context-link" href={doc.url} target="_blank" rel="noreferrer">{doc.label}</a>
            </div>
          {/each}
        {/if}
      </div>
    </details>
  </div>
{:else if store.provider === 'grok'}
  <div class="rinput">
    <details class="sect" open>
      <summary>Runtime <span class="n">{store.meta ? 'wire snapshot' : 'waiting for wire'}</span></summary>
      <div class="sect-body">
        <dl class="kv">
          <dt>model</dt><dd>{grokCurrentModel}</dd>
          <dt>effort</dt><dd>effective {grokEffectiveEffort || 'unverified'}
            {#if grokRequestedEffort}
              <span class:n={!grokEffortConflict} class:warn={grokEffortConflict}>requested {grokRequestedEffort}{grokEffortConflict ? ' · conflict' : ''}</span>
            {/if}
          </dd>
          <dt>cwd</dt><dd>{store.meta?.cwd ?? '—'}</dd>
          <dt>leader</dt><dd>durable native leader <span class="n">Grok preserves active sessions while Nano restarts</span></dd>
          <dt>sandbox</dt><dd>off <span class="n">fixed for hosted sessions; permissions are requested by Grok itself, not chosen here</span></dd>
        </dl>
        <div class="muted">Model state comes from Grok's ACP handshake. Grok does not currently expose a Nano-compatible MCP inventory endpoint.</div>
      </div>
    </details>

    <details class="sect">
      <summary>Disk History <span class="n">native transcript</span></summary>
      <div class="sect-body">
        <dl class="kv">
          <dt>session id</dt><dd>{store.meta?.threadId || '—'}</dd>
        </dl>
      </div>
    </details>
  </div>
{:else}
<div class="rinput">
  <div class="role-row" role="radiogroup" aria-label="input role">
    {#each ['system', 'developer', 'user'] as r}
      <button class:on={role === r} onclick={() => (role = r)} aria-pressed={role === r}>{r}</button>
    {/each}
  </div>

  {#if !thread}
    <div class="muted">Waiting for thread…</div>
  {:else}
    <details class="sect" open>
      <summary>Model Instructions <span class="n">{thread.instructionSources?.length ?? 0} sources</span></summary>
      <div class="sect-body">
        {#if thread.instructionSources?.length}
          <InstructionFiles {session} />
        {:else}
          <div class="muted">No instruction files loaded.</div>
        {/if}

        <details
          class="sect"
          style="margin-top: 10px"
          ontoggle={(e) => e.currentTarget.open && session.loadMcpServers()}
        >
          <summary>Tool Registry <span class="n">{toolRegistryNote}</span></summary>
          <div class="sect-body">
            {#if store.mcpError && !store.mcp}
              <div class="muted">Tool registry unavailable: {store.mcpError}</div>
            {:else if !store.mcp}
              <div class="muted">Loading tool registry…</div>
            {:else}
              {#each mcpServers as s}
                <div class="list-row">
                  <div class="name">{s.name} <span class="n">{s.tools.length} tools</span></div>
                  {#if s.tools.length}
                    <div class="desc">{s.tools.join(', ')}</div>
                  {/if}
                </div>
              {:else}
                <div class="muted">No MCP servers configured.</div>
              {/each}
            {/if}
          </div>
        </details>
      </div>
    </details>

    <details class="sect" open>
      <summary>Permissions &amp; Runtime</summary>
      <div class="sect-body">
        <dl class="kv">
          <dt>model</dt><dd>{thread.model} <span class="n">({thread.reasoningEffort ?? 'default'} effort{thread.serviceTier ? `, ${thread.serviceTier}` : ''})</span></dd>
          <dt>sandbox</dt><dd>{sandbox?.type ?? String(sandbox)}
            {#if sandbox?.networkAccess === false}<span class="ok">network off</span>
            {:else if sandbox?.networkAccess}<span class="warn">network on</span>{/if}</dd>
          <dt>approvals</dt><dd>{JSON.stringify(thread.approvalPolicy)} <span class="n">reviewer: {thread.approvalsReviewer}</span></dd>
          <dt>cwd</dt><dd>{thread.cwd}</dd>
          <dt>workspace roots</dt><dd>{(thread.runtimeWorkspaceRoots ?? []).join(', ')}</dd>
          <dt>multi-agent</dt><dd>{thread.multiAgentMode ?? '—'}</dd>
        </dl>
      </div>
    </details>

    <details class="sect" ontoggle={(e) => e.currentTarget.open && session.loadSkills()}>
      <summary>Available Skills <span class="n">{store.skills ? skills.length : store.skillsError ? 'unavailable' : 'expand to load'}</span></summary>
      <div class="sect-body">
        {#if store.skillsError}
          <div class="muted">Skills unavailable: {store.skillsError}</div>
        {/if}
        {#each skills as s}
          <div class="list-row">
            <div class="name">{s.name ?? s.metadata?.name}</div>
            <div class="desc">{s.shortDescription ?? s.description ?? s.metadata?.description ?? ''}</div>
          </div>
        {:else}
          {#if store.skills}
            <div class="muted">No skills available.</div>
          {:else if !store.skillsError}
            <div class="muted">loading…</div>
          {/if}
        {/each}
      </div>
    </details>

    <details class="sect">
      <summary>Thread History</summary>
      <div class="sect-body">
        <dl class="kv">
          <dt>thread id</dt><dd>{thread.thread?.id}</dd>
          <dt>rollout</dt><dd>{thread.thread?.path}</dd>
          <dt>created</dt><dd>{thread.thread?.createdAt ? new Date(thread.thread.createdAt * 1000).toLocaleString() : '—'}</dd>
          <dt>cli</dt><dd>{thread.thread?.cliVersion} <span class="n">source: {thread.thread?.source}</span></dd>
        </dl>
      </div>
    </details>
  {/if}
</div>
{/if}
